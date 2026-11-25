package main

import (
	"encoding/json"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Mihonarium/discordgo"
)

// onRawEvent is a development aid, used only with "DebugGatewayEvents": true. It logs the gateway
// events the library doesn't know and the shape of direct messages and unusual interactions, to
// find out what Discord sends for features its documentation doesn't cover. It doesn't log the
// text of messages.
func (b *Bot) onRawEvent(_ *discordgo.Session, e *discordgo.Event) {
	defer recoverPanic("rawEvent")
	switch e.Type {
	case "MESSAGE_CREATE":
		var fields map[string]json.RawMessage
		if json.Unmarshal(e.RawData, &fields) != nil {
			return
		}
		if _, inGuild := fields["guild_id"]; inGuild {
			return
		}
		var author struct {
			Bot bool `json:"bot"`
		}
		_ = json.Unmarshal(fields["author"], &author)
		if author.Bot {
			return
		}
		var content string
		_ = json.Unmarshal(fields["content"], &content)
		log.Printf("Debug: direct message: type=%s flags=%s, %d characters, invite link: %v, fields with a value: %s",
			fields["type"], fields["flags"], len(content), inviteCode(content) != "", strings.Join(keysWithValue(fields), " "))
		if content == "" && isEmptyJSON(fields["attachments"]) {
			// Not a text message: its structure is what we're trying to learn.
			delete(fields, "author")
			delete(fields, "mentions")
			raw, _ := json.Marshal(fields)
			log.Printf("Debug: direct message without text: %s", truncate(string(raw), 1500))
		}
	case "INTERACTION_CREATE":
		var interaction struct {
			Type int                        `json:"type"`
			Data map[string]json.RawMessage `json:"data"`
		}
		if json.Unmarshal(e.RawData, &interaction) != nil || interaction.Type == int(discordgo.InteractionApplicationCommand) {
			return
		}
		log.Printf("Debug: interaction of type %d, data fields: %s", interaction.Type, strings.Join(keysWithValue(interaction.Data), " "))
	default:
		if e.Struct != nil || e.Type == "" {
			return // an event the library knows
		}
		if !b.limiter.Allow("debug-event:"+e.Type, 5, time.Hour) {
			return
		}
		payload := truncate(string(e.RawData), 700)
		if e.Type == "VOICE_CHANNEL_STATUS_UPDATE" {
			payload = "(not logged: it has text written by users)"
		}
		log.Printf("Debug: gateway event %s: %s", e.Type, payload)
	}
}

func isEmptyJSON(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", `""`, "[]", "{}", "false", "0":
		return true
	}
	return false
}

// keysWithValue lists the fields that are set to something, in alphabetical order.
func keysWithValue(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key, value := range fields {
		if !isEmptyJSON(value) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
