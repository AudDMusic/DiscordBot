package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/AudDMusic/audd-go"
	"github.com/Mihonarium/dgvoice"
	"github.com/cryptix/wav"
	"io/ioutil"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Mihonarium/discordgo"
)

var DiscordToken string
var AudDToken string

func main() {
	// Get a token from the Telegram bot: https://t.me/auddbot?start=api and copy it to AudDToken
	AudDToken = ""

	// Create an application here: https://discordapp.com/developers/applications
	// Copy the secret to DiscordToken and get the Client ID
	DiscordToken = ""

	// Create a bot for your Discord application
	// Run this code (e.g. go run main.go)
	// Open https://discordapp.com/api/oauth2/authorize?client_id=<INSERT CLIENT ID HERE>&permissions=1049088&scope=bot and add the bot to a server

	// To recognize a song from a voice channel, type !song or !recognize.
	// It's better to mention users who are playing the song (like !song @SomeRandomMusicBot).
	// If you want the bot to listen to a channel so it can immediately recognize the song from the last 15 second of audio, type !listen.

	dg, err := discordgo.New("Bot " + DiscordToken)
	if capture(err) {
		fmt.Println("Error creating Discord session: ", err)
		return
	}
	dg.AddHandler(ready)
	dg.AddHandler(messageCreate)
	dg.AddHandler(guildCreate)
	err = dg.Open()
	if capture(err) {
		fmt.Println("Error opening Discord session: ", err)
	}
	fmt.Println("AudD music recognition bot is now running.  Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc
	capture(dg.Close())
}

func ready(s *discordgo.Session, _ *discordgo.Ready) {
	capture(s.UpdateStatus(0, "!song"))
}

type serverBuffer struct {
	buf       chan *discordgo.Packet
	start     chan struct{}
	stop      chan struct{}
	LastUsers []string
}

func (v *serverBuffer) Start() {
	v.start <- struct{}{}
}
func (v *serverBuffer) Stop() {
	v.stop <- struct{}{}
}

var serverBuffers = map[string]serverBuffer{}
var mu sync.Mutex

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}
	if strings.HasPrefix(m.Content, "!recognize") || strings.HasPrefix(m.Content, "!song") {
		Users := make([]string, 0)
		if len(m.Mentions) > 0 {
			for _, mention := range m.Mentions {
				Users = append(Users, mention.ID)
			}
		}
		c, err := s.State.Channel(m.ChannelID)
		if capture(err) {
			return
		}
		g, err := s.State.Guild(c.GuildID)
		if capture(err) {
			return
		}
		for _, vs := range g.VoiceStates {
			if vs.UserID == m.Author.ID {
				if len(Users) == 0 {
					mu.Lock()
					Users = serverBuffers[g.ID+"-"+vs.ChannelID].LastUsers
					mu.Unlock()
				}
				if Users == nil {
					Users = make([]string, 0)
				}
				mu.Lock()
				existedBuf, alreadySet := serverBuffers[g.ID+"-"+vs.ChannelID]
				existedBuf.LastUsers = Users
				serverBuffers[g.ID+"-"+vs.ChannelID] = existedBuf
				mu.Unlock()
				if len(Users) == 0 {
					_, _ = s.ChannelMessageSend(m.ChannelID, "For better results, mention who is playing the music (like !song @musicbot)")
				}
				var result audd.RecognitionResult
				if alreadySet {
					result, err = recognizeFromBuffer(existedBuf, Users)
				} else {
					result, err = recordSound(s, g.ID, vs.ChannelID, Users)
				}
				if capture(err) {
					return
				}
				if result.Title != "" {
					_, err := s.ChannelMessageSendEmbed(m.ChannelID, &discordgo.MessageEmbed{
						URL:         result.SongLink,
						Type:        "",
						Title:       result.Title,
						Description: "By " + result.Artist,
						Color:       3066993,
						Footer: &discordgo.MessageEmbedFooter{
							Text:    "Powered by AudD Music Recognition API",
							IconURL: "https://audd.io/logo_t.png",
						},
						Image: &discordgo.MessageEmbedImage{
							URL: result.SongLink + "?thumb",
						},
						Thumbnail: nil,
						Author:    nil,
						Fields: []*discordgo.MessageEmbedField{
							{
								Name:   "Plays on",
								Value:  result.Timecode,
								Inline: true,
							},
							{
								Name:   "Album",
								Value:  result.Album,
								Inline: true,
							},
							{
								Name:   "Label",
								Value:  result.Label,
								Inline: true,
							},
							{
								Name:   "Released",
								Value:  result.ReleaseDate,
								Inline: true,
							},
						},
					})
					capture(err)
				} else {
					_, _ = s.ChannelMessageSend(m.ChannelID, "Couldn't recognize the song")
				}
				return
			}
		}
	}
	if strings.HasPrefix(m.Content, "!listen") {
		c, err := s.State.Channel(m.ChannelID)
		if capture(err) {
			return
		}
		g, err := s.State.Guild(c.GuildID)
		if capture(err) {
			return
		}
		for _, vs := range g.VoiceStates {
			if vs.UserID == m.Author.ID {
				err := CreateAndStartBuffer(s, g.ID, vs.ChannelID)
				if capture(err) {
					return
				}
				_, _ = s.ChannelMessageSend(m.ChannelID, "Listening!\n"+
					"Type !song or !recognize with a mention to recognize a song played by someone mentioned. ")
				return
			}
		}
	}
}

func guildCreate(s *discordgo.Session, event *discordgo.GuildCreate) {
	if event.Guild.Unavailable {
		return
	}
	for _, channel := range event.Guild.Channels {
		if channel.ID == event.Guild.ID {
			_, _ = s.ChannelMessageSend(channel.ID, "Type !song or !recognize while in a voice channel to start music recognition.\n"+
				"Type !listen while in a voice channel, and I'll join it so when you type !song or !recognize I'll immediately recognize the song .")
			return
		}
	}
}

func convertPCMToMono(pcm []int16) []int16 {
	var monoPCM []int16
	for i := 0; i < len(pcm); i += 2 {
		sample1 := pcm[i] //+ pcm[i+1]
		monoPCM = append(monoPCM, sample1)
	}
	return monoPCM
}

func listenBuffer(in chan *discordgo.Packet, size time.Duration, onClose func()) (chan *discordgo.Packet, chan struct{}, chan struct{}) {
	started := make(chan struct{}, 2)
	stop := make(chan struct{}, 2)
	out := make(chan *discordgo.Packet, 50000)
	go func() {
		defer onClose()
		buffered := false
		var ticker *time.Ticker
		isStarted := false
		for f := range in {
			select {
			case <-started:
				isStarted = true
			case <-stop:
				close(out)
				return
			default:
			}
			if ticker == nil {
				ticker = time.NewTicker(size)
			}
			if !buffered {
				select {
				case <-ticker.C:
					buffered = true
					ticker.Stop()
				default:
					out <- f
				}
			}
			if buffered && !isStarted {
				<-out
				out <- f
			}
			if isStarted && buffered {
				out <- &discordgo.Packet{
					Type: []byte("stream-stop"),
				}
				isStarted = false
				ticker = nil
				buffered = false
			}
		}
	}()
	return out, started, stop
}

func checkSSRC(ssrc uint32, Users []string) string {
	mu.Lock()
	defer mu.Unlock()
	if len(Users) == 0 {
		for u, s := range usersSSRCs {
			if ssrc == uint32(s) {
				return u
			}
		}
	}
	for _, u := range Users {
		if ssrc == uint32(usersSSRCs[u]) {
			return u
		}
	}
	return ""
}

func getWavAudio(in chan *discordgo.Packet, readAll bool, Users []string) ([]byte, error) {
	file := wav.File{
		SampleRate:      48000,
		SignificantBits: 16,
		Channels:        1,
	}
	out, err := ioutil.TempFile("", "*.wav")
	if err != nil {
		return nil, err
	}
	writer, err := file.NewWriter(out)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	count := 0
	PCMStreams := make(map[string][]int16)
	for f := range in {
		if bytes.Equal(f.Type, []byte("stream-stop")) {
			break
		}
		count++
		u := checkSSRC(f.SSRC, Users)
		if u == "" {
			continue
		}
		int16Slice := convertPCMToMono(f.PCM)
		if PCMStreams[u] == nil {
			PCMStreams[u] = make([]int16, 0)
		}
		PCMStreams[u] = append(PCMStreams[u], int16Slice...)
		if !readAll {
			if start.Add(time.Second * 15).Before(time.Now()) {
				break
			}
		}
	}
	maxLen := 0
	for i := range PCMStreams {
		if len(PCMStreams[i]) > maxLen {
			maxLen = len(PCMStreams[i])
		}
	}
	resultPCM := make([]int16, maxLen)
	for i := range PCMStreams {
		for j := range PCMStreams[i] {
			resultPCM[j] += PCMStreams[i][j]
		}
	}
	for i := 0; i < len(resultPCM); i++ {
		buf := new(bytes.Buffer)
		err := binary.Write(buf, binary.LittleEndian, resultPCM[i])
		if err != nil {
			return nil, err
		}
		bytes_ := buf.Bytes()
		var byteSlice []byte
		byteSlice = append(byteSlice, bytes_[0])
		byteSlice = append(byteSlice, bytes_[1])
		err = writer.WriteSample(byteSlice)
		if err != nil {
			return nil, err
		}
	}

	capture(writer.Close())
	buf, err := ioutil.ReadFile(out.Name())
	if err != nil {
		return nil, err
	}

	return buf, nil
}

func CreateAndStartBuffer(s *discordgo.Session, guildID, channelID string) error {
	mu.Lock()
	existedBuf, alreadySet := serverBuffers[guildID+"-"+channelID]
	if alreadySet {
		existedBuf.Stop()
	}
	buf, err := startBuffer(s, guildID, channelID)
	if err != nil {
		mu.Unlock()
		return err
	}
	serverBuffers[guildID+"-"+channelID] = buf
	mu.Unlock()
	return nil
}

func recordSound(s *discordgo.Session, guildID, channelID string, Users []string) (audd.RecognitionResult, error) {
	vc, err := s.ChannelVoiceJoin(guildID, channelID, true, false, &h)
	if err != nil {
		return audd.RecognitionResult{}, err
	}
	recv := make(chan *discordgo.Packet, 2)
	go dgvoice.ReceivePCM(vc, recv)
	out, err := os.Create("output.pcm")
	if err != nil {
		return audd.RecognitionResult{}, err
	}
	defer captureFunc(out.Close)
	audioBuf, err := getWavAudio(recv, false, Users)
	if err != nil {
		return audd.RecognitionResult{}, err
	}
	result, err := audd.RecognizeByFile(bytes.NewBuffer(audioBuf), AudDToken, map[string]string{"return": "song_link,timecode"})
	if err != nil {
		return audd.RecognitionResult{}, err
	}
	capture(vc.Disconnect())
	return result.Result, nil
}

func recognizeFromBuffer(buffer serverBuffer, Users []string) (audd.RecognitionResult, error) {
	buffer.Start()
	audioBuf, err := getWavAudio(buffer.buf, true, Users)
	if err != nil {
		return audd.RecognitionResult{}, err
	}
	result, err := audd.RecognizeByFile(bytes.NewBuffer(audioBuf), AudDToken, map[string]string{"return": "song_link,timecode"})
	if err != nil {
		return audd.RecognitionResult{}, err
	}
	return result.Result, nil
}

var usersSSRCs = map[string]int{}
var h = discordgo.VoiceSpeakingUpdateHandler(func(vc *discordgo.VoiceConnection, vs *discordgo.VoiceSpeakingUpdate) {
	if vs.Speaking {
		mu.Lock()
		usersSSRCs[vs.UserID] = vs.SSRC
		mu.Unlock()
	} else {
		mu.Lock()
		delete(usersSSRCs, vs.UserID)
		mu.Unlock()
	}
})

func startBuffer(s *discordgo.Session, guildID, channelID string) (serverBuffer, error) {
	vc, err := s.ChannelVoiceJoin(guildID, channelID, true, false, &h)
	if err != nil {
		return serverBuffer{}, err
	}
	recv := make(chan *discordgo.Packet, 2)
	go dgvoice.ReceivePCM(vc, recv)

	onClose := func() {
		capture(vc.Disconnect())
	}

	audioBuf, started, stop := listenBuffer(recv, time.Second*15, onClose)
	return serverBuffer{audioBuf, started, stop, []string{}}, nil
}

func capture(err error) bool {
	if err == nil {
		return false
	}
	_, file, no, ok := runtime.Caller(1)
	if ok {
		err = fmt.Errorf("%v from %s#%d", err, file, no)
	}
	//go raven.CaptureError(err, nil)
	go fmt.Println(err)
	return true
}
func captureFunc(f func() error) (r bool) {
	err := f()
	if r = err != nil; r {
		_, file, no, ok := runtime.Caller(1)
		if ok {
			err = fmt.Errorf("%v from %s#%d", err, file, no)
		}
		//go raven.CaptureError(err, nil)
		go fmt.Println(err)
	}
	return
}
func init() {
	/*err := raven.SetDSN("")
	if err != nil {
		panic(err)
	}*/
}
