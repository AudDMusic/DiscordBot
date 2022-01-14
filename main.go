package main

import (
	"encoding/json"
	"fmt"
	"github.com/AudDMusic/audd-go"
	"github.com/Mihonarium/discordgo"
	"github.com/Mihonarium/go-profanity"
	"github.com/getsentry/sentry-go"
	"github.com/kodova/html-to-markdown/escape"
	_ "github.com/youpy/go-wav"
	"io/ioutil"
	"mvdan.cc/xurls/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const release = "0.0.2"

const configFile = "config.json"

type BotConfig struct {
	AudDToken            string   `required:"true" default:"test" usage:"the token from dashboard.audd.io" json:"AudDToken"`
	DiscordToken         string   `required:"true" default:"test" usage:"the secret from https://discordapp.com/developers/applications" json:"DiscordToken"`
	Triggers             []string `usage:"phrases bot will react to" json:"Triggers"`
	AntiTriggers         []string `usage:"phrases bot will avoid replying to" json:"AudDTAntiTriggers"`
	MaxTriggerTextLength int      `json:"MaxTriggerTextLength"`
	PatreonSupporters    []string `json:"patreon_supporters"`
	SecretCallbackToken  string   `json:"SecretCallbackToken"`
	CallbacksAddr        string   `json:"CallbacksAddr"`
	MaxReplyDepth        int      `json:"MaxReplyDepth"`
	MinScore             int      `json:"MinScore"`
	SentryDSN            string   `default:"" usage:"add a Sentry DSN to capture errors" json:"SentryDSN"`
}

var dSession *discordgo.Session
var dSessionMu sync.Mutex
var dg *discordgo.Session
var AudDClient *audd.Client

const enterpriseChunkLength = 12

//ToDo: decide on the commands, create a good help message
var help = "If you see an audio or a video and want to know what's the music, reply to it with the !recognize command.\n" +
	"Use the recognize !listen while in a voice channel, and I'll join it and listen to what happens. " +
	"Then, use the !recognize command with a mention of a user and I'll identify the music they're playing"

func main() {
	cfg, err := loadConfig(configFile)
	if err != nil {
		panic(err)
	}
	err = sentry.Init(sentry.ClientOptions{
		// Either set your DSN here or set the SENTRY_DSN environment variable.
		Dsn: cfg.SentryDSN,
		// Enable printing of SDK debug messages.
		// Useful when getting started or trying to figure something out.
		Debug:   false,
		Release: release,
	})
	AudDClient = audd.NewClient(cfg.AudDToken)
	AudDClient.SetEndpoint(audd.EnterpriseAPIEndpoint)
	if err != nil {
		panic(err)
	}
	dSessionMu.Lock() // Unlocks in the goroutine
	go func() {
		dg, err = discordgo.New("Bot " + cfg.DiscordToken)
		if capture(err) {
			fmt.Println("Error creating Discord session: ", err)
			return
		}
		dg.MaxRestRetries = 0
		fmt.Println("Created Discord session")
		go func() {
			dg.AddHandler(cfg.ready)
			dg.AddHandler(cfg.resumed)
			dg.AddHandler(cfg.messageCreate)
			dg.AddHandler(cfg.guildCreate)
		}()
		dSession = dg
		dSessionMu.Unlock() // Unlocks the outside lock so the callback server can start
		fmt.Println("Added the session to dSession")
		err = dg.Open()
		if capture(err) {
			panic(err)
		}
	}()
	http.HandleFunc("/", cfg.HandleCallback)
	err = http.ListenAndServe(cfg.CallbacksAddr, nil)
	capture(err)
}

func (c *BotConfig) HandleCallback(_ http.ResponseWriter, r *http.Request) {
	b, err := ioutil.ReadAll(r.Body)
	defer captureFunc(r.Body.Close)
	if capture(err) {
		return
	}
	if r.URL.Query().Get("secret") != c.SecretCallbackToken {
		return
	}
	var result audd.RecognitionResult
	err = json.Unmarshal(b, &result)
	if capture(err) {
		return
	}
	includePlaysOn := r.URL.Query().Get("includePlaysOn") == "true"
	publishAnnouncement := r.URL.Query().Get("publishAnnouncement") == "true"
	c.sendResult(r.URL.Query().Get("chat_id"), []audd.RecognitionResult{result}, includePlaysOn,
		publishAnnouncement, false, nil)
}

func (c *BotConfig) guildCreate(s *discordgo.Session, event *discordgo.GuildCreate) {
	if event.Guild.Unavailable {
		return
	}
	fmt.Println("Guild: ", event.Guild.Name)

	for _, channel := range event.Guild.Channels {
		fmt.Println(channel.Name, channel.ID, channel.GuildID)
	}
	if event.Guild.JoinedAt.Before(time.Now().Add(-2 * time.Minute)) {
		return
	}
	for _, channel := range event.Guild.Channels {
		if strings.Contains(channel.Name, "bot") {
			_, _ = s.ChannelMessageSend(channel.ID, help)
			fmt.Println(channel.ID)
			return
		}
	}
	for _, channel := range event.Guild.Channels {
		if channel.ID == event.Guild.ID {
			_, _ = s.ChannelMessageSend(channel.ID, help)
			return
		}
	}
}

type serverBuffer struct {
	buf      chan *discordgo.Packet
	start    chan struct{}
	stop     chan struct{}
	LastUser string
}

func (v *serverBuffer) Start() {
	v.start <- struct{}{}
}
func (v *serverBuffer) Stop() {
	v.stop <- struct{}{}
}

var serverBuffers = map[string]serverBuffer{}
var mu sync.Mutex

var rxStrict = xurls.Strict()

func (c *BotConfig) GetLinkFromMessage(s *discordgo.Session, m *discordgo.Message) (string, error) {
	sourceMessage := *m
	var results []string
	for depth := 0; depth <= c.MaxReplyDepth; depth++ {
		results = linksFromMessage(&sourceMessage)
		if len(results) > 0 {
			break
		}
		if sourceMessage.Type != discordgo.MessageTypeReply || sourceMessage.MessageReference == nil {
			break
		}
		if sourceMessage.MessageReference.MessageID != "" {
			replyTo, err := s.ChannelMessage(sourceMessage.MessageReference.ChannelID, sourceMessage.MessageReference.MessageID)
			if err != nil {
				return "", err
			}
			sourceMessage = *replyTo
		}
	}
	if len(results) == 0 {
		return "", nil
	}
	return results[0], nil
}

func GetButtons(includeDonate bool) []discordgo.MessageComponent {
	buttonsRow := &discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.Button{
		Label: "GitHub", Style: discordgo.LinkButton, URL: "https://github.com/AudDMusic/DiscordBot",
	}, discordgo.Button{
		Label: "Report bug", Style: discordgo.LinkButton, URL: "https://github.com/AudDMusic/DiscordBot/issues/new",
	}}}
	if includeDonate {
		buttonsRow.Components = append(buttonsRow.Components, discordgo.Button{
			Label: "Donate", Style: discordgo.LinkButton,
			URL: "https://www.reddit.com/r/AudD/comments/nua48w/please_consider_donating_and_making_the_bot_happy/",
		})
	}
	return []discordgo.MessageComponent{buttonsRow}
}

func (c *BotConfig) HandleQuery(s *discordgo.Session, m *discordgo.Message) bool {
	resultUrl, err := c.GetLinkFromMessage(s, m)
	if capture(err) {
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "Sorry, I got an error from Discord when I tried to "+
			"get the referenced message", m.Reference())
		return false
	}
	if resultUrl == "" {
		return false
	}
	if strings.Contains(resultUrl, "https://lis.tn/") {
		fmt.Println("Skipping a reply to our comment")
		return false
	}
	timestampTo := 0
	timestamp := GetSkipFirstFromLink(resultUrl)
	if timestamp == 0 {
		timestamp, timestampTo = GetTimeFromText(m.Content)
	}
	limit := 2
	if strings.Contains(resultUrl, "https://media.discordapp.net/") {
		limit = 3
	}
	if timestampTo != 0 && timestampTo-timestamp > limit*enterpriseChunkLength {
		// recognize music at the middle of the specified interval
		timestamp += (timestampTo - timestamp - limit*enterpriseChunkLength) / 2
	}
	timestampTo = timestamp + limit*enterpriseChunkLength
	atTheEnd := "false"
	if timestamp == 0 && strings.Contains(m.Content, "at the end") {
		atTheEnd = "true"
	}
	result, err := AudDClient.RecognizeLongAudio(resultUrl,
		map[string]string{"accurate_offsets": "true", "limit": strconv.Itoa(limit),
			"skip_first_seconds": strconv.Itoa(timestamp), "reversed_order": atTheEnd})

	at := SecondsToTimeString(timestamp, timestampTo >= 3600) + "-" + SecondsToTimeString(timestampTo, timestampTo >= 3600)
	if atTheEnd == "true" {
		at = "the end"
	}

	c.sendRecognitionResult(s, m, result, err,
		fmt.Sprintf("Sorry, I couldn't get any audio from %s", resultUrl),
		fmt.Sprintf("Sorry, I couldn't recognize the song."+
			"\n\nI tried to identify music from %s at %s.",
			resultUrl, at))
	return true
}

func (c *BotConfig) sendRecognitionResult(s *discordgo.Session, m *discordgo.Message,
	result []audd.RecognitionEnterpriseResult, err error, responseNoAudio, responseNoResult string) {
	songs, highestScore := GetSongs(result, c.MinScore)
	response := &discordgo.MessageSend{
		//Content:   "<@" + m.Author.ID + "> ",
		Reference: m.Reference(),
	}
	footerEmbed := &discordgo.MessageEmbed{Fields: make([]*discordgo.MessageEmbedField, 0)}
	if len(songs) > 0 {
		footerEmbed.Author = &discordgo.MessageEmbedAuthor{
			Name:    "Powered by AudD Music Recognition API",
			IconURL: "https://audd.io/pride_logo_outline_100px.png",
			URL:     "https://audd.io/",
		}
		if highestScore == 100 {
			/*footerEmbed.Fields = append(footerEmbed.Fields, &discordgo.MessageEmbedField{
				Value: "Please consider supporting the bot on Patreon",
			})*/
			footerEmbed.Footer = &discordgo.MessageEmbedFooter{Text: "Please consider supporting the bot on Patreon"}
		} else {
			footerEmbed.Footer = &discordgo.MessageEmbedFooter{
				Text: "If the matched percent is less than 100, it could be a false positive result"}
		}
	}
	response.Embeds = []*discordgo.MessageEmbed{footerEmbed}
	response.Components = GetButtons(len(songs) > 0)
	if len(songs) == 0 {
		var textResponse string
		if err != nil {
			if v, ok := err.(*audd.Error); ok {
				if v.ErrorCode == 501 {
					textResponse = responseNoAudio
				}
			}
			if textResponse == "" {
				capture(err)
			}
		}
		if textResponse == "" {
			textResponse = responseNoResult
		}
		response.Content += textResponse
		_, err = s.ChannelMessageSendComplex(m.ChannelID, response)
		if capture(err) {
			fmt.Println("error during sending to", m.ChannelID)
		}
		return
	}
	if len(songs) > 0 {
		if len(songs) > 1 {
			response.Content += "I got matches with these songs:"
		}
		c.sendResult(m.ChannelID, songs, true, false, true, response)
	}
}

func GetSongs(result []audd.RecognitionEnterpriseResult, minScore int) ([]audd.RecognitionResult, int) {
	if len(result) == 0 {
		return nil, 0
	}
	highestScore := 0
	links := map[string]bool{}
	songs := make([]audd.RecognitionResult, 0)
	for _, results := range result {
		if len(results.Songs) == 0 {
			capture(fmt.Errorf("enterprise response has a result without any songs"))
		}
		for _, song := range results.Songs {
			if song.Score < minScore {
				continue
			}
			if song.Score > highestScore {
				highestScore = song.Score
			}
			if song.SongLink == "https://lis.tn/rvXTou" || song.SongLink == "https://lis.tn/XIhppO" {
				song.Artist = "The Caretaker (Leyland James Kirby)"
				song.Title = "Everywhere at the End of Time - Stage 1"
				song.Album = "Everywhere at the End of Time - Stage 1"
				song.ReleaseDate = "2016-09-22"
				song.SongLink = "https://www.youtube.com/watch?v=wJWksPWDKOc"
			}
			if song.SongLink != "" {
				if _, exists := links[song.SongLink]; exists { // making sure this song isn't a duplicate
					continue
				}
				links[song.SongLink] = true
			}
			song.Title = profanity.MaskProfanityWithoutKeepingSpaceTypes(song.Title, "*", 2)
			song.Artist = profanity.MaskProfanityWithoutKeepingSpaceTypes(song.Artist, "*", 2)
			song.Album = profanity.MaskProfanityWithoutKeepingSpaceTypes(song.Album, "*", 2)
			song.Label = profanity.MaskProfanityWithoutKeepingSpaceTypes(song.Label, "*", 2)
			song.Title = escape.Markdown(song.Title)
			song.Artist = escape.Markdown(song.Artist)
			song.Album = escape.Markdown(song.Album)
			song.Label = escape.Markdown(song.Label)
			songs = append(songs, song)
		}
	}
	return songs, highestScore
}
func (c *BotConfig) messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}
	if strings.HasPrefix(m.Content, "!here") {
		fmt.Println(m.ChannelID, m.GuildID)
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "Guild ID: "+m.GuildID+", Channel ID: "+m.ChannelID, m.Reference())
	}
	compare := getBodyToCompare(m.Content)
	trigger := substringInSlice(compare, c.Triggers)
	if trigger {
		reactedToUrl := c.HandleQuery(s, m.Message) // Try to find a video or an audio and react to it
		if reactedToUrl {
			return
		}
		var UserToListenTo string
		if len(m.Mentions) > 0 {
			UserToListenTo = m.Mentions[0].ID
		} else {
			_, _ = s.ChannelMessageSendReply(m.ChannelID,
				"Please mention the user playing the music in a voice channel (like !song @musicbot) or "+
					"reply to a message with an audio file or a link to the audio file", m.Reference())
			return
		}
		channel, err := s.State.Channel(m.ChannelID)
		if capture(err) {
			return
		}
		g, err := s.State.Guild(channel.GuildID)
		if capture(err) {
			return
		}
		for _, vs := range g.VoiceStates {
			if vs.UserID != m.Author.ID {
				continue
			}
			if UserToListenTo == "" {
				mu.Lock()
				UserToListenTo = serverBuffers[g.ID+"-"+vs.ChannelID].LastUser
				mu.Unlock()
			}
			if UserToListenTo == "" {
				return
			}
			mu.Lock()
			existedBuf, alreadySet := serverBuffers[g.ID+"-"+vs.ChannelID]
			existedBuf.LastUser = UserToListenTo
			serverBuffers[g.ID+"-"+vs.ChannelID] = existedBuf
			mu.Unlock()
			var audioBuf []byte

			if alreadySet {
				_, _ = s.ChannelMessageSendReply(m.ChannelID, "I'll identify the song in the 12 seconds of audio",
					m.Reference())
				audioBuf, err = c.getBufferBytes(existedBuf, UserToListenTo)
			} else {
				_, _ = s.ChannelMessageSendReply(m.ChannelID, "I'm listening to the audio for 12 seconds and "+
					"will identify the song after that",
					m.Reference())
				audioBuf, err = c.recordSound(s, g.ID, vs.ChannelID, UserToListenTo)
				mu.Lock()
				delete(serverBuffers, g.ID+"-"+vs.ChannelID)
				mu.Unlock()
			}
			result, err := AudDClient.RecognizeLongAudio(audioBuf,
				map[string]string{"accurate_offsets": "true", "limit": "1"})
			c.sendRecognitionResult(s, m.Message, result, err,
				"Sorry, I couldn't record the audio",
				"Sorry, I couldn't recognize the song.")
			return
		}
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "You need to be in a voice channel and mention a user "+
			"playing music there or you need to reply to an audio file or URL to identify music from", m.Reference())
		return
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
				_, _ = s.ChannelMessageSendReply(m.ChannelID, "Listening!\n"+
					"Type !song or !recognize with a mention to recognize a song played by someone mentioned.",
					m.Reference())
				return
			}
		}
	}

}

func (c *BotConfig) getBufferBytes(buffer serverBuffer, User string) ([]byte, error) {
	buffer.Start()
	audioBuf, err := getWavAudio(buffer.buf, true, User)
	if err != nil {
		return nil, err
	}
	return audioBuf, nil
}

func loadConfig(file string) (*BotConfig, error) {
	var cfg BotConfig
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	j, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(j, &cfg)
	if err != nil {
		return nil, err
	}
	if stringInSlice(cfg.Triggers, "") {
		return nil, fmt.Errorf("got a config with an empty string in the triggers")
	}
	if stringInSlice(cfg.AntiTriggers, "") {
		return nil, fmt.Errorf("got a config with an empty string in the anti-triggers")
	}
	return &cfg, nil
}

func (c *BotConfig) ready(s *discordgo.Session, _ *discordgo.Ready) {
	dSessionMu.Lock()
	dSession = s
	dSessionMu.Unlock()
	capture(s.UpdateListeningStatus("!song"))
}

func (c *BotConfig) resumed(s *discordgo.Session, _ *discordgo.Resumed) {
	dSessionMu.Lock()
	dSession = s
	dSessionMu.Unlock()
	capture(s.UpdateListeningStatus("!song"))
}

func (c *BotConfig) sendResult(channelID string, results []audd.RecognitionResult, includePlaysOn,
	publishAnnouncement bool, includeScore bool, baseMessage *discordgo.MessageSend) {
	dSessionMu.Lock()
	s := dSession
	dSessionMu.Unlock()
	if s == nil {
		dSessionMu.Lock()
		err := dg.Open()
		if capture(err) {
			if err != discordgo.ErrWSAlreadyOpen {
				fmt.Println("Error opening Discord session:", err)
				dSessionMu.Unlock()
				return
			}
			s = dg
			dSession = dg
		}
		dSessionMu.Unlock()
	}
	if s == nil {
		return
	}

	if baseMessage == nil {
		baseMessage = &discordgo.MessageSend{}
	}
	if baseMessage.Embeds == nil {
		baseMessage.Embeds = make([]*discordgo.MessageEmbed, 0)
	}
	resultEmbeds := make([]*discordgo.MessageEmbed, 0)
	for _, result := range results {
		thumb := result.SongLink + "?thumb"
		if strings.Contains(result.SongLink, "youtu.be/") {
			thumb = "https://i3.ytimg.com/vi/" + strings.ReplaceAll(result.SongLink, "https://youtu.be/", "") + "/maxresdefault.jpg"
		}
		if result.SongLink == "https://lis.tn/VhpgG" || result.SongLink == "" {
			result.SongLink = "https://www.youtube.com/results?search_query=" + url.QueryEscape(result.Artist+" - "+result.Title)
			thumb = ""
		}
		if strings.Contains(result.Timecode, ":") {
			ms := strings.Split(result.Timecode, ":")
			minutes, _ := strconv.Atoi(ms[0])
			seconds, _ := strconv.Atoi(ms[1])
			result.SongLink += "?t=" + strconv.Itoa(minutes*60+seconds)
		}
		fields := make([]*discordgo.MessageEmbedField, 0)
		if includePlaysOn {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Plays on",
				Value:  result.Timecode,
				Inline: true,
			})
		}
		if includeScore {
			score := strconv.Itoa(result.Score) + "%"
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Matched",
				Value:  score,
				Inline: true,
			})
		}
		if result.Album != "" {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Album",
				Value:  result.Album,
				Inline: true,
			})
		}
		if result.ReleaseDate != "" {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Released on",
				Value:  result.ReleaseDate,
				Inline: true,
			})
		}
		if result.Artist != result.Label && result.Label != "Self-released" && result.Label != "" {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Label",
				Value:  result.Label,
				Inline: true,
			})
		}
		embed := &discordgo.MessageEmbed{
			URL:         result.SongLink,
			Type:        "",
			Title:       result.Title,
			Description: "By " + result.Artist,
			Color:       3066993,
			Thumbnail:   nil,
			Author:      nil,
			Fields:      fields,
		}
		if thumb != "" {
			embed.Image = &discordgo.MessageEmbedImage{
				URL: thumb,
			}
		}
		if len(baseMessage.Embeds) == 0 {
			embed.Footer = &discordgo.MessageEmbedFooter{
				Text:    "Powered by AudD Music Recognition API",
				IconURL: "https://audd.io/pride_logo_outline_100px.png",
			}
		}
		resultEmbeds = append(resultEmbeds, embed)
	}
	resultEmbeds = append(resultEmbeds, baseMessage.Embeds...)
	baseMessage.Embeds = resultEmbeds
	m, err := s.ChannelMessageSendComplex(channelID, baseMessage)
	if capture(err) {
		b, _ := json.Marshal(baseMessage)
		fmt.Println("error during sending to", channelID)
		fmt.Println(string(b))
		return
	}
	if publishAnnouncement {
		_, err = s.ChannelMessageCrosspost(channelID, m.ID)
		if capture(err) {
			fmt.Println("error during publishing to", channelID)
			return
		}
	}
}

func capture(err error) bool {
	if err == nil {
		return false
	}
	go sentry.CaptureException(err)
	go fmt.Println(err.Error())
	return true
}
func captureFunc(f func() error) bool {
	return capture(f())
}
