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
	"io"
	"mvdan.cc/xurls/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const release = "discord-bot@0.0.3"

const configFile = "config.json"

type BotConfig struct {
	AudDToken               string   `required:"true" default:"test" usage:"the token from dashboard.audd.io" json:"AudDToken"`
	DiscordToken            string   `required:"true" default:"test" usage:"the secret from https://discordapp.com/developers/applications" json:"DiscordToken"`
	DiscordAppID            string   `usage:"the application id from https://discordapp.com/developers/applications" json:"DiscordAppID"`
	Triggers                []string `usage:"phrases bot will react to" json:"Triggers"`
	AntiTriggers            []string `usage:"phrases bot will avoid replying to" json:"AudDTAntiTriggers"`
	MaxTriggerTextLength    int      `json:"MaxTriggerTextLength"`
	PatreonSupporters       []string `json:"patreon_supporters"`
	SecretCallbackToken     string   `json:"SecretCallbackToken"`
	CallbacksAddr           string   `json:"CallbacksAddr"`
	MaxReplyDepth           int      `json:"MaxReplyDepth"`
	MinScore                int      `json:"MinScore"`
	UncompressedLimit       int      `usage:"the maximum amount of songs to post as large embeds" json:"UncompressedLimit"`
	CompressStartingWith    int      `usage:"the first result to compress when compressing" json:"CompressStartingWith"`
	CanCompressWithoutSlash bool     `usage:"whether can send compressed messages in responses not to usual text" json:"CanCompressWithoutSlash"`
	SentryDSN               string   `default:"" usage:"add a Sentry DSN to capture errors" json:"SentryDSN"`
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
	b, err := io.ReadAll(r.Body)
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
		publishAnnouncement, nil, c.CanCompressWithoutSlash)
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
	InitiatedByUser string
}

func (v *serverBuffer) Start() {
	v.start <- struct{}{}
}
func (v *serverBuffer) Stop() {
	v.stop <- struct{}{}
}

var serverBuffers = map[string]serverBuffer{}
var lastUserListenedTo = map[string]string{}
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

func (c *BotConfig) HandleQuery(s *discordgo.Session, m *discordgo.Message, canCompress bool) (bool, *discordgo.MessageSend) {
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
	fmt.Println("Recognizing from", resultUrl)
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
			resultUrl, at), m.Reference(), canCompress)
	return true, message
}

func (c *BotConfig) getMessageFromRecognitionResult(result []audd.RecognitionEnterpriseResult, err error,
	responseNoAudio, responseNoResult string, reference *discordgo.MessageReference, canCompress bool) *discordgo.MessageSend {
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
				textResponse = "Sorry, there's been an error while processing the audio"
			}
		}
		if textResponse == "" {
			textResponse = responseNoResult
		}
		response.Content += textResponse
		return response
	}
	if len(songs) > 0 {
		message := c.getResult(songs, true, true, response, canCompress)
		return message
	}
	return nil
}

func GetSongs(result []audd.RecognitionEnterpriseResult, minScore int) (songs []audd.RecognitionResult, highestScore int) {
	if len(result) == 0 {
		return
	}
	songs = make([]audd.RecognitionResult, 0)
	links := map[string]bool{}
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
	return
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
		reacted, message := c.HandleQuery(s, m, true)
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
		var UserToListenToID string
		if data.Options != nil {
			if len(data.Options) > 0 {
				if data.Options[0].Type == discordgo.ApplicationCommandOptionUser {
					UserToListenToID = data.Options[0].Value.(string)
				}
			}
		}
		capture(s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Collecting 12 seconds of audio...",
			},
		}))
		_, message := c.SongVCCommand(s, i.Member.User.ID, UserToListenToID, i.GuildID, nil, true)
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
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "[test](https://example.com) Guild ID: "+m.GuildID+", Channel ID: "+m.ChannelID, m.Reference())
		return
	}
	if m.Content == "!help" {
		_, _ = s.ChannelMessageSendReply(m.ChannelID, help, m.Reference())
		return
	}
	compare := getBodyToCompare(m.Content)
	triggered, trigger := substringInSlice(compare, c.Triggers)
	if triggered {
		reactedToUrl, message := c.HandleQuery(s, m.Message, c.CanCompressWithoutSlash) // Try to find a video or an audio and react to it
		if reactedToUrl {
			if message != nil {
				c.sendResult(m.ChannelID, message, false)
			}
			return
		}
		var UserToListenToID string
		if len(m.Mentions) > 0 {
			UserToListenToID = m.Mentions[0].ID
		}
		channel, err := s.State.Channel(m.ChannelID)
		if capture(err) {
			return
		}
		replyInAnyCase, message := c.SongVCCommand(s, m.Author.ID, UserToListenToID, channel.GuildID, m.Reference(), c.CanCompressWithoutSlash)
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
	userID, userToListenToID, guildID string, reference *discordgo.MessageReference, canCompress bool) (bool, *discordgo.MessageSend) {
	g, err := s.State.Guild(guildID)
	if capture(err) {
		return false, nil
	}
	for _, vs := range g.VoiceStates {
		if vs.UserID != userID {
			continue
		}
		if userToListenToID == "" {
			mu.Lock()
			userToListenToID = lastUserListenedTo[g.ID+"-"+vs.ChannelID]
			mu.Unlock()
		}
		if userToListenToID == "" {
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
		lastUserListenedTo[g.ID+"-"+vs.ChannelID] = userToListenToID
		mu.Unlock()
		var audioBuf []byte

		if reference != nil {
			go s.MessageReactionAdd(reference.ChannelID, reference.MessageID, "🎧")
		}
		if alreadySet {
			/*_, _ = s.ChannelMessageSendReply(m.ChannelID, "I'll identify the song in the 12 seconds of audio",
			m.Reference())*/
			audioBuf, err = c.getBufferBytes(existedBuf, userToListenToID)
		} else {
			/*_, _ = s.ChannelMessageSendReply(m.ChannelID, "I'm listening to the audio for 12 seconds and "+
			"will identify the song after that",
			m.Reference())*/
			/*
				if !c.BotInvitedToVC(s, guildID, userID) {
					reply := &discordgo.MessageSend{
						Content: "Sorry, I can't find a voice channel you're in",
					}
					if reference != nil {
						reply.Reference = reference
					}
					return true, reply
				}
				// audioBuf, err = c.recordSound(s, g.ID, vs.ChannelID, userToListenToID)
				mu.Lock()
				existedBuf = serverBuffers[g.ID+"-"+vs.ChannelID]
				mu.Unlock()
				audioBuf, err = c.getBufferBytes(existedBuf, userToListenToID)
				existedBuf.Stop()
				mu.Lock()
				delete(serverBuffers, g.ID+"-"+vs.ChannelID)
				mu.Unlock()
			*/
			audioBuf, err = c.recordSound(s, g.ID, vs.ChannelID, userToListenToID)
			mu.Lock()
			delete(serverBuffers, g.ID+"-"+vs.ChannelID)
			mu.Unlock()
		}
		if capture(err) || audioBuf == nil {
			reply := &discordgo.MessageSend{
				Content: "Sorry, I couldn't capture any audio",
			}
			if reference != nil {
				reply.Reference = reference
			}
			return true, reply
		}
		fmt.Println("Recognizing from a buffer")
		result, err := AudDClient.RecognizeLongAudio(audioBuf,
			map[string]string{"accurate_offsets": "true", "limit": "1"})
		message := c.getMessageFromRecognitionResult(result, err,
			"Sorry, I couldn't record the audio",
			"Sorry, I couldn't recognize the song.", reference, canCompress)
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

func (c *BotConfig) ListenCommand(s *discordgo.Session, guildID, userID string) string {
	if c.BotInvitedToVC(s, guildID, userID) {
		return "Listening!\n" +
			"Type !song with a mention to recognize a song played by someone mentioned."
	}
	return ""
}

func (c *BotConfig) BotInvitedToVC(s *discordgo.Session, guildID, userID string) bool {
	g, err := s.State.Guild(guildID)
	if capture(err) {
		return false
	}
	mu.Lock()
	ch, exists := UsersInvitedBot[userID]
	delete(UsersInvitedBot, userID)
	mu.Unlock()
	if exists {
		StopBuffer(ch.GuildID, ch.ChannelID)
	}
	for _, vs := range g.VoiceStates {
		if vs.UserID != userID {
			continue
		}
		err := CreateAndStartBuffer(s, g.ID, vs.ChannelID, userID)
		if capture(err) {
			return false
		}
		mu.Lock()
		UsersInvitedBot[userID] = GuildChPair{GuildID: guildID, ChannelID: vs.ChannelID}
		mu.Unlock()
		return true
	}
	return false

}

func (c *BotConfig) StopListeningCommand(s *discordgo.Session, guildID, userID string) (left bool, response string) {
	leavingResponse := "Bye!"
	mu.Lock()
	ch, exists := UsersInvitedBot[userID]
	delete(UsersInvitedBot, userID)
	mu.Unlock()
	if exists {
		// Allow kicking the bot by the user who invited it
		StopBuffer(ch.GuildID, ch.ChannelID)
		response = leavingResponse
		left = true
	}
	g, err := s.State.Guild(guildID)
	if capture(err) {
		return
	}
	UsersByChannels := map[string]string{}
	for _, vs := range g.VoiceStates {
		UsersByChannels[vs.UserID] = vs.ChannelID
	}
	for _, vs := range g.VoiceStates {
		if vs.UserID != userID {
			continue
		}
		mu.Lock()
		existedBuf, isSet := serverBuffers[g.ID+"-"+vs.ChannelID]
		mu.Unlock()
		if isSet {
			if UsersByChannels[existedBuf.InitiatedByUser] != vs.ChannelID {
				// Allow kicking the bot by any user on the voice channel if the user who invited it has left
				StopBuffer(guildID, vs.ChannelID)
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
			response = "I don't think I was invited with !listen to the voice channel you are on"
		}
		return
	}
	if response == "" {
		response = "You can use this command if you invited me to listen to a voice channel or if you're on a voice channel " +
			"with me and the person who invited me has left"
	}
	return
}

func (c *BotConfig) getBufferBytes(buffer serverBuffer, userToListenToID string) ([]byte, error) {
	buffer.Start()
	audioBuf, err := getWavAudio(buffer.buf, true, userToListenToID)
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
	j, err := io.ReadAll(f)
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

func getReleaseInfoString(song *audd.RecognitionResult) string {
	album := ""
	label := ""
	releaseDate := ""
	if song.Title != song.Album && song.Album != "" {
		album = "Album: `" + song.Album + "`. "
	}
	if song.Artist != song.Label && song.Label != "Self-released" && song.Label != "" {
		label = " by `" + song.Label + "`"
	}
	if song.ReleaseDate != "" {
		releaseDate = "Released on `" + song.ReleaseDate + "`"
	} else if label != "" {
		label = "Label: " + song.Label
	}
	return fmt.Sprintf("%s%s%s",
		album, releaseDate, label)
}

func addTimecodeToLink(song *audd.RecognitionResult) {
	if strings.Contains(song.Timecode, ":") {
		ms := strings.Split(song.Timecode, ":")
		m, _ := strconv.Atoi(ms[0])
		s, _ := strconv.Atoi(ms[1])
		song.SongLink += "?t=" + strconv.Itoa(m*60+s)
	}
}

func getThumb(song *audd.RecognitionResult) string {
	thumb := song.SongLink + "?thumb"
	if strings.Contains(song.SongLink, "youtu.be/") {
		thumb = "https://i3.ytimg.com/vi/" + strings.ReplaceAll(song.SongLink, "https://youtu.be/", "") + "/maxresdefault.jpg"
	}
	if song.SongLink == "https://lis.tn/VhpgG" || song.SongLink == "" {
		song.SongLink = "https://www.youtube.com/results?search_query=" + url.QueryEscape(song.Artist+" - "+song.Title)
		thumb = ""
	}
	return thumb
}

func (c *BotConfig) getResult(results []audd.RecognitionResult, includePlaysOn, includeScore bool,
	baseMessage *discordgo.MessageSend, canCompress bool) *discordgo.MessageSend {

	if baseMessage == nil {
		baseMessage = &discordgo.MessageSend{}
	}
	if len(results) > 1 {
		baseMessage.Content += "I got matches with these songs:"
	}
	compressToText := len(results) > c.UncompressedLimit && c.UncompressedLimit != -1 && canCompress
	compressToEmbeds := len(results) > c.UncompressedLimit && c.UncompressedLimit != -1 && !canCompress
	if compressToText {
		texts := make([]string, 0)
		for _, song := range results {
			addTimecodeToLink(&song)
			score := strconv.Itoa(song.Score) + "%"
			text := fmt.Sprintf("[**%s** by %s](%s)",
				song.Title, song.Artist, song.SongLink)
			if includeScore {
				text += fmt.Sprintf(" (%s; matched: `%s`)", song.Timecode, score)
			}
			releaseInfo := getReleaseInfoString(&song)
			if releaseInfo != "" {
				text += fmt.Sprintf("\n%s.",
					releaseInfo)
			}
			texts = append(texts, text)
		}
		if len(texts) == 1 {
			baseMessage.Content += texts[0]
		} else {
			for _, text := range texts {
				// response += fmt.Sprintf("\n\n%d. %s", i+1, text)
				baseMessage.Content += fmt.Sprintf("\n\n• %s", text)
			}
		}
		return baseMessage
	}
	if baseMessage.Embeds == nil {
		baseMessage.Embeds = make([]*discordgo.MessageEmbed, 0)
	}
	resultEmbeds := make([]*discordgo.MessageEmbed, 0)
	for i, result := range results {
		thumb := getThumb(&result)
		/*if i > 0 && !compress {
			thumb = ""
		}*/
		addTimecodeToLink(&result)
		score := strconv.Itoa(result.Score) + "%"
		fields := make([]*discordgo.MessageEmbedField, 0)
		if includePlaysOn {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Plays on",
				Value:  result.Timecode,
				Inline: true,
			})
		}
		if includeScore {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Matched",
				Value:  score,
				Inline: true,
			})
		}
		if i >= c.CompressStartingWith && compressToEmbeds {
			description := fmt.Sprintf("By **%s**\n\n", result.Artist)
			/*if includeScore && includePlaysOn {
				description += fmt.Sprintf("At %s; matched: `%s`\n\n", result.Timecode, score)
			} else if includeScore {
				description += fmt.Sprintf("Matched: `%s`\n\n", score)
			} else if includePlaysOn {
				description += fmt.Sprintf("At %s\n\n", result.Timecode)
			}*/
			description += getReleaseInfoString(&result)
			embed := &discordgo.MessageEmbed{
				URL:         result.SongLink,
				Title:       result.Title,
				Description: description,
				Color:       3066993,
				Thumbnail:   nil,
				Author:      nil,
				Fields:      fields,
			}
			if thumb != "" {
				embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
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
			continue
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
			Description: "By **" + result.Artist + "**",
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
