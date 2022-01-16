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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const release = "discord-bot@0.0.3"

const configFile = "config.json"

type BotConfig struct {
	AudDToken            string   `required:"true" default:"test" usage:"the token from dashboard.audd.io" json:"AudDToken"`
	DiscordToken         string   `required:"true" default:"test" usage:"the secret from https://discordapp.com/developers/applications" json:"DiscordToken"`
	DiscordAppID         string   `usage:"the application id from https://discordapp.com/developers/applications" json:"DiscordAppID"`
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

//ToDo: make a good help message
var help = "👋 Hi! I'm a music recognition bot. I'm still in testing and might restart from time to time. Please report any bugs if you experience them.\n\n" +
	"If you see an audio or a video and want to know what's the music, you can reply to it with !song, and the " +
	"bot will identify the music (the commands are subject to change). Or make a right click on the message and pick Apps " +
	"-> Recognize This Song.\n\n" +
	"When you're on a voice channel and someone is playing music there, type \"!song [mention]\"," +
	" mentioning the user playing the music. The bot will record the sound for 12 seconds and then attempt to " +
	"identify the song. (The same as slash /song-vc command.)\n\n" +
	"On a voice channel, you can also use the !listen command, so the bot joins, listens, and keeps the last 12 seconds " +
	"of audio in it's memory, and when you type !song [mention], it will immediately identify music from the last 12 " +
	"seconds. (The same as the slash /listen command.) If you send !disconnect, the bot will leave the VC. (The same as the /disconnect command.)"

// ToDo: move from converting to PCM and stacking to directly recording OPUS? E.g., something like https://github.com/bwmarrin/dca or https://github.com/jonas747/dca

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
		Debug:            false,
		Release:          release,
		AttachStacktrace: true,
	})
	defer func() {
		err := recover()

		if err != nil {
			sentry.CurrentHub().Recover(err)
			sentry.Flush(time.Second * 5)
		}
	}()
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
			dg.AddHandler(cfg.interactionCreate)
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
	message := c.getResult([]audd.RecognitionResult{result}, includePlaysOn,
		publishAnnouncement, nil)
	if message == nil {
		return
	}
	c.sendResult(r.URL.Query().Get("chat_id"), message, false)
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
	buf             chan *discordgo.Packet
	start           chan struct{}
	stop            chan struct{}
	LastUser        string
	InitiatedByUser string
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

func (c *BotConfig) HandleQuery(s *discordgo.Session, m *discordgo.Message) (bool, *discordgo.MessageSend) {
	resultUrl, err := c.GetLinkFromMessage(s, m)
	if capture(err) {
		return false, &discordgo.MessageSend{
			Content:   "Sorry, I got an error from Discord when I tried to get the referenced message",
			Reference: m.Reference(),
		}
	}
	if resultUrl == "" {
		return false, nil
	}
	if strings.Contains(resultUrl, "https://lis.tn/") {
		fmt.Println("Skipping a reply to our comment")
		return false, nil
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

	message := c.getMessageFromRecognitionResult(result, err,
		fmt.Sprintf("Sorry, I couldn't get any audio from %s", resultUrl),
		fmt.Sprintf("Sorry, I couldn't recognize the song."+
			"\n\nI tried to identify music from %s at %s.",
			resultUrl, at), m.Reference())
	return true, message
}

func (c *BotConfig) getMessageFromRecognitionResult(result []audd.RecognitionEnterpriseResult, err error,
	responseNoAudio, responseNoResult string, reference *discordgo.MessageReference) *discordgo.MessageSend {
	songs, highestScore := GetSongs(result, c.MinScore)
	response := &discordgo.MessageSend{}
	if reference != nil {
		response.Reference = reference
	}
	if len(songs) > 0 {
		footerEmbed := &discordgo.MessageEmbed{Fields: make([]*discordgo.MessageEmbedField, 0)}
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
		response.Embeds = []*discordgo.MessageEmbed{footerEmbed}
	}
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
		return response
	}
	if len(songs) > 0 {
		if len(songs) > 1 {
			response.Content += "I got matches with these songs:"
		}
		message := c.getResult(songs, true, true, response)
		return message
	}
	return nil
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

var ApplicationCommands = []*discordgo.ApplicationCommand{
	{
		Type:        discordgo.ChatApplicationCommand,
		Name:        "song-vc",
		Description: "Recognize a song playing on the voice channel you're on",
		Version:     "",
		Options: []*discordgo.ApplicationCommandOption{{
			Type:        discordgo.ApplicationCommandOptionUser,
			Name:        "speaker",
			Description: "User playing the music on the voice channel",
			Required:    true,
		}},
	},
	{
		Type:        discordgo.ChatApplicationCommand,
		Name:        "listen",
		Description: "Join the voice channel and wait for /song-vc, then immediately identify music from last 12 seconds",
	},
	{
		Type:        discordgo.ChatApplicationCommand,
		Name:        "disconnect",
		Description: "Leave the voice channel",
	},
	{
		Type: discordgo.MessageApplicationCommand,
		Name: "Recognize This Song",
	},
}

var commandHandlers = map[string]func(c *BotConfig, s *discordgo.Session, i *discordgo.InteractionCreate){
	"Recognize This Song": func(c *BotConfig, s *discordgo.Session, i *discordgo.InteractionCreate) {
		data := i.ApplicationCommandData()
		if data.Resolved == nil {
			return
		}
		if data.Resolved.Messages == nil {
			return
		}
		var m *discordgo.Message
		for _, m_ := range data.Resolved.Messages {
			m = m_
		}
		if m == nil {
			return
		}
		reacted, message := c.HandleQuery(s, m)
		if !reacted {
			capture(s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Sorry, I couldn't get any audio from this message",
				},
			}))
			return
		}
		capture(s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content:         message.Content,
				Components:      message.Components,
				Embeds:          message.Embeds,
				Files:           message.Files,
				TTS:             message.TTS,
				AllowedMentions: message.AllowedMentions,
			},
		}))
	},
	"song-vc": func(c *BotConfig, s *discordgo.Session, i *discordgo.InteractionCreate) {
		data := i.ApplicationCommandData()
		if i.Member == nil {
			b, _ := json.Marshal(i)
			fmt.Println(string(b))
			b, _ = json.Marshal(data)
			fmt.Println(string(b))
			return
		}
		if i.Member.User == nil {
			b, _ := json.Marshal(i)
			fmt.Println(string(b))
			b, _ = json.Marshal(data)
			fmt.Println(string(b))
			return
		}
		var UserToListenTo string
		if data.Options != nil {
			if len(data.Options) > 0 {
				if data.Options[0].Type == discordgo.ApplicationCommandOptionUser {
					UserToListenTo = data.Options[0].Value.(string)
				}
			}
		}
		capture(s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Collecting 12 seconds of audio...",
			},
		}))
		_, message := c.SongVCCommand(s, i.Member.User.ID, UserToListenTo, i.GuildID, nil)
		if message == nil {
			message = &discordgo.MessageSend{
				Content: "Sorry, I experienced an unexpected error",
			}
		}
		_, err := s.FollowupMessageCreate(c.DiscordAppID, i.Interaction, true, &discordgo.WebhookParams{
			Content:         message.Content,
			Components:      message.Components,
			Embeds:          message.Embeds,
			Files:           message.Files,
			TTS:             message.TTS,
			AllowedMentions: message.AllowedMentions,
			// Flags:           1 << 6,
		})
		capture(err)
	},
	"listen": func(c *BotConfig, s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Member == nil {
			return
		}
		if i.Member.User == nil {
			return
		}
		message := c.ListenCommand(s, i.GuildID, i.Member.User.ID)
		if message == "" {
			message = "Sorry, I can't find a voice channel you're in"
		}
		capture(s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: message,
			},
		}))
	},
	"disconnect": func(c *BotConfig, s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Member == nil {
			return
		}
		if i.Member.User == nil {
			return
		}
		left, message := c.StopListeningCommand(s, i.GuildID, i.Member.User.ID)
		if left {
			capture(s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: message,
				},
			}))
		} else {
			capture(s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: message,
					Flags:   1 << 6,
				},
			}))
		}
	},
}

func (c *BotConfig) interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h, ok := commandHandlers[i.ApplicationCommandData().Name]; ok {
		h(c, s, i)
	} else {
		fmt.Println("Unknown command:", i.ApplicationCommandData().Name)
	}
}

func (c *BotConfig) messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}
	if strings.HasPrefix(m.Content, "!here") {
		fmt.Println(m.ChannelID, m.GuildID)
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "Guild ID: "+m.GuildID+", Channel ID: "+m.ChannelID, m.Reference())
		return
	}
	if m.Content == "!help" {
		_, _ = s.ChannelMessageSendReply(m.ChannelID, help, m.Reference())
		return
	}
	compare := getBodyToCompare(m.Content)
	triggered, trigger := substringInSlice(compare, c.Triggers)
	if triggered {
		reactedToUrl, message := c.HandleQuery(s, m.Message) // Try to find a video or an audio and react to it
		if reactedToUrl {
			if message != nil {
				c.sendResult(m.ChannelID, message, false)
			}
			return
		}
		var UserToListenTo string
		if len(m.Mentions) > 0 {
			UserToListenTo = m.Mentions[0].ID
		}
		channel, err := s.State.Channel(m.ChannelID)
		if capture(err) {
			return
		}
		replyInAnyCase, message := c.SongVCCommand(s, m.Author.ID, UserToListenTo, channel.GuildID, m.Reference())
		if !replyInAnyCase {
			if strings.Count(compare, " ") > strings.Count(trigger, " ")+2 {
				return
			}
		}
		if message != nil {
			c.sendResult(m.ChannelID, message, false)
		}
		return
	}
	if strings.HasPrefix(m.Content, "!listen") {
		reply := c.ListenCommand(s, m.GuildID, m.Author.ID)
		if reply == "" {
			reply = "Sorry, I can't find a voice channel you're in"
		}
		_, _ = s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference()) // ToDo: Add GitHub and Report bug components to all the messages like this one?
	}
	if strings.HasPrefix(m.Content, "!disconnect") {
		_, reply := c.StopListeningCommand(s, m.GuildID, m.Author.ID)
		_, _ = s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference())
	}
}

func (c *BotConfig) SongVCCommand(s *discordgo.Session,
	UserID, UserToListenTo, GuildID string, reference *discordgo.MessageReference) (bool, *discordgo.MessageSend) {
	g, err := s.State.Guild(GuildID)
	if capture(err) {
		return false, nil
	}
	for _, vs := range g.VoiceStates {
		if vs.UserID != UserID {
			continue
		}
		if UserToListenTo == "" {
			mu.Lock()
			UserToListenTo = serverBuffers[g.ID+"-"+vs.ChannelID].LastUser
			mu.Unlock()
		}
		if UserToListenTo == "" {
			reply := &discordgo.MessageSend{
				Content: "Please mention the user playing the music in a voice channel (like !song @musicbot) or " +
					"reply with !song to a message with an audio file or a link to the audio file and I'll identify the music",
			}
			if reference != nil {
				reply.Reference = reference
			}
			return true, reply
		}
		mu.Lock()
		existedBuf, alreadySet := serverBuffers[g.ID+"-"+vs.ChannelID]
		existedBuf.LastUser = UserToListenTo
		serverBuffers[g.ID+"-"+vs.ChannelID] = existedBuf
		mu.Unlock()
		var audioBuf []byte

		if reference != nil {
			go s.MessageReactionAdd(reference.ChannelID, reference.MessageID, "🎧")
		}
		if alreadySet {
			/*_, _ = s.ChannelMessageSendReply(m.ChannelID, "I'll identify the song in the 12 seconds of audio",
			m.Reference())*/
			audioBuf, err = c.getBufferBytes(existedBuf, UserToListenTo)
		} else {
			/*_, _ = s.ChannelMessageSendReply(m.ChannelID, "I'm listening to the audio for 12 seconds and "+
			"will identify the song after that",
			m.Reference())*/
			audioBuf, err = c.recordSound(s, g.ID, vs.ChannelID, UserToListenTo)
			mu.Lock()
			delete(serverBuffers, g.ID+"-"+vs.ChannelID)
			mu.Unlock()
		}
		result, err := AudDClient.RecognizeLongAudio(audioBuf,
			map[string]string{"accurate_offsets": "true", "limit": "1"})
		message := c.getMessageFromRecognitionResult(result, err,
			"Sorry, I couldn't record the audio",
			"Sorry, I couldn't recognize the song.", reference)
		if reference != nil {
			go s.MessageReactionRemove(reference.ChannelID, reference.MessageID, "🎧", "@me")
		}
		return true, message
	}
	reply := &discordgo.MessageSend{
		Content: "You need to be in a voice channel and mention a user " +
			"playing music there or you need to reply to an audio file or URL to identify music from",
	}
	if reference != nil {
		reply.Reference = reference
	}
	return false, reply
}

//ToDo: leave the VC if it's empty/the person who added it has left
//ToDo: a setting allowing to change from 12 to other numbers of  seconds

type GuildChPair struct {
	GuildID   string
	ChannelID string
}

var UsersInvitedBot = map[string]GuildChPair{}

func (c *BotConfig) ListenCommand(s *discordgo.Session, GuildID, UserID string) string {
	g, err := s.State.Guild(GuildID)
	if capture(err) {
		return ""
	}
	mu.Lock()
	ch, exists := UsersInvitedBot[UserID]
	delete(UsersInvitedBot, UserID)
	mu.Unlock()
	if exists {
		StopBuffer(ch.GuildID, ch.ChannelID)
	}
	for _, vs := range g.VoiceStates {
		if vs.UserID != UserID {
			continue
		}
		err := CreateAndStartBuffer(s, g.ID, vs.ChannelID, UserID)
		if capture(err) {
			return ""
		}
		mu.Lock()
		UsersInvitedBot[UserID] = GuildChPair{GuildID: GuildID, ChannelID: vs.ChannelID}
		mu.Unlock()
		return "Listening!\n" +
			"Type !song or !recognize with a mention to recognize a song played by someone mentioned."
	}
	return ""
}
func (c *BotConfig) StopListeningCommand(s *discordgo.Session, GuildID, UserID string) (left bool, response string) {
	leavingResponse := "Bye!"
	mu.Lock()
	ch, exists := UsersInvitedBot[UserID]
	delete(UsersInvitedBot, UserID)
	mu.Unlock()
	if exists {
		// Allow kicking the bot by the user who invited it
		StopBuffer(ch.GuildID, ch.ChannelID)
		response = leavingResponse
		left = true
	}
	g, err := s.State.Guild(GuildID)
	if capture(err) {
		return
	}
	UsersByChannels := map[string]string{}
	for _, vs := range g.VoiceStates {
		UsersByChannels[vs.UserID] = vs.ChannelID
	}
	for _, vs := range g.VoiceStates {
		if vs.UserID != UserID {
			continue
		}
		mu.Lock()
		existedBuf, isSet := serverBuffers[g.ID+"-"+vs.ChannelID]
		mu.Unlock()
		if isSet {
			if UsersByChannels[existedBuf.InitiatedByUser] != vs.ChannelID {
				// Allow kicking the bot by any user on the voice channel if the user who invited it has left
				StopBuffer(GuildID, vs.ChannelID)
				left = true
				response = leavingResponse
				return
			}
			if response == "" {
				response = "Sorry, if the person who invited me to listen to the voice channel is still on the voice channel, only " +
					"they can use this command"
			}
		}
		if response == "" {
			response = "I don't think I'm on the same voice channel as you are"
		}
		return
	}
	response = "You can use this command if you invited me to listen to a voice channel or if you're on a voice channel " +
		"with me and the person who invited me has left"
	return
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

	names := make(map[string]int)
	for i, wantedCmd := range ApplicationCommands {
		names[wantedCmd.Name] = i
	}
	cmds, _ := s.ApplicationCommands(c.DiscordAppID, "")
	for _, oldCmd := range cmds {
		if i, stillWant := names[oldCmd.Name]; stillWant {
			delete(names, oldCmd.Name)
			wantedCmd := ApplicationCommands[i]
			if oldCmd.Description != wantedCmd.Description { // ToDo: full comparison instead of just descriptions?
				updatedCmd, err := s.ApplicationCommandEdit(c.DiscordAppID, "", oldCmd.ID, wantedCmd)
				capture(err)
				b, _ := json.Marshal(updatedCmd)
				fmt.Println("Updated cmd:", string(b))
			}
		} else {
			capture(s.ApplicationCommandDelete(c.DiscordAppID, "", oldCmd.ID))
			fmt.Println("Deleted cmd:", oldCmd.Name)
		}
		// fmt.Println(oldCmd.ID, oldCmd.Name, oldCmd)
	}
	for _, i := range names {
		createdCmd, err := s.ApplicationCommandCreate(c.DiscordAppID, "", ApplicationCommands[i])
		capture(err)
		b, _ := json.Marshal(createdCmd)
		fmt.Println("Added cmd:", string(b))
	}
}

func (c *BotConfig) resumed(s *discordgo.Session, _ *discordgo.Resumed) {
	dSessionMu.Lock()
	dSession = s
	dSessionMu.Unlock()
	capture(s.UpdateListeningStatus("!song"))
}

func (c *BotConfig) getResult(results []audd.RecognitionResult, includePlaysOn, includeScore bool,
	baseMessage *discordgo.MessageSend) *discordgo.MessageSend {

	if baseMessage == nil {
		baseMessage = &discordgo.MessageSend{}
	}
	if baseMessage.Embeds == nil {
		baseMessage.Embeds = make([]*discordgo.MessageEmbed, 0)
	}
	resultEmbeds := make([]*discordgo.MessageEmbed, 0)
	for i, result := range results {
		thumb := result.SongLink + "?thumb"
		if strings.Contains(result.SongLink, "youtu.be/") {
			thumb = "https://i3.ytimg.com/vi/" + strings.ReplaceAll(result.SongLink, "https://youtu.be/", "") + "/maxresdefault.jpg"
		}
		if result.SongLink == "https://lis.tn/VhpgG" || result.SongLink == "" {
			result.SongLink = "https://www.youtube.com/results?search_query=" + url.QueryEscape(result.Artist+" - "+result.Title)
			thumb = ""
		}
		if i > 0 {
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
	return baseMessage
}

func (c *BotConfig) sendResult(channelID string, message *discordgo.MessageSend, publishAnnouncement bool) {
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
	m, err := s.ChannelMessageSendComplex(channelID, message)
	if capture(err) {
		b, _ := json.Marshal(message)
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

//ToDo: fix error capturing
func capture(err error) bool {
	if err == nil {
		return false
	}
	_, file, no, ok := runtime.Caller(1)
	if ok {
		err = fmt.Errorf("%v from %s#%d", err, file, no)
	}
	go sentry.CaptureException(err)
	go fmt.Println(err.Error())
	return true
}
func captureFunc(f func() error) bool {
	return capture(f())
}
