package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mihonarium/discordgo"
)

// reply sends a message, logging the error if there's one.
func (b *Bot) reply(channelID string, message *discordgo.MessageSend) {
	if err := b.sendResult(channelID, message, false); err != nil {
		log.Println(err)
	}
}

// notifyLimited lets the user know (at most once a minute) that the request was dropped.
func (b *Bot) notifyLimited(s *discordgo.Session, m *discordgo.Message) {
	if b.limiter.Allow("limit-notice:"+m.Author.ID, 1, time.Minute) {
		_ = s.MessageReactionAdd(m.ChannelID, m.ID, "⏳")
	}
}

func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer recoverPanic("messageCreate")
	if m.Message == nil || m.Author == nil || m.Content == "" || s.State.User == nil {
		return
	}
	// Never react to bots (including ourselves) and webhooks: that's how reply loops start.
	if m.Author.Bot || m.WebhookID != "" || m.Author.ID == s.State.User.ID {
		return
	}
	if b.isBlocked(m.Author.ID, m.GuildID) || b.optOuts.Has(m.Author.ID) {
		return // opted-out users' messages are dropped before anything looks at their text
	}
	if m.GuildID == "" && b.handleVoiceInvite(s, m.Message) {
		return
	}
	command := strings.ToLower(strings.TrimSpace(m.Content))
	isCommand := func(name string) bool {
		return command == name || strings.HasPrefix(command, name+" ")
	}
	utility := func() bool { return b.limiter.Allow("utility:"+m.Author.ID, 5, time.Minute) }
	switch {
	case isCommand("!here"):
		if utility() {
			b.reply(m.ChannelID, textMessage("Guild ID: "+m.GuildID+", Channel ID: "+m.ChannelID, m.Reference()))
		}
		return
	case isCommand("!total-servers"):
		if utility() {
			servers, members := b.stats()
			b.reply(m.ChannelID, textMessage(fmt.Sprintf("I'm on %d servers with %d members", servers, members), m.Reference()))
		}
		return
	case isCommand("!help"):
		if utility() {
			b.reply(m.ChannelID, textMessage(b.helpText(), m.Reference()))
		}
		return
	case isCommand("!listen"):
		if utility() {
			text := replyVoiceUnavailable
			if b.cfg.VoiceEnabled && m.GuildID != "" {
				text = b.ListenCommand(s, m.GuildID, m.Author.ID)
			}
			b.reply(m.ChannelID, textMessage(text, m.Reference()))
		}
		return
	case isCommand("!disconnect"):
		if utility() && b.cfg.VoiceEnabled && m.GuildID != "" {
			_, text := b.StopListeningCommand(s, m.GuildID, m.Author.ID)
			b.reply(m.ChannelID, textMessage(text, m.Reference()))
		}
		return
	}
	if b.cfg.MaxTriggerTextLength > 0 && utf8.RuneCountInString(m.Content) > b.cfg.MaxTriggerTextLength {
		return
	}
	compare := getBodyToCompare(m.Content)
	triggered, trigger := substringInSlice(compare, b.cfg.Triggers)
	if !triggered {
		return
	}
	if avoid, _ := substringInSlice(compare, b.cfg.AntiTriggers); avoid {
		return
	}
	b.handleTrigger(s, m.Message, compare, trigger)
}

// handleTrigger reacts to a message asking to identify a song.
func (b *Bot) handleTrigger(s *discordgo.Session, m *discordgo.Message, compare, trigger string) {
	// Looking at the replied-to messages can take requests to Discord, so it has its own (generous) limit.
	if !b.limiter.Allow("lookup:"+m.Author.ID, 20, time.Minute) {
		return
	}
	// A long message that happens to have a trigger phrase in it is probably not a request to the bot.
	isShort := strings.Count(compare, " ") <= strings.Count(trigger, " ")+2
	canCompress := b.cfg.CanCompressWithoutSlash

	mediaURL, err := b.findMediaURL(s, m) // Try to find a video or an audio and react to it
	if errors.Is(err, errOptedOut) {
		if isShort {
			b.reply(m.ChannelID, textMessage(replyAuthorOptedOut, m.Reference()))
		}
		return
	}
	if err != nil {
		log.Println("can't get a referenced message:", err)
		if isShort {
			b.reply(m.ChannelID, textMessage("Sorry, I got an error from Discord when I tried to get the referenced message", m.Reference()))
		}
		return
	}
	if mediaURL != "" {
		if isOwnLink(mediaURL) {
			return // a reply to our own result
		}
		if ok, _ := b.admit(m.Author.ID, m.GuildID); !ok {
			b.notifyLimited(s, m)
			return
		}
		_ = s.ChannelTyping(m.ChannelID)
		ctx, cancel := context.WithTimeout(context.Background(), recognitionTimeout)
		defer cancel()
		b.reply(m.ChannelID, b.recognizeFromURL(ctx, mediaURL, m.Content, m.Reference(), canCompress))
		return
	}

	if !b.cfg.VoiceEnabled || m.GuildID == "" {
		if isShort {
			hint := "Please reply with !song to a message with an audio or video file or " +
				"a link to it and I'll identify the music"
			if !b.contentIntent && m.GuildID != "" {
				// Without the Message Content intent, Discord doesn't show the bot the other messages.
				hint = "I can't see the message with the audio or video from here. Right-click it " +
					"(long-press on a phone) and pick Apps -> Recognize This Song, and I'll identify the music."
			}
			b.reply(m.ChannelID, textMessage(hint, m.Reference()))
		}
		return
	}
	var userToListenToID string
	for _, mentioned := range m.Mentions {
		if mentioned != nil && mentioned.ID != s.State.User.ID {
			userToListenToID = mentioned.ID
			break
		}
	}
	_ = s.MessageReactionAdd(m.ChannelID, m.ID, "🎧")
	ctx, cancel := context.WithTimeout(context.Background(), recognitionTimeout+time.Minute)
	defer cancel()
	replyInAnyCase, message := b.SongVCCommand(ctx, s, m.Author.ID, userToListenToID, m.GuildID, m.Reference(), canCompress)
	_ = s.MessageReactionRemove(m.ChannelID, m.ID, "🎧", "@me")
	if message == nil || !replyInAnyCase && !isShort {
		return
	}
	b.reply(m.ChannelID, message)
}
