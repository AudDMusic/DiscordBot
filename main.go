package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/AudDMusic/audd-go"
	"github.com/Mihonarium/discordgo"
	"github.com/getsentry/sentry-go"
	davesession "github.com/thomas-vilte/dave-go/session"
)

const release = "discord-bot@0.2.0"

// exitConfig is the exit status for the problems a restart can't fix (a broken config, a rejected
// token), so that a service manager can be told not to restart the bot in a loop.
const exitConfig = 78

// Application flags that say the bot may use the Message Content intent (approved, or under 100 servers).
const messageContentFlags = 1<<18 | 1<<19

var errBadToken = errors.New("Discord rejected the bot token")

func isUnauthorized(err error) bool {
	var restErr *discordgo.RESTError
	return errors.As(err, &restErr) && restErr.Response != nil && restErr.Response.StatusCode == http.StatusUnauthorized
}

// Bot holds everything the handlers share.
type Bot struct {
	cfg     *BotConfig
	audd    *audd.Client
	limiter *RateLimiter
	cache   *resultCache
	flights *flightGroup
	sem     chan struct{} // bounds the recognitions running at the same time
	voice   *voiceManager
	optOuts *optOutStore

	blockedUsers  map[string]bool
	blockedGuilds map[string]bool

	// contentIntent tells whether the bot receives the text of messages that don't mention it.
	contentIntent bool

	sessionsMu         sync.RWMutex
	sessions           []*discordgo.Session
	commandsRegistered atomic.Bool

	statsMu sync.RWMutex
	guilds  map[string]int // guild ID -> member count
}

func newBot(cfg *BotConfig) *Bot {
	b := &Bot{
		cfg:           cfg,
		audd:          audd.NewClient(cfg.AudDToken, audd.WithEnterpriseTimeout(recognitionTimeout)),
		limiter:       NewRateLimiter(),
		cache:         newResultCache(time.Duration(cfg.ResultCacheMinutes)*time.Minute, 5000),
		flights:       newFlightGroup(),
		sem:           make(chan struct{}, cfg.RateLimits.MaxConcurrent),
		voice:         newVoiceManager(cfg.MaxVoiceSessions, time.Duration(cfg.VoiceIdleMinutes)*time.Minute),
		blockedUsers:  map[string]bool{},
		blockedGuilds: map[string]bool{},
		guilds:        map[string]int{},
	}
	for _, id := range cfg.BlockedUserIDs {
		b.blockedUsers[id] = true
	}
	for _, id := range cfg.BlockedGuildIDs {
		b.blockedGuilds[id] = true
	}
	return b
}

func main() {
	configPath := flag.String("config", "config.json", "path to the config file")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Println("Can't load the config:", err)
		os.Exit(exitConfig)
	}
	if err := sentry.Init(sentry.ClientOptions{Dsn: cfg.SentryDSN, Release: release, AttachStacktrace: true}); err != nil {
		log.Println("Can't initialize Sentry:", err)
	}
	defer sentry.Flush(5 * time.Second)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bot := newBot(cfg)
	// Without the opt-out list the bot could process the messages of the people who opted out: don't start.
	if bot.optOuts, err = loadOptOuts(cfg.OptOutFile); err != nil {
		log.Println("Can't load the opt-out list:", err)
		os.Exit(exitConfig)
	}
	bot.voice.skipUser = bot.optOuts.Has
	if err := bot.openSessions(ctx); err != nil {
		if errors.Is(err, errBadToken) {
			log.Println("Discord rejected the bot token. Put a valid one into DiscordToken in the config " +
				"(Discord Developer Portal -> your application -> Bot -> Reset Token) and start the bot again.")
			os.Exit(exitConfig)
		}
		capture(err)
		sentry.Flush(5 * time.Second)
		log.Fatalln("Can't connect to Discord:", err)
	}
	callbacks := bot.startCallbackServer()
	go bot.janitor(ctx)

	<-ctx.Done()
	log.Println("Shutting down")
	bot.voice.leaveAll()
	if callbacks != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = callbacks.Shutdown(shutdownCtx)
		cancel()
	}
	time.Sleep(500 * time.Millisecond) // let the voice disconnects go out
	bot.sessionsMu.RLock()
	defer bot.sessionsMu.RUnlock()
	for _, s := range bot.sessions {
		_ = s.Close()
	}
}

func (b *Bot) intents() discordgo.Intent {
	intents := discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages
	if b.cfg.VoiceEnabled {
		intents |= discordgo.IntentsGuildVoiceStates
	}
	if b.contentIntent {
		intents |= discordgo.IntentsMessageContent
	}
	return intents
}

func (b *Bot) newSession(shardID, shardCount int) (*discordgo.Session, error) {
	s, err := discordgo.New("Bot " + b.cfg.DiscordToken)
	if err != nil {
		return nil, err
	}
	s.ShardID, s.ShardCount = shardID, shardCount
	s.Identify.Intents = b.intents()
	s.MaxRestRetries = 1
	s.Client = &http.Client{Timeout: 30 * time.Second}
	s.State.MaxMessageCount = 0
	s.State.TrackEmojis = false
	s.State.TrackPresences = false
	s.State.TrackThreads = false
	s.State.TrackThreadMembers = false
	s.AddHandler(b.onReady)
	s.AddHandler(b.onResumed)
	s.AddHandler(b.onMessageCreate)
	s.AddHandler(b.onGuildCreate)
	s.AddHandler(b.onGuildDelete)
	s.AddHandler(b.onInteractionCreate)
	if b.cfg.DebugGatewayEvents {
		s.AddHandler(b.onRawEvent)
	}
	if b.cfg.VoiceEnabled {
		s.AddHandler(b.onVoiceStateUpdate)
		// Discord's voice channels are end-to-end encrypted (DAVE); this is the session that does it.
		s.DAVESessionCreate = davesession.CreateFunc()
	}
	return s, nil
}

// openSessions connects to the gateway with as many shards as needed.
func (b *Bot) openSessions(ctx context.Context) error {
	probe, err := b.newSession(0, 1)
	if err != nil {
		return err
	}
	// Asking for an intent Discord hasn't granted makes it refuse the connection,
	// so check what the application is allowed to use first.
	app, err := probe.Application("@me")
	switch {
	case isUnauthorized(err):
		return errBadToken
	case err != nil:
		log.Println("Can't check which intents the application has, going by the config:", err)
		b.contentIntent = !b.cfg.DisableMessageContentIntent
	default:
		granted := app.Flags&messageContentFlags != 0
		b.contentIntent = granted && !b.cfg.DisableMessageContentIntent
		if !granted {
			log.Println("Discord hasn't granted this application the Message Content intent: replies with " +
				"!song won't work in servers until it does. Recognize This Song and the slash commands work. " +
				"Restart the bot once the intent is granted.")
		}
	}
	shardCount := b.cfg.ShardCount
	if shardCount == 0 {
		gateway, err := probe.GatewayBot()
		if isUnauthorized(err) {
			return errBadToken
		}
		if err != nil {
			return fmt.Errorf("can't get the gateway information: %w", err)
		}
		shardCount = max(gateway.Shards, 1)
	}
	for shardID := 0; shardID < shardCount; shardID++ {
		s, err := b.newSession(shardID, shardCount)
		if err != nil {
			return err
		}
		if err := s.Open(); err != nil {
			return fmt.Errorf("can't open shard %d: %w", shardID, err)
		}
		b.sessionsMu.Lock()
		b.sessions = append(b.sessions, s)
		b.sessionsMu.Unlock()
		log.Printf("Opened shard %d of %d", shardID+1, shardCount)
		if shardID < shardCount-1 {
			select { // Discord allows one new gateway session per 5 seconds
			case <-time.After(6 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

// rest returns a session to use for the requests that don't depend on the shard.
func (b *Bot) rest() *discordgo.Session {
	b.sessionsMu.RLock()
	defer b.sessionsMu.RUnlock()
	if len(b.sessions) == 0 {
		return nil
	}
	return b.sessions[0]
}

// sessionForGuild returns the session of the shard the server is on, or nil while
// that shard is still connecting. Direct messages always arrive on the first shard,
// so anything they say about a server may belong to another one.
func (b *Bot) sessionForGuild(guildID string) *discordgo.Session {
	id, err := strconv.ParseUint(guildID, 10, 64)
	if err != nil {
		return nil
	}
	b.sessionsMu.RLock()
	defer b.sessionsMu.RUnlock()
	if len(b.sessions) == 0 {
		return nil
	}
	shard := int((id >> 22) % uint64(max(b.sessions[0].ShardCount, 1)))
	if shard >= len(b.sessions) {
		return nil
	}
	return b.sessions[shard]
}

func (b *Bot) janitor(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.limiter.Prune()
			b.voice.cleanup()
		}
	}
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	defer recoverPanic("ready")
	capture(s.UpdateListeningStatus("!song"))
	if s.ShardID != 0 || !b.commandsRegistered.CompareAndSwap(false, true) {
		return
	}
	appID := b.cfg.DiscordAppID
	if appID == "" && r.Application != nil {
		appID = r.Application.ID
	}
	if appID == "" && r.User != nil {
		appID = r.User.ID
	}
	// A bulk overwrite is idempotent: Discord only changes the commands that differ.
	if _, err := s.ApplicationCommandBulkOverwrite(appID, "", b.applicationCommands()); err != nil {
		b.commandsRegistered.Store(false) // try again on the next connection
		capture(fmt.Errorf("can't register the commands: %w", err))
		return
	}
	log.Println("Registered the application commands")
}

func (b *Bot) onResumed(s *discordgo.Session, _ *discordgo.Resumed) {
	defer recoverPanic("resumed")
	capture(s.UpdateListeningStatus("!song"))
}

func (b *Bot) onGuildCreate(s *discordgo.Session, event *discordgo.GuildCreate) {
	defer recoverPanic("guildCreate")
	if event.Guild == nil || event.Guild.Unavailable {
		return
	}
	b.statsMu.Lock()
	b.guilds[event.Guild.ID] = event.Guild.MemberCount
	b.statsMu.Unlock()
	if event.Guild.JoinedAt.Before(time.Now().Add(-2*time.Minute)) || b.blockedGuilds[event.Guild.ID] {
		return
	}
	// The bot has just been added to the server: say hi.
	if !b.limiter.Allow("welcome:"+event.Guild.ID, 1, 24*time.Hour) {
		return
	}
	welcome := textMessage(b.helpText(), nil)
	for _, channel := range event.Guild.Channels {
		if channel.Type == discordgo.ChannelTypeGuildText && containsFold(channel.Name, "bot") {
			_, _ = s.ChannelMessageSendComplex(channel.ID, welcome)
			return
		}
	}
	if event.Guild.SystemChannelID != "" {
		_, _ = s.ChannelMessageSendComplex(event.Guild.SystemChannelID, welcome)
	}
}

func (b *Bot) onGuildDelete(_ *discordgo.Session, event *discordgo.GuildDelete) {
	defer recoverPanic("guildDelete")
	if event.Guild == nil || event.Guild.Unavailable { // unavailable means an outage, not that the bot was removed
		return
	}
	b.statsMu.Lock()
	delete(b.guilds, event.Guild.ID)
	b.statsMu.Unlock()
	b.voice.leave(event.Guild.ID, nil)
}

func (b *Bot) stats() (servers, members int) {
	b.statsMu.RLock()
	defer b.statsMu.RUnlock()
	for _, count := range b.guilds {
		members += count
	}
	return len(b.guilds), members
}

// sendResult posts a message to the channel and, if asked, publishes it in an announcement channel.
func (b *Bot) sendResult(channelID string, message *discordgo.MessageSend, publishAnnouncement bool) error {
	s := b.rest()
	if s == nil {
		return errors.New("not connected to Discord")
	}
	if message.AllowedMentions == nil {
		message.AllowedMentions = noMentions()
	}
	m, err := s.ChannelMessageSendComplex(channelID, message)
	if err != nil && message.Reference != nil {
		// The message we're replying to could have been deleted: send without the reference.
		message.Reference = nil
		m, err = s.ChannelMessageSendComplex(channelID, message)
	}
	if err != nil {
		return fmt.Errorf("can't send a message to %s: %w", channelID, err)
	}
	if publishAnnouncement {
		if _, err = s.ChannelMessageCrosspost(channelID, m.ID); err != nil {
			return fmt.Errorf("can't publish a message in %s: %w", channelID, err)
		}
	}
	return nil
}
