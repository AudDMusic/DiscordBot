package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Mihonarium/discordgo"
	"github.com/Mihonarium/go-profanity"
)

// Song is one recognized song, ready to be shown.
type Song struct {
	Artist      string `json:"artist,omitempty"`
	Title       string `json:"title,omitempty"`
	Album       string `json:"album,omitempty"`
	ReleaseDate string `json:"release_date,omitempty"`
	Label       string `json:"label,omitempty"`
	Timecode    string `json:"timecode,omitempty"`
	SongLink    string `json:"song_link,omitempty"`
	Score       int    `json:"score,omitempty"`
}

const (
	embedColor = 3066993
	// Discord allows 10 embeds per message; one is used for the footer.
	maxSongsPerMessage = 8
	maxContentLength   = 2000
	poweredBy          = "Powered by AudD Music Recognition API"
	logoURL            = "https://audd.io/pride_logo_outline_100px.png"
)

// maxEmbedCharacters is a bit under the 6000 characters Discord allows in all the embeds of a message.
const maxEmbedCharacters = 5800

// embedSize counts the characters that Discord counts towards the limit.
func embedSize(e *discordgo.MessageEmbed) int {
	size := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
	if e.Footer != nil {
		size += utf8.RuneCountInString(e.Footer.Text)
	}
	if e.Author != nil {
		size += utf8.RuneCountInString(e.Author.Name)
	}
	for _, f := range e.Fields {
		size += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
	}
	return size
}

// noMentions makes sure nothing the bot posts can ping anyone, whatever text ends up in it.
func noMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}

func textMessage(content string, reference *discordgo.MessageReference) *discordgo.MessageSend {
	return &discordgo.MessageSend{
		Content:         truncate(content, maxContentLength),
		Reference:       reference,
		AllowedMentions: noMentions(),
	}
}

var markdownEscaper = strings.NewReplacer(
	`\`, `\\`, `*`, `\*`, `_`, `\_`, `~`, `\~`, "`", "\\`", `|`, `\|`, `>`, `\>`,
	`[`, `\[`, `]`, `\]`, `(`, `\(`, `)`, `\)`, `#`, `\#`,
)

// md escapes text for use in Discord markdown.
func md(s string) string {
	return markdownEscaper.Replace(s)
}

// code makes text safe to put between backticks.
func code(s string) string {
	return strings.ReplaceAll(s, "`", "'")
}

// cleanSong masks profanity and bounds the lengths of everything that comes from the metadata.
func cleanSong(song Song) Song {
	clean := func(s string, max int) string {
		s = strings.Join(strings.Fields(s), " ")
		if s != "" {
			s = profanity.MaskProfanityWithoutKeepingSpaceTypes(s, "*", 2)
		}
		return truncate(s, max)
	}
	song.Title = clean(song.Title, 200)
	song.Artist = clean(song.Artist, 200)
	song.Album = clean(song.Album, 200)
	song.Label = clean(song.Label, 200)
	song.ReleaseDate = truncate(strings.TrimSpace(song.ReleaseDate), 32)
	song.Timecode = truncate(strings.TrimSpace(song.Timecode), 16)
	if link, ok := validateMediaURL(song.SongLink); ok {
		song.SongLink = link
	} else {
		song.SongLink = ""
	}
	return song
}

func GetButtons(includeDonate bool) []discordgo.MessageComponent {
	buttonsRow := discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.Button{
		Label: "GitHub", Emoji: &discordgo.ComponentEmoji{Name: "📝"}, Style: discordgo.LinkButton,
		URL: "https://github.com/AudDMusic/DiscordBot",
	}, discordgo.Button{
		Label: "Report bug", Emoji: &discordgo.ComponentEmoji{Name: "🪲"}, Style: discordgo.LinkButton,
		URL: "https://github.com/AudDMusic/DiscordBot/issues/new",
	}}}
	if includeDonate {
		buttonsRow.Components = append(buttonsRow.Components, discordgo.Button{
			Label: "Donate", Emoji: &discordgo.ComponentEmoji{Name: "💸"}, Style: discordgo.LinkButton,
			URL: "https://github.com/AudDMusic/DiscordBot/wiki/Please-consider-donating",
		})
	}
	return []discordgo.MessageComponent{buttonsRow}
}

func getReleaseInfoString(song *Song) string {
	album := ""
	label := ""
	releaseDate := ""
	if song.Title != song.Album && song.Album != "" {
		album = "Album: `" + code(song.Album) + "`. "
	}
	if song.Artist != song.Label && song.Label != "Self-released" && song.Label != "" {
		label = " by `" + code(song.Label) + "`"
	}
	if song.ReleaseDate != "" {
		releaseDate = "Released on `" + code(song.ReleaseDate) + "`"
	} else if label != "" {
		label = "Label: `" + code(song.Label) + "`"
	}
	return album + releaseDate + label
}

// timecodeSeconds parses "mm:ss" or "hh:mm:ss".
func timecodeSeconds(timecode string) (int, bool) {
	if !strings.Contains(timecode, ":") {
		return 0, false
	}
	seconds, err := TimeStringToSeconds(timecode)
	return seconds, err == nil
}

// linkAndThumb returns the link to show for the song and its cover image (if there is one).
func linkAndThumb(song *Song) (link, thumb string) {
	link = song.SongLink
	u, err := url.Parse(link)
	if link == "" || err != nil || link == "https://lis.tn/VhpgG" {
		return "https://www.youtube.com/results?search_query=" + url.QueryEscape(song.Artist+" - "+song.Title), ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "lis.tn":
		thumb = link + "?thumb"
		if seconds, ok := timecodeSeconds(song.Timecode); ok {
			link += "?t=" + strconv.Itoa(seconds)
		}
	case "youtu.be":
		if id := strings.Trim(u.Path, "/"); id != "" && !strings.Contains(id, "/") {
			thumb = "https://i3.ytimg.com/vi/" + id + "/maxresdefault.jpg"
		}
	}
	return link, thumb
}

// footerEmbed is added to the results of recognizing files and voice channels.
func footerEmbed(highestScore int) *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{
		Author: &discordgo.MessageEmbedAuthor{Name: poweredBy, IconURL: logoURL, URL: "https://audd.io/"},
	}
	if highestScore == 100 {
		embed.Footer = &discordgo.MessageEmbedFooter{Text: "Please consider supporting the bot on Patreon"}
	} else {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text: "If the matched percent is less than 100, it could be a false positive result"}
	}
	return embed
}

// getResult renders the songs into the message. The songs must be already cleaned with cleanSong.
func (c *BotConfig) getResult(results []Song, includePlaysOn, includeScore bool,
	baseMessage *discordgo.MessageSend, canCompress bool) *discordgo.MessageSend {

	if baseMessage == nil {
		baseMessage = &discordgo.MessageSend{}
	}
	baseMessage.AllowedMentions = noMentions()
	if len(results) > maxSongsPerMessage {
		results = results[:maxSongsPerMessage]
	}
	if len(results) > 1 {
		baseMessage.Content += "I got matches with these songs:"
	}
	overLimit := len(results) > c.UncompressedLimit && c.UncompressedLimit != -1
	if overLimit && canCompress {
		texts := make([]string, 0, len(results))
		for i := range results {
			song := &results[i]
			link, _ := linkAndThumb(song)
			text := fmt.Sprintf("[**%s** by %s](<%s>)", md(song.Title), md(song.Artist), link)
			if includeScore {
				text += fmt.Sprintf(" (%s; matched: `%d%%`)", md(song.Timecode), song.Score)
			}
			if releaseInfo := getReleaseInfoString(song); releaseInfo != "" {
				text += "\n" + releaseInfo + "."
			}
			texts = append(texts, text)
		}
		if len(texts) == 1 {
			baseMessage.Content += texts[0]
		} else {
			for _, text := range texts {
				if utf8.RuneCountInString(baseMessage.Content)+utf8.RuneCountInString(text)+4 > maxContentLength {
					break
				}
				baseMessage.Content += "\n\n• " + text
			}
		}
		baseMessage.Content = truncate(baseMessage.Content, maxContentLength)
		return baseMessage
	}
	resultEmbeds := make([]*discordgo.MessageEmbed, 0, len(results)+len(baseMessage.Embeds))
	budget := maxEmbedCharacters
	for _, embed := range baseMessage.Embeds {
		budget -= embedSize(embed)
	}
	for i := range results {
		result := &results[i]
		link, thumb := linkAndThumb(result)
		fields := make([]*discordgo.MessageEmbedField, 0, 5)
		addField := func(name, value string) {
			if value != "" {
				fields = append(fields, &discordgo.MessageEmbedField{Name: name, Value: truncate(value, 1024), Inline: true})
			}
		}
		if includePlaysOn {
			addField("Plays on", md(result.Timecode))
		}
		if includeScore {
			addField("Matched", strconv.Itoa(result.Score)+"%")
		}
		embed := &discordgo.MessageEmbed{
			URL:   link,
			Title: truncate(md(result.Title), 256),
			Color: embedColor,
		}
		if embed.Title == "" {
			embed.Title = "Unknown title"
		}
		if overLimit && i >= c.CompressStartingWith {
			embed.Description = fmt.Sprintf("By **%s**\n\n", md(result.Artist)) + getReleaseInfoString(result)
			if thumb != "" {
				embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: thumb}
			}
		} else {
			embed.Description = "By **" + md(result.Artist) + "**"
			addField("Album", md(result.Album))
			addField("Released on", md(result.ReleaseDate))
			if result.Artist != result.Label && result.Label != "Self-released" {
				addField("Label", md(result.Label))
			}
			if thumb != "" {
				embed.Image = &discordgo.MessageEmbedImage{URL: thumb}
			}
		}
		embed.Fields = fields
		if len(baseMessage.Embeds) == 0 {
			embed.Footer = &discordgo.MessageEmbedFooter{Text: poweredBy, IconURL: logoURL}
		}
		if budget -= embedSize(embed); budget < 0 && len(resultEmbeds) > 0 {
			break // Discord rejects the messages with too much text in the embeds
		}
		resultEmbeds = append(resultEmbeds, embed)
	}
	baseMessage.Embeds = append(resultEmbeds, baseMessage.Embeds...)
	return baseMessage
}
