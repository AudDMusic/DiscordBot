package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/AudDMusic/audd-go"
	"github.com/bwmarrin/discordgo"
	"mvdan.cc/xurls/v2"
)

const (
	enterpriseChunkLength = 12
	recognitionTimeout    = 3 * time.Minute
)

var rxStrict = xurls.Strict()

var (
	errBusy     = errors.New("too many recognitions are running")
	errNoAudio  = errors.New("couldn't get any audio")
	errUpstream = errors.New("the recognition service is unavailable")
)

const (
	replyUserLimited  = "You're sending requests too quickly. Please try again a bit later."
	replyGuildLimited = "This server has made a lot of requests recently. Please try again a bit later."
	replyBusy         = "Sorry, I'm handling a lot of requests right now. Please try again in a minute."
	replyError        = "Sorry, there's been an error while processing the audio"
	replyUnavailable  = "Sorry, music recognition is temporarily unavailable. Please try again later."
)

// admit decides whether the user may start a recognition. When it returns
// false with an empty reply, the request should be silently ignored.
func (b *Bot) admit(userID, guildID string) (ok bool, reply string) {
	if b.isBlocked(userID, guildID) {
		return false, ""
	}
	rl := b.cfg.RateLimits
	if !b.limiter.Allow("user-min:"+userID, rl.UserPerMinute, time.Minute) ||
		!b.limiter.Allow("user-day:"+userID, rl.UserPerDay, 24*time.Hour) {
		return false, replyUserLimited
	}
	if guildID != "" {
		if !b.limiter.Allow("guild-min:"+guildID, rl.GuildPerMinute, time.Minute) ||
			!b.limiter.Allow("guild-day:"+guildID, rl.GuildPerDay, 24*time.Hour) {
			return false, replyGuildLimited
		}
	}
	return true, ""
}

func (b *Bot) isBlocked(userID, guildID string) bool {
	return b.blockedUsers[userID] || guildID != "" && b.blockedGuilds[guildID]
}

// attachmentLinks returns the attachments that can have audio in them, audio and video first.
func attachmentLinks(m *discordgo.Message) []string {
	var media, other []string
	for _, a := range m.Attachments {
		if a == nil || a.URL == "" {
			continue
		}
		contentType := strings.ToLower(a.ContentType)
		switch {
		case strings.HasPrefix(contentType, "audio/"), strings.HasPrefix(contentType, "video/"):
			media = append(media, a.URL)
		case strings.HasPrefix(contentType, "image/"), strings.HasPrefix(contentType, "text/"):
		default:
			other = append(other, a.URL)
		}
	}
	return append(media, other...)
}

// linksFromMessage lists the links to media in the message, the most likely ones first.
func linksFromMessage(m *discordgo.Message) []string {
	links := attachmentLinks(m)
	for _, link := range rxStrict.FindAllString(m.Content, -1) {
		links = append(links, strings.ReplaceAll(link, "\\", ""))
	}
	for _, e := range m.Embeds {
		if e == nil {
			continue
		}
		if e.Video != nil && e.Video.URL != "" {
			links = append(links, e.Video.URL)
		}
		if e.URL != "" {
			links = append(links, e.URL)
		}
	}
	for _, snapshot := range m.MessageSnapshots { // forwarded messages
		if snapshot.Message != nil {
			links = append(links, linksFromMessage(snapshot.Message)...)
		}
	}
	valid := links[:0]
	for _, link := range links {
		if link, ok := validateMediaURL(link); ok {
			valid = append(valid, link)
		}
	}
	return valid
}

// findMediaURL looks for a link in the message and, if there's none, in the messages it replies to.
func (b *Bot) findMediaURL(s *discordgo.Session, m *discordgo.Message) (string, error) {
	current := m
	for depth := 0; current != nil; depth++ {
		if current.Author != nil && b.optOuts.Has(current.Author.ID) {
			return "", errOptedOut
		}
		if links := linksFromMessage(current); len(links) > 0 {
			return links[0], nil
		}
		ref := current.MessageReference
		if depth >= b.cfg.MaxReplyDepth || ref == nil || ref.MessageID == "" || len(current.MessageSnapshots) > 0 {
			break
		}
		if current.ReferencedMessage != nil {
			current = current.ReferencedMessage
			continue
		}
		channelID := ref.ChannelID
		if channelID == "" {
			channelID = current.ChannelID
		}
		replyTo, err := s.ChannelMessage(channelID, ref.MessageID)
		if err != nil {
			return "", err
		}
		current = replyTo
	}
	return "", nil
}

// songsFromMatches filters, de-duplicates and cleans the API results.
func songsFromMatches(matches []audd.EnterpriseMatch, minScore int) (songs []Song, highestScore int) {
	links := map[string]bool{}
	for i := range matches {
		match := &matches[i]
		if match.Score < minScore {
			continue
		}
		song := Song{
			Artist: match.Artist, Title: match.Title, Album: match.Album, ReleaseDate: match.ReleaseDate,
			Label: match.Label, Timecode: match.Timecode, SongLink: match.SongLink, Score: match.Score,
		}
		if song.Title == "" && song.Artist == "" {
			continue
		}
		if song.SongLink == "https://lis.tn/rvXTou" || song.SongLink == "https://lis.tn/XIhppO" {
			song.Artist = "The Caretaker (Leyland James Kirby)"
			song.Title = "Everywhere at the End of Time - Stage 1"
			song.Album = "Everywhere at the End of Time - Stage 1"
			song.ReleaseDate = "2016-09-22"
			song.SongLink = "https://www.youtube.com/watch?v=wJWksPWDKOc"
		}
		if song.SongLink != "" {
			if links[song.SongLink] { // making sure this song isn't a duplicate
				continue
			}
			links[song.SongLink] = true
		}
		if song.Score > highestScore {
			highestScore = song.Score
		}
		songs = append(songs, cleanSong(song))
	}
	return
}

func highestScore(songs []Song) (highest int) {
	for i := range songs {
		if songs[i].Score > highest {
			highest = songs[i].Score
		}
	}
	return
}

// recognize sends the audio to the API. cacheKey may be empty to skip the cache.
// The returned error is errBusy, errNoAudio, errUpstream or an unexpected error.
func (b *Bot) recognize(ctx context.Context, cacheKey string, source audd.Source, opts *audd.EnterpriseOptions) ([]Song, error) {
	run := func() ([]Song, error) {
		select {
		case b.sem <- struct{}{}:
			defer func() { <-b.sem }()
		case <-time.After(15 * time.Second):
			return nil, errBusy
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		ctx, cancel := context.WithTimeout(ctx, recognitionTimeout)
		defer cancel()
		matches, err := b.audd.RecognizeEnterpriseContext(ctx, source, opts)
		if err != nil {
			return nil, classifyAudDError(err)
		}
		songs, _ := songsFromMatches(matches, b.cfg.MinScore)
		return songs, nil
	}
	if cacheKey == "" {
		return run()
	}
	if songs, ok := b.cache.Get(cacheKey); ok {
		return songs, nil
	}
	return b.flights.Do(ctx, cacheKey, func() ([]Song, error) {
		songs, err := run()
		if err == nil {
			b.cache.Put(cacheKey, songs)
		}
		return songs, err
	})
}

func classifyAudDError(err error) error {
	var apiErr *audd.AudDAPIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == 501 || errors.Is(err, audd.ErrInvalidAudio) {
		return errNoAudio
	}
	if errors.Is(err, audd.ErrQuota) || errors.Is(err, audd.ErrAuthentication) || errors.Is(err, audd.ErrRateLimit) ||
		errors.Is(err, audd.ErrSubscription) || errors.Is(err, audd.ErrBlocked) {
		capture(err)
		return errUpstream
	}
	return err
}

// resultMessage turns the outcome of a recognition into the message to post.
func (b *Bot) resultMessage(songs []Song, err error, responseNoAudio, responseNoResult string,
	reference *discordgo.MessageReference, canCompress bool) *discordgo.MessageSend {
	if len(songs) > 0 {
		response := &discordgo.MessageSend{
			Reference:  reference,
			Embeds:     []*discordgo.MessageEmbed{footerEmbed(highestScore(songs))},
			Components: GetButtons(true),
		}
		return b.cfg.getResult(songs, true, true, response, canCompress)
	}
	text := responseNoResult
	switch {
	case err == nil:
	case errors.Is(err, errNoAudio):
		text = responseNoAudio
	case errors.Is(err, errBusy):
		text = replyBusy
	case errors.Is(err, errUpstream):
		text = replyUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		text = "Sorry, it took too long to process the audio"
	default:
		capture(err)
		text = replyError
	}
	response := textMessage(text, reference)
	response.Components = GetButtons(false)
	return response
}

// recognizeFromURL identifies the music at the link. text is the message the request came with:
// it can have a timestamp ("1:23", "1:23-2:00") or ask for the end of the file ("at the end").
func (b *Bot) recognizeFromURL(ctx context.Context, mediaURL, text string,
	reference *discordgo.MessageReference, canCompress bool) *discordgo.MessageSend {
	timestampTo := 0
	timestamp := GetSkipFirstFromLink(mediaURL)
	if timestamp == 0 {
		timestamp, timestampTo = GetTimeFromText(text)
	}
	limit := 2
	if strings.Contains(mediaURL, "https://media.discordapp.net/") {
		limit = 3
	}
	if timestampTo != 0 && timestampTo-timestamp > limit*enterpriseChunkLength {
		// recognize music at the middle of the specified interval
		timestamp += (timestampTo - timestamp - limit*enterpriseChunkLength) / 2
	}
	timestampTo = timestamp + limit*enterpriseChunkLength
	atTheEnd := timestamp == 0 && strings.Contains(strings.ToLower(text), "at the end")

	opts := &audd.EnterpriseOptions{Limit: &limit, SkipFirstSeconds: &timestamp}
	if atTheEnd {
		opts.ExtraParameters = map[string]string{"reversed_order": "true"}
	}
	log.Println("Recognizing from a link")
	cacheKey := cacheKeyForURL(mediaURL) + "|" + strconv.Itoa(timestamp) + "|" + strconv.Itoa(limit) + "|" + strconv.FormatBool(atTheEnd)
	songs, err := b.recognize(ctx, cacheKey, mediaURL, opts)

	at := SecondsToTimeString(timestamp, timestampTo >= 3600) + "-" + SecondsToTimeString(timestampTo, timestampTo >= 3600)
	if atTheEnd {
		at = "the end"
	}
	// <...> keeps Discord from adding a preview of the link to the reply.
	return b.resultMessage(songs, err,
		fmt.Sprintf("Sorry, I couldn't get any audio from <%s>", mediaURL),
		fmt.Sprintf("Sorry, I couldn't recognize the song.\n\nI tried to identify music from <%s> at %s.", mediaURL, at),
		reference, canCompress)
}

// recognizeFromAudio identifies the music in a recording from a voice channel.
func (b *Bot) recognizeFromAudio(ctx context.Context, audio []byte,
	reference *discordgo.MessageReference, canCompress bool) *discordgo.MessageSend {
	limit := 1
	log.Println("Recognizing from a buffer")
	songs, err := b.recognize(ctx, "", audio, &audd.EnterpriseOptions{Limit: &limit})
	return b.resultMessage(songs, err, "Sorry, I couldn't record the audio",
		"Sorry, I couldn't recognize the song.", reference, canCompress)
}
