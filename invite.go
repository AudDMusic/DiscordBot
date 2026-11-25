package main

import (
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/Mihonarium/discordgo"
)

// inviteLinkRegexp finds a Discord invite link and captures its code.
var inviteLinkRegexp = regexp.MustCompile(`(?i)\b(?:https?://)?(?:www\.)?(?:discord\.gg|discord(?:app)?\.com/invite)/([a-z0-9-]{2,64})`)

// inviteCode returns the code of the first Discord invite link in the text ("" if there is none).
func inviteCode(content string) string {
	match := inviteLinkRegexp.FindStringSubmatch(content)
	if match == nil {
		return ""
	}
	return match[1]
}

const (
	replyInviteExpired  = "That invite doesn't work anymore. Please send me a new one."
	replyInviteJoinVC   = "Join that voice channel first, then invite me again: I only join a voice channel when the person inviting me is in it."
	replyInviteStarting = "Sorry, I'm still starting up. Please try again in a minute."
	replyInviteJoined   = "I've joined <#%s> and I'm listening. On that server, use **/song-vc** and pick who's playing the music, " +
		"and I'll identify the song from the last 12 seconds. **/disconnect** makes me leave."
)

// addBotLink is where someone can add the bot to a server.
func addBotLink(botID string) string {
	return "https://discord.com/oauth2/authorize?client_id=" + botID + "&permissions=277026819136&scope=bot%20applications.commands"
}

// voiceInviteProblem checks an invite that someone sent the bot in a direct message. It returns
// what to tell them when the bot can't follow the invite, or "" when the bot may join the channel.
// gs is the session of the shard the invite's server is on.
func (b *Bot) voiceInviteProblem(gs *discordgo.Session, invite *discordgo.Invite, userID, botID string) string {
	if invite == nil || invite.Guild == nil || invite.Channel == nil {
		return replyInviteExpired
	}
	isVoice := invite.Channel.Type == discordgo.ChannelTypeGuildVoice || invite.Channel.Type == discordgo.ChannelTypeGuildStageVoice
	if !isVoice {
		return "I can't join a server through an invite link. To add me to a server, use <" + addBotLink(botID) + ">.\n\n" +
			"To have me listen to a voice channel, invite me to the voice channel you're in."
	}
	if gs == nil {
		return replyInviteStarting
	}
	if _, err := gs.State.Guild(invite.Guild.ID); err != nil {
		return "I'm not on that server yet. Someone who manages it can add me with <" + addBotLink(botID) + ">, and then you can invite me to the voice channel."
	}
	if userVoiceChannel(gs, invite.Guild.ID, userID) != invite.Channel.ID {
		return replyInviteJoinVC
	}
	return ""
}

// handleVoiceInvite reacts to a direct message with an invite to a voice channel, which is what
// Discord's "Invite to Voice" button sends: the bot joins the channel, as it does for /listen.
// It reports whether the message was an invite.
func (b *Bot) handleVoiceInvite(s *discordgo.Session, m *discordgo.Message) bool {
	code := inviteCode(m.Content)
	if code == "" {
		return false
	}
	if !b.limiter.Allow("invite:"+m.Author.ID, 5, time.Minute) {
		b.notifyLimited(s, m)
		return true
	}
	if !b.cfg.VoiceEnabled {
		b.reply(m.ChannelID, textMessage(replyVoiceUnavailable, nil))
		return true
	}
	_ = s.ChannelTyping(m.ChannelID)

	invite, err := s.Invite(code)
	if err != nil {
		var restErr *discordgo.RESTError
		if !errors.As(err, &restErr) || restErr.Response == nil || restErr.Response.StatusCode != 404 {
			log.Println("can't look up an invite:", err)
		}
		b.reply(m.ChannelID, textMessage(replyInviteExpired, nil))
		return true
	}
	if invite.Guild != nil && b.isBlocked(m.Author.ID, invite.Guild.ID) {
		return true
	}
	var gs *discordgo.Session
	if invite.Guild != nil {
		gs = b.sessionForGuild(invite.Guild.ID)
	}
	if problem := b.voiceInviteProblem(gs, invite, m.Author.ID, s.State.User.ID); problem != "" {
		b.reply(m.ChannelID, textMessage(problem, nil))
		return true
	}
	joined, text := b.listen(gs, invite.Guild.ID, invite.Channel.ID, m.Author.ID)
	log.Println("Voice invite in a direct message: joined =", joined)
	if joined {
		text = fmt.Sprintf(replyInviteJoined, invite.Channel.ID)
	}
	b.reply(m.ChannelID, textMessage(text, nil))
	return true
}
