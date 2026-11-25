package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const helpIntro = "👋 Hi! I'm a music recognition bot.\n\n" +
	"If you see an audio or a video and want to know what's the music, you can reply to it with **!song**, and the " +
	"bot will identify the music. Or make a right click on the message and pick Apps -> Recognize This Song. " +
	"You can add a timestamp (like **!song 1:25**) or **at the end** to pick the part of the file to listen to.\n\n"

const helpVoice = "When you're on a voice channel and someone is playing music there, type the slash **/song-vc [mention]** command, " +
	"mentioning the user playing the music (**!song [mention]** also works). The bot will record the sound for 12 seconds and then attempt to " +
	"identify the song.\n\n" +
	"On a voice channel, you can also use the slash **/listen** command (or **!listen**), and the bot will join the VC, " +
	"listen, and keep the last 12 seconds " +
	"of audio in its memory, and when you type **/song-vc [mention]** (or **!song [mention]**), it will immediately identify music from the last 12 " +
	"seconds of what the mentioned user or bot has played on the voice channel. If you send **/disconnect**, the bot will leave the VC.\n\n"

const helpOutro = "If you don't want me to read your messages, use **/privacy opt-out**. " +
	"Privacy policy: <" + privacyPolicyURL + ">. Source code: <https://github.com/AudDMusic/DiscordBot>.\n\n" +
	"Please report any bugs if you experience them. Support server: https://audd.cc/discord"

// helpIntroNoContent is used while the bot can't read the messages that don't mention it.
const helpIntroNoContent = "👋 Hi! I'm a music recognition bot.\n\n" +
	"If you see an audio or a video and want to know what's the music, right-click the message " +
	"(long-press on a phone) and pick Apps -> Recognize This Song.\n\n"

func (b *Bot) helpText() string {
	intro := helpIntro
	if !b.contentIntent {
		intro = helpIntroNoContent
	}
	if b.cfg.VoiceEnabled {
		return intro + helpVoice + helpOutro
	}
	return intro + helpOutro
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func (b *Bot) applicationCommands() []*discordgo.ApplicationCommand {
	guildOnly := &[]discordgo.InteractionContextType{discordgo.InteractionContextGuild}
	commands := []*discordgo.ApplicationCommand{
		{
			Type:        discordgo.ChatApplicationCommand,
			Name:        "help",
			Description: "Display the help message",
		},
		{
			Type:        discordgo.ChatApplicationCommand,
			Name:        "privacy",
			Description: "Choose whether the bot may process your messages and voice, or see the privacy policy",
			Options: []*discordgo.ApplicationCommandOption{{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "action",
				Description: "What to do",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "opt-out: don't process my messages and voice", Value: "opt-out"},
					{Name: "opt-in: undo the opt-out", Value: "opt-in"},
					{Name: "status: show my current setting and the privacy policy", Value: "status"},
				},
			}},
		},
		{
			Type: discordgo.MessageApplicationCommand,
			Name: "Recognize This Song",
		},
	}
	if b.cfg.VoiceEnabled {
		commands = append(commands, &discordgo.ApplicationCommand{
			Type:        discordgo.ChatApplicationCommand,
			Name:        "song-vc",
			Description: "Recognize a song playing on the voice channel you're on",
			Contexts:    guildOnly,
			Options: []*discordgo.ApplicationCommandOption{{
				Type:        discordgo.ApplicationCommandOptionUser,
				Name:        "speaker",
				Description: "User playing the music on the voice channel",
				Required:    true,
			}},
		}, &discordgo.ApplicationCommand{
			Type:        discordgo.ChatApplicationCommand,
			Name:        "listen",
			Description: "Join the voice channel and wait for /song-vc, then immediately identify music from last 12 seconds",
			Contexts:    guildOnly,
		}, &discordgo.ApplicationCommand{
			Type:        discordgo.ChatApplicationCommand,
			Name:        "disconnect",
			Description: "Leave the voice channel",
			Contexts:    guildOnly,
		})
	}
	return commands
}

// interactionUser returns who used the command, in a server or in DMs.
func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, content string, ephemeral bool) {
	data := &discordgo.InteractionResponseData{
		Content:         truncate(content, maxContentLength),
		AllowedMentions: noMentions(),
	}
	if ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource, Data: data,
	})
	if err != nil {
		log.Println("can't respond to an interaction:", err)
	}
}

// deferResponse tells Discord that the answer will take more than 3 seconds.
func deferResponse(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Println("can't defer an interaction:", err)
	}
	return err == nil
}

// editResponse replaces the deferred response with the message.
func editResponse(s *discordgo.Session, i *discordgo.InteractionCreate, message *discordgo.MessageSend) {
	if message == nil {
		message = textMessage("Sorry, I experienced an unexpected error", nil)
	}
	if message.Embeds == nil {
		message.Embeds = []*discordgo.MessageEmbed{}
	}
	if message.Components == nil {
		message.Components = []discordgo.MessageComponent{}
	}
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:         &message.Content,
		Embeds:          &message.Embeds,
		Components:      &message.Components,
		AllowedMentions: noMentions(),
	})
	if err != nil {
		log.Println("can't edit an interaction response:", err)
	}
}

func (b *Bot) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer recoverPanic("interactionCreate")
	if i.Interaction == nil || i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	user := interactionUser(i)
	if user == nil {
		return
	}
	if b.isBlocked(user.ID, i.GuildID) {
		respond(s, i, "Sorry, you can't use this command", true)
		return
	}
	data := i.ApplicationCommandData()
	switch data.Name {
	case "help":
		respond(s, i, b.helpText(), true)
	case "privacy":
		b.privacyCommand(s, i, user, data)
	case "Recognize This Song":
		b.recognizeMessageCommand(s, i, user, data)
	case "song-vc":
		b.songVCSlashCommand(s, i, user, data)
	case "listen":
		if !b.voiceCommandAllowed(s, i) {
			return
		}
		if !deferResponse(s, i) {
			return
		}
		editResponse(s, i, textMessage(b.ListenCommand(s, i.GuildID, user.ID), nil))
	case "disconnect":
		if !b.voiceCommandAllowed(s, i) {
			return
		}
		left, message := b.StopListeningCommand(s, i.GuildID, user.ID)
		respond(s, i, message, !left)
	default:
		log.Println("Unknown command:", data.Name)
		respond(s, i, "Sorry, I don't know this command", true)
	}
}

func (b *Bot) voiceCommandAllowed(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	if !b.cfg.VoiceEnabled {
		respond(s, i, replyVoiceUnavailable, true)
		return false
	}
	if i.GuildID == "" {
		respond(s, i, "This command only works on servers", true)
		return false
	}
	return true
}

func (b *Bot) recognizeMessageCommand(s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User,
	data discordgo.ApplicationCommandInteractionData) {
	var m *discordgo.Message
	if data.Resolved != nil {
		m = data.Resolved.Messages[data.TargetID]
	}
	if m == nil {
		respond(s, i, "Sorry, I couldn't get this message", true)
		return
	}
	if m.Author != nil && b.optOuts.Has(m.Author.ID) {
		respond(s, i, replyAuthorOptedOut, true)
		return
	}
	if len(linksFromMessage(m)) == 0 && (m.MessageReference == nil || b.cfg.MaxReplyDepth == 0) {
		respond(s, i, "Sorry, I couldn't get any audio from this message", true)
		return
	}
	if ok, reply := b.admit(user.ID, i.GuildID); !ok {
		respond(s, i, reply, true)
		return
	}
	if !deferResponse(s, i) {
		return
	}
	mediaURL, err := b.findMediaURL(s, m)
	if errors.Is(err, errOptedOut) {
		editResponse(s, i, textMessage(replyAuthorOptedOut, nil))
		return
	}
	if err != nil || mediaURL == "" || isOwnLink(mediaURL) {
		editResponse(s, i, textMessage("Sorry, I couldn't get any audio from this message", nil))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recognitionTimeout)
	defer cancel()
	editResponse(s, i, b.recognizeFromURL(ctx, mediaURL, m.Content, nil, true))
}

func (b *Bot) songVCSlashCommand(s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User,
	data discordgo.ApplicationCommandInteractionData) {
	if !b.voiceCommandAllowed(s, i) {
		return
	}
	userToListenToID := ""
	if option := data.GetOption("speaker"); option != nil && option.Type == discordgo.ApplicationCommandOptionUser {
		if id, ok := option.Value.(string); ok {
			userToListenToID = id
		}
	}
	if userVoiceChannel(s, i.GuildID, user.ID) == "" {
		respond(s, i, "You need to be in a voice channel to use this command", true)
		return
	}
	if !deferResponse(s, i) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recognitionTimeout+time.Minute)
	defer cancel()
	_, message := b.SongVCCommand(ctx, s, user.ID, userToListenToID, i.GuildID, nil, true)
	if message == nil {
		message = textMessage("Sorry, you can't use this command", nil)
	}
	editResponse(s, i, message)
}

// privacyCommand lets people opt out of (and back into) the bot processing their messages and voice.
func (b *Bot) privacyCommand(s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User,
	data discordgo.ApplicationCommandInteractionData) {
	action := ""
	if option := data.GetOption("action"); option != nil {
		action, _ = option.Value.(string)
	}
	if !b.limiter.Allow("privacy:"+user.ID, 10, time.Minute) {
		respond(s, i, replyUserLimited, true)
		return
	}
	policy := "\n\nPrivacy policy: <" + privacyPolicyURL + ">"
	switch action {
	case "opt-out", "opt-in":
		optOut := action == "opt-out"
		if err := b.optOuts.Set(user.ID, optOut); err != nil {
			capture(fmt.Errorf("can't save the opt-out list: %w", err))
			respond(s, i, "Sorry, I couldn't save your choice. Please try again later or write to privacy@audd.io.", true)
			return
		}
		if optOut {
			respond(s, i, replyOptedOut+policy, true)
		} else {
			respond(s, i, replyOptedIn+policy, true)
		}
	default:
		status := "You haven't opted out: I check the messages I can see for requests like !song and forget everything else right away."
		if b.optOuts.Has(user.ID) {
			status = "You've opted out: I don't read your messages or listen to you in voice channels."
		}
		respond(s, i, status+policy, true)
	}
}
