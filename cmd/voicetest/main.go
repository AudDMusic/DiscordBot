// voicetest joins a voice channel with the bot's account, prints how the
// end-to-end encryption handshake goes and whether received audio decodes.
// It is a development tool and is not part of the bot.
//
// Don't point it at a server where the running bot is in a voice channel: both use the
// same account, and each would keep taking the voice connection away from the other.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Mihonarium/discordgo"
	davesession "github.com/thomas-vilte/dave-go/session"
	"layeh.com/gopus"
)

func main() {
	configPath := flag.String("config", "config.json", "bot config with the token")
	guildID := flag.String("guild", "", "server to look at")
	channelID := flag.String("channel", "", "voice channel to join; without it the channels are only listed")
	seconds := flag.Int("seconds", 20, "how long to stay")
	shards := flag.Int("shards", 2, "the bot's shard count")
	daveDebug := flag.Bool("davedebug", false, "print the debug log of the encryption session")
	flag.Parse()

	raw, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfg struct{ DiscordToken string }
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Fatal(err)
	}
	gid, err := strconv.ParseUint(*guildID, 10, 64)
	if err != nil {
		log.Fatal("give -guild")
	}

	s, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		log.Fatal(err)
	}
	s.ShardCount, s.ShardID = *shards, int((gid>>22)%uint64(*shards))
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates
	s.LogLevel = discordgo.LogWarning

	var dave *davesession.Session
	var daveMu sync.Mutex
	s.DAVESessionCreate = davesession.CreateFunc(davesession.WithSessionHook(func(x *davesession.Session) {
		daveMu.Lock()
		dave = x
		daveMu.Unlock()
	}))
	daveLevel := slog.LevelInfo
	if *daveDebug {
		daveLevel = slog.LevelDebug
	}
	s.DAVELogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: daveLevel}))

	guildReady := make(chan *discordgo.Guild, 1)
	s.AddHandler(func(_ *discordgo.Session, g *discordgo.GuildCreate) {
		if g.ID == *guildID {
			select {
			case guildReady <- g.Guild:
			default:
			}
		}
	})
	if err := s.Open(); err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	var guild *discordgo.Guild
	select {
	case guild = <-guildReady:
	case <-time.After(20 * time.Second):
		log.Fatal("the server didn't show up; is the bot in it?")
	}
	members := map[string]int{}
	for _, vs := range guild.VoiceStates {
		members[vs.ChannelID]++
	}
	fmt.Printf("server %q: voice channels\n", guild.Name)
	sort.Slice(guild.Channels, func(i, j int) bool { return guild.Channels[i].Position < guild.Channels[j].Position })
	for _, c := range guild.Channels {
		if c.Type == discordgo.ChannelTypeGuildVoice || c.Type == discordgo.ChannelTypeGuildStageVoice {
			fmt.Printf("  %s  type=%d  people=%d  %q\n", c.ID, c.Type, members[c.ID], c.Name)
		}
	}
	if *channelID == "" {
		return
	}

	s.LogLevel = discordgo.LogInformational // the voice connection copies this
	vc, err := s.ChannelVoiceJoin(*guildID, *channelID, true, false)
	if err != nil {
		if vc != nil {
			_ = vc.Disconnect()
		}
		log.Fatalln("join failed:", err)
	}
	fmt.Println("joined; voice connection ready")

	type stream struct {
		packets, decoded, failed int
		decoder                  *gopus.Decoder
	}
	var mu sync.Mutex
	streams := map[uint32]*stream{}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case p := <-vc.OpusRecv:
				if p == nil {
					continue
				}
				mu.Lock()
				st := streams[p.SSRC]
				if st == nil {
					d, _ := gopus.NewDecoder(48000, 2)
					st = &stream{decoder: d}
					streams[p.SSRC] = st
				}
				st.packets++
				if _, err := st.decoder.Decode(p.Opus, 5760, false); err != nil {
					st.failed++
				} else {
					st.decoded++
				}
				mu.Unlock()
			}
		}
	}()

	for i := 0; i < *seconds; i++ {
		time.Sleep(time.Second)
		daveMu.Lock()
		d := dave
		daveMu.Unlock()
		line := fmt.Sprintf("t+%02ds", i+1)
		if d != nil {
			st, stats := d.State(), d.Stats()
			line += fmt.Sprintf("  dave: ready=%v epoch=%d version=%d welcomes=%d commits=%d decryptFailures=%d",
				st.Ready, st.EpochID, st.ProtocolVersion, stats.WelcomesJoined, stats.CommitsProcessed, stats.DecryptFailures)
		}
		mu.Lock()
		for ssrc, st := range streams {
			line += fmt.Sprintf("  | ssrc %d user %q: %d packets, %d decoded as Opus, %d not", ssrc, vc.UserIDForSSRC(ssrc), st.packets, st.decoded, st.failed)
		}
		mu.Unlock()
		r := vc.ReceiveStats()
		line += fmt.Sprintf("  | udp %d: delivered %d, not audio %d, empty %d, transport failed %d, dave failed %d, unknown user %d, speaking updates %d",
			r.Packets, r.Delivered, r.NotAudio, r.Empty, r.TransportFailed, r.DAVEFailed, r.UnknownUser, r.SpeakingUpdates)
		if r.LastDAVEError != "" {
			line += " (" + r.LastDAVEError + ")"
		}
		fmt.Println(line)
	}
	close(done)
	if err := vc.Disconnect(); err != nil {
		fmt.Println("disconnect:", err)
	}
	time.Sleep(time.Second)
	fmt.Println("left the channel")
}
