package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/AudDMusic/audd-go"
	"github.com/Mihonarium/discordgo"
)

const maxCallbackBody = 1 << 20

// callbackSender posts the result of a callback to a channel.
type callbackSender func(channelID string, message *discordgo.MessageSend, publishAnnouncement bool) error

// startCallbackServer starts the server that receives the results for the streams
// and posts them to the channels. It's only started when a secret is configured.
func (b *Bot) startCallbackServer() *http.Server {
	if b.cfg.CallbacksAddr == "" {
		return nil
	}
	if len(b.cfg.SecretCallbackToken) < 16 {
		log.Println("The callback server is disabled: SecretCallbackToken must be at least 16 characters long")
		return nil
	}
	server := &http.Server{
		Addr:              b.cfg.CallbacksAddr,
		Handler:           b.cfg.callbackHandler(b.sendResult, b.limiter),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			capture(err)
		}
	}()
	log.Println("Listening for callbacks on", b.cfg.CallbacksAddr)
	return server
}

func isSnowflake(id string) bool {
	if len(id) < 15 || len(id) > 20 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// songFromCallback reads the song from an AudD stream callback or from a body with just the song.
func songFromCallback(body []byte) (Song, bool) {
	if match, _, err := audd.ParseCallback(body); err == nil && match != nil {
		return Song{
			Artist: match.Song.Artist, Title: match.Song.Title, Album: match.Song.Album,
			ReleaseDate: match.Song.ReleaseDate, Label: match.Song.Label, SongLink: match.Song.SongLink,
			Score: match.Song.Score,
		}, match.Song.Title != "" || match.Song.Artist != ""
	}
	var song Song
	if err := json.Unmarshal(body, &song); err != nil {
		return Song{}, false
	}
	return song, song.Title != "" || song.Artist != ""
}

func (c *BotConfig) callbackHandler(send callbackSender, limiter *RateLimiter) http.Handler {
	secret := []byte(c.SecretCallbackToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer recoverPanic("callback")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query := r.URL.Query()
		if len(secret) == 0 || subtle.ConstantTimeCompare([]byte(query.Get("secret")), secret) != 1 {
			// Slow down guessing: a few wrong attempts per minute per address.
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			if !limiter.Allow("callback-auth:"+host, 10, time.Minute) {
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		channelID := query.Get("chat_id")
		if channelID == "" {
			channelID = query.Get("chat")
		}
		if !isSnowflake(channelID) {
			http.Error(w, "incorrect chat_id", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallbackBody))
		if err != nil {
			http.Error(w, "can't read the body", http.StatusBadRequest)
			return
		}
		song, ok := songFromCallback(body)
		if !ok {
			// Notifications about the streams and results without songs are fine, there's just nothing to post.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !limiter.Allow("callback-channel:"+channelID, 30, time.Minute) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		includePlaysOn := strings.EqualFold(query.Get("includePlaysOn"), "true")
		publishAnnouncement := strings.EqualFold(query.Get("publishAnnouncement"), "true")
		message := c.getResult([]Song{cleanSong(song)}, includePlaysOn && song.Timecode != "", false, nil, c.CanCompressWithoutSlash)
		if err := send(channelID, message, publishAnnouncement); err != nil {
			log.Println("callback:", err)
			http.Error(w, "can't send the message", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
