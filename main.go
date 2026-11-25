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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/AudDMusic/audd-go"
	"github.com/bwmarrin/discordgo"
	"github.com/getsentry/sentry-go"
)

const release = "discord-bot@0.1.0"

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
		log.Fatalln("Can't load the config:", err)
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
		log.Fatalln("Can't load the opt-out list:", err)
	}
	bot.voice.skipUser = bot.optOuts.Has
	if err := bot.openSessions(ctx); err != nil {
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
	for _, s := range bot.sessions {
		_ = s.Close()
	}
}

func (b *Bot) intents() discordgo.Intent {
	intents := discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages
	if b.cfg.VoiceEnabled {
		intents |= discordgo.IntentsGuildVoiceStates
	}
	if !b.cfg.DisableMessageContentIntent {
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
	if b.cfg.VoiceEnabled {
		s.AddHandler(b.onVoiceStateUpdate)
	}
	return s, nil
}

// openSessions connects to the gateway with as many shards as needed.
func (b *Bot) openSessions(ctx context.Context) error {
	shardCount := b.cfg.ShardCount
	if shardCount == 0 {
		probe, err := b.newSession(0, 1)
		if err != nil {
			return err
		}
		gateway, err := probe.GatewayBot()
		if err != nil {
			return fmt.Errorf("can't get the gateway information (is the Discord token valid?): %w", err)
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
		b.sessions = append(b.sessions, s)
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
	if len(b.sessions) == 0 {
		return nil
	}
	return b.sessions[0]
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
