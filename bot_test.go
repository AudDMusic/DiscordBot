package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AudDMusic/audd-go"
	"github.com/Mihonarium/discordgo"
)

func TestTimeParsing(t *testing.T) {
	for _, tc := range []struct {
		text     string
		from, to int
	}{
		{"!song", 0, 0},
		{"!song 1:25", 85, 0},
		{"!song 1:25-2:00", 85, 120},
		{"!song 1:25 - 2:00", 85, 120},
		{"!song 50-70", 50, 70},
		{"!song 1:02:03", 3723, 0},
		{"whats the song at 30s", 0, 0},
		{"!song 99999999999999999999:00", 0, 0},
		{"!song -5:00", 0, 0},
		{"!song 999999:00", 0, 0},
		{"artist - title", 0, 0},
	} {
		if from, to := GetTimeFromText(tc.text); from != tc.from || to != tc.to {
			t.Errorf("GetTimeFromText(%q) = %d, %d; want %d, %d", tc.text, from, to, tc.from, tc.to)
		}
	}
	for link, want := range map[string]int{
		"https://youtu.be/abc?t=90":                    90,
		"https://youtu.be/abc?t=90s":                   90,
		"https://youtu.be/abc?t=1m30s":                 90,
		"https://youtu.be/abc?t=1m30":                  90,
		"https://youtu.be/abc?t=1h2m3s":                3723,
		"https://example.com/v?start=1:30":             90,
		"https://example.com/v?time_continue=15":       15,
		"https://example.com/v?t=-5":                   0,
		"https://example.com/v?t=99999999999":          maxSkipSeconds,
		"https://example.com/v?t=abc":                  0,
		"https://example.com/live.m3u8?t=50":           0,
		"https://cdn.discordapp.com/attachments/1/2/a": 0,
		"::not a url::":                                0,
	} {
		if got := GetSkipFirstFromLink(link); got != want {
			t.Errorf("GetSkipFirstFromLink(%q) = %d; want %d", link, got, want)
		}
	}
	if got := SecondsToTimeString(3723, true); got != "01:02:03" {
		t.Errorf("SecondsToTimeString = %q", got)
	}
}

func TestTriggers(t *testing.T) {
	cfg := &BotConfig{
		DiscordToken: "x", AudDToken: "x",
		Triggers:           []string{"What's the song", "!song", "\nsong id?"},
		LegacyAntiTriggers: []string{"whats the song about"},
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	match := func(text string) bool {
		compare := getBodyToCompare(text)
		triggered, _ := substringInSlice(compare, cfg.Triggers)
		avoid, _ := substringInSlice(compare, cfg.AntiTriggers)
		return triggered && !avoid
	}
	for text, want := range map[string]bool{
		"What is the song":          true,
		"what’s the song?":          true,
		"WHAT'S THE SONG":           true,
		"!song 1:20":                true,
		"song id":                   true,
		"this song id is cool":      false,
		"what's the song about?":    false,
		"what is the weather today": false,
	} {
		if got := match(text); got != want {
			t.Errorf("trigger match for %q = %v; want %v", text, got, want)
		}
	}
}

func TestValidateMediaURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://cdn.discordapp.com/attachments/1/2/song.mp3?ex=1&is=2&hm=3": true,
		"http://example.com/a.mp4":                       true,
		"https://8.8.8.8/a.mp3":                          true,
		"file:///etc/passwd":                             false,
		"/etc/passwd":                                    false,
		"config.json":                                    false,
		"ftp://example.com/a.mp3":                        false,
		"https://localhost/a.mp3":                        false,
		"https://intranet/a.mp3":                         false,
		"https://printer.local/a.mp3":                    false,
		"http://127.0.0.1:4541/?secret=a":                false,
		"http://10.0.0.5/a.mp3":                          false,
		"http://192.168.1.1/a.mp3":                       false,
		"http://169.254.169.254/latest":                  false,
		"http://100.64.0.1/a.mp3":                        false,
		"http://[::1]/a.mp3":                             false,
		"http://[fd00::1]/a.mp3":                         false,
		"http://0.0.0.0/a.mp3":                           false,
		"https://user:pass@example.com/a":                false,
		"https://example.com/a b":                        false,
		"https://example.com/@everyone>":                 false,
		"https://" + strings.Repeat("a", 2100) + ".com/": false,
	} {
		if _, ok := validateMediaURL(raw); ok != want {
			t.Errorf("validateMediaURL(%q) = %v; want %v", raw, ok, want)
		}
	}
	a := cacheKeyForURL("https://cdn.discordapp.com/attachments/1/2/song.mp3?ex=aa&is=bb&hm=cc")
	b := cacheKeyForURL("https://cdn.discordapp.com/attachments/1/2/song.mp3?ex=dd&is=ee&hm=ff")
	if a != b {
		t.Errorf("signed links to the same attachment have different cache keys: %q, %q", a, b)
	}
	if !isOwnLink("https://lis.tn/abc?t=5") || isOwnLink("https://example.com/https://lis.tn/") {
		t.Error("isOwnLink is wrong")
	}
}

func TestLinksFromMessage(t *testing.T) {
	m := &discordgo.Message{
		Content: "look https://example.com/video and file:///etc/passwd and http://127.0.0.1/x",
		Attachments: []*discordgo.MessageAttachment{
			{URL: "https://cdn.discordapp.com/attachments/1/2/pic.png", ContentType: "image/png"},
			{URL: "https://cdn.discordapp.com/attachments/1/2/doc.bin"},
			{URL: "https://cdn.discordapp.com/attachments/1/2/clip.mp4", ContentType: "video/mp4"},
		},
		Embeds: []*discordgo.MessageEmbed{{Video: &discordgo.MessageEmbedVideo{URL: "https://example.com/embed.mp4"}}},
	}
	got := linksFromMessage(m)
	want := []string{
		"https://cdn.discordapp.com/attachments/1/2/clip.mp4",
		"https://cdn.discordapp.com/attachments/1/2/doc.bin",
		"https://example.com/video",
		"https://example.com/embed.mp4",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("linksFromMessage = %v; want %v", got, want)
	}
	forwarded := &discordgo.Message{MessageSnapshots: []discordgo.MessageSnapshot{{Message: m}}}
	if links := linksFromMessage(forwarded); len(links) != 4 {
		t.Errorf("links from a forwarded message = %v", links)
	}
	// The reply chain is followed through the message Discord already included, without any requests.
	bot := &Bot{cfg: &BotConfig{MaxReplyDepth: 3}}
	reply := &discordgo.Message{
		Content:           "!song",
		MessageReference:  &discordgo.MessageReference{MessageID: "1", ChannelID: "2"},
		ReferencedMessage: m,
	}
	link, err := bot.findMediaURL(nil, reply)
	if err != nil || link != want[0] {
		t.Errorf("findMediaURL = %q, %v", link, err)
	}
	bot.cfg.MaxReplyDepth = 0
	if link, _ := bot.findMediaURL(nil, reply); link != "" {
		t.Errorf("findMediaURL ignored MaxReplyDepth: %q", link)
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewRateLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.Allow("u", 3, time.Minute) {
			t.Fatalf("request %d was denied", i)
		}
	}
	if l.Allow("u", 3, time.Minute) {
		t.Fatal("the 4th request was allowed")
	}
	if !l.Allow("other", 3, time.Minute) {
		t.Fatal("the limit leaked to another key")
	}
	now = now.Add(20 * time.Second) // one token refills every 20 seconds
	if !l.Allow("u", 3, time.Minute) || l.Allow("u", 3, time.Minute) {
		t.Fatal("the refill is wrong")
	}
	if !l.Allow("u", -1, time.Minute) || l.Allow("u", 0, time.Minute) {
		t.Fatal("negative means unlimited, zero means never")
	}
	now = now.Add(time.Hour)
	l.Prune()
	if len(l.buckets) != 0 {
		t.Fatalf("%d buckets are left after pruning", len(l.buckets))
	}
}

func TestAdmit(t *testing.T) {
	cfg := &BotConfig{BlockedUserIDs: []string{"bad"}, RateLimits: RateLimitConfig{UserPerMinute: 2, GuildPerMinute: 3}}
	cfg.applyDefaults()
	bot := newBot(cfg)
	if ok, reply := bot.admit("bad", "g"); ok || reply != "" {
		t.Error("a blocked user must be silently ignored")
	}
	bot.admit("u1", "g")
	bot.admit("u1", "g")
	if ok, reply := bot.admit("u1", "g"); ok || reply != replyUserLimited {
		t.Errorf("the user limit didn't work: %v %q", ok, reply)
	}
	bot.admit("u2", "g")
	if ok, reply := bot.admit("u3", "g"); ok || reply != replyGuildLimited {
		t.Errorf("the server limit didn't work: %v %q", ok, reply)
	}
	if ok, _ := bot.admit("u4", ""); !ok {
		t.Error("a DM request was denied")
	}
}

func TestCacheAndFlights(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newResultCache(time.Minute, 2)
	c.now = func() time.Time { return now }
	c.Put("a", []Song{{Title: "A"}})
	c.Put("b", nil)
	if songs, ok := c.Get("a"); !ok || songs[0].Title != "A" {
		t.Fatal("cache miss")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("an empty result must be cached too")
	}
	c.Put("c", nil)
	if len(c.entries) > 2 {
		t.Fatal("the cache grew over its limit")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.Get("c"); ok {
		t.Fatal("an expired entry was returned")
	}

	g := newFlightGroup()
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			songs, err := g.Do(context.Background(), "k", func() ([]Song, error) {
				calls.Add(1)
				<-release
				return []Song{{Title: "T"}}, nil
			})
			if err != nil || len(songs) != 1 {
				t.Error("wrong shared result")
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("the function ran %d times", calls.Load())
	}
}

func TestRender(t *testing.T) {
	cfg := &BotConfig{UncompressedLimit: 2}
	evil := cleanSong(Song{
		Title: "@everyone [click](https://evil.example) `x` " + strings.Repeat("long ", 200), Artist: "*Artist*_",
		Album: "Al`bum", Label: "La`bel", ReleaseDate: "2020-01-01", Timecode: "01:05", SongLink: "javascript:alert(1)", Score: 90,
	})
	if evil.SongLink != "" {
		t.Error("a non-http song link was kept")
	}
	message := cfg.getResult([]Song{evil}, true, true, nil, false)
	if message.AllowedMentions == nil || len(message.AllowedMentions.Parse) != 0 {
		t.Error("mentions must be disabled")
	}
	embed := message.Embeds[0]
	if utf8.RuneCountInString(embed.Title) > 256 || strings.Contains(embed.Title, "[click](") {
		t.Errorf("the title isn't bounded or escaped: %q", embed.Title)
	}
	if !strings.HasPrefix(embed.URL, "https://www.youtube.com/results?search_query=") {
		t.Errorf("wrong fallback link: %q", embed.URL)
	}
	if embed.Description != `By **\*Artist\*\_**` {
		t.Errorf("the artist isn't escaped: %q", embed.Description)
	}

	song := cleanSong(Song{Title: "Song", Artist: "Artist", Timecode: "01:05", SongLink: "https://lis.tn/abc", Score: 100})
	message = cfg.getResult([]Song{song}, true, true, nil, false)
	if message.Embeds[0].URL != "https://lis.tn/abc?t=65" || message.Embeds[0].Image.URL != "https://lis.tn/abc?thumb" {
		t.Errorf("wrong link or image: %+v", message.Embeds[0])
	}

	many := make([]Song, 30)
	for i := range many {
		many[i] = cleanSong(Song{Title: strings.Repeat("T", 300), Artist: strings.Repeat("A", 300), Album: strings.Repeat("B", 300),
			Label: strings.Repeat("L", 300), ReleaseDate: "2020-01-01", Timecode: "00:10", SongLink: "https://lis.tn/abc", Score: 80})
	}
	base := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{footerEmbed(80)}}
	message = cfg.getResult(many, true, true, base, false)
	if len(message.Embeds) > 10 {
		t.Errorf("%d embeds in a message; Discord allows 10", len(message.Embeds))
	}
	total := 0
	for _, e := range message.Embeds {
		total += utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
		for _, f := range e.Fields {
			total += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
		}
	}
	if total > 6000 {
		t.Errorf("%d characters in the embeds; Discord allows 6000", total)
	}
	message = cfg.getResult(many, true, true, nil, true)
	if n := utf8.RuneCountInString(message.Content); n > maxContentLength || len(message.Embeds) != 0 {
		t.Errorf("the compressed message has %d characters and %d embeds", n, len(message.Embeds))
	}
}

func TestSongsFromMatches(t *testing.T) {
	songs, highest := songsFromMatches([]audd.EnterpriseMatch{
		{Score: 100, Title: "A", Artist: "X", SongLink: "https://lis.tn/a"},
		{Score: 90, Title: "A again", Artist: "X", SongLink: "https://lis.tn/a"},
		{Score: 50, Title: "Low", Artist: "X", SongLink: "https://lis.tn/low"},
		{Score: 80, Title: "", Artist: ""},
		{Score: 70, Title: "Stage", Artist: "?", SongLink: "https://lis.tn/rvXTou"},
	}, 65)
	if len(songs) != 2 || highest != 100 {
		t.Fatalf("got %d songs with the highest score %d", len(songs), highest)
	}
	if songs[1].Artist != "The Caretaker (Leyland James Kirby)" {
		t.Errorf("unexpected second song: %+v", songs[1])
	}
}

// roundTripper sends the API requests of the SDK to a function.
type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRecognizeFromURL(t *testing.T) {
	var requests atomic.Int32
	var form url.Values
	response := `{"status":"success","result":[{"songs":[{"artist":"Imagine Dragons","title":"Warriors","album":"Warriors",
		"release_date":"2014-09-18","label":"Universal Music","score":100,"timecode":"00:40","song_link":"https://lis.tn/Warriors"}],"offset":"00:24"}]}`
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			_ = r.ParseForm()
		}
		form = r.Form
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})}
	cfg := &BotConfig{MinScore: 65, UncompressedLimit: 2}
	cfg.applyDefaults()
	bot := newBot(cfg)
	bot.audd = audd.NewClient("token", audd.WithHTTPClient(client))

	link := "https://cdn.discordapp.com/attachments/1/2/clip.mp4?ex=1&is=2&hm=3"
	message := bot.recognizeFromURL(context.Background(), link, "!song 1:00", nil, false)
	if len(message.Embeds) != 2 || message.Embeds[0].Title != "Warriors" {
		t.Fatalf("unexpected message: %+v", message)
	}
	if form.Get("url") != link || form.Get("skip_first_seconds") != "60" || form.Get("limit") != "2" ||
		form.Get("accurate_offsets") != "true" || form.Get("api_token") != "token" {
		t.Errorf("unexpected API request: %v", form)
	}
	// The same file (with a re-signed link) is answered from the cache.
	bot.recognizeFromURL(context.Background(), strings.Replace(link, "ex=1", "ex=9", 1), "!song 1:00", nil, false)
	if requests.Load() != 1 {
		t.Errorf("%d API requests were made; the second one must have been cached", requests.Load())
	}
	bot.recognizeFromURL(context.Background(), link, "!song at the end", nil, false)
	if requests.Load() != 2 || form.Get("reversed_order") != "true" || form.Get("skip_first_seconds") != "0" {
		t.Errorf("unexpected 'at the end' request: %v", form)
	}

	response = `{"status":"success","result":[]}`
	message = bot.recognizeFromURL(context.Background(), "https://example.com/other.mp3", "!song", nil, false)
	if !strings.Contains(message.Content, "couldn't recognize the song") || !strings.Contains(message.Content, "<https://example.com/other.mp3> at 00:00-00:24") {
		t.Errorf("unexpected no-result message: %q", message.Content)
	}
	response = `{"status":"error","error":{"error_code":501,"error_message":"can't get the file"}}`
	message = bot.recognizeFromURL(context.Background(), "https://example.com/broken.mp3", "!song", nil, false)
	if !strings.Contains(message.Content, "couldn't get any audio") {
		t.Errorf("unexpected no-audio message: %q", message.Content)
	}
	response = `{"status":"error","error":{"error_code":901,"error_message":"no api_token passed"}}`
	message = bot.recognizeFromURL(context.Background(), "https://example.com/denied.mp3", "!song", nil, false)
	if message.Content != replyUnavailable {
		t.Errorf("unexpected message for an API-side problem: %q", message.Content)
	}

	// A voice recording is uploaded as a file.
	response = `{"status":"success","result":[]}`
	message = bot.recognizeFromAudio(context.Background(), encodeWAV(make([]int16, 48000), 48000), nil, true)
	if message.Content != "Sorry, I couldn't recognize the song." || form.Get("limit") != "1" || form.Get("url") != "" {
		t.Errorf("unexpected result for a recording: %q %v", message.Content, form)
	}
}

func TestCallbackHandler(t *testing.T) {
	cfg := &BotConfig{SecretCallbackToken: "0123456789abcdef-secret", UncompressedLimit: 2}
	type sent struct {
		channel string
		message *discordgo.MessageSend
		publish bool
	}
	var got []sent
	handler := cfg.callbackHandler(func(channelID string, message *discordgo.MessageSend, publish bool) error {
		got = append(got, sent{channelID, message, publish})
		return nil
	}, NewRateLimiter())
	do := func(method, query, body string) int {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/?"+query, strings.NewReader(body)))
		return rec.Code
	}
	stream := `{"status":"success","result":{"radio_id":7,"timestamp":"2024-01-01 10:00:00","play_length":111,
		"results":[{"artist":"Artist","title":"Title","score":100,"song_link":"https://lis.tn/abc"}]}}`
	flat := `{"artist":"Artist","title":"Flat","timecode":"00:30","song_link":"https://lis.tn/abc"}`
	secret := "secret=0123456789abcdef-secret"

	if code := do("POST", "secret=wrong&chat_id=123456789012345678", stream); code != http.StatusForbidden {
		t.Errorf("a wrong secret got %d", code)
	}
	if code := do("POST", "chat_id=123456789012345678", stream); code != http.StatusForbidden {
		t.Errorf("a missing secret got %d", code)
	}
	if code := do("GET", secret+"&chat_id=123456789012345678", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET got %d", code)
	}
	if code := do("POST", secret+"&chat_id=../../webhooks/1", stream); code != http.StatusBadRequest {
		t.Errorf("an incorrect chat_id got %d", code)
	}
	if code := do("POST", secret+"&chat_id=123456789012345678", strings.Repeat("a", maxCallbackBody+10)); code != http.StatusBadRequest {
		t.Errorf("a huge body got %d", code)
	}
	if code := do("POST", secret+"&chat_id=123456789012345678", `{"status":"success","notification":{"radio_id":7,"notification_code":650,"notification_message":"x"},"time":1}`); code != http.StatusNoContent {
		t.Errorf("a notification got %d", code)
	}
	if len(got) != 0 {
		t.Fatalf("%d messages were sent for the requests that must be rejected or ignored", len(got))
	}
	if code := do("POST", secret+"&chat_id=123456789012345678&publishAnnouncement=true", stream); code != http.StatusNoContent {
		t.Errorf("a stream callback got %d", code)
	}
	if code := do("POST", secret+"&chat_id=123456789012345678&includePlaysOn=true", flat); code != http.StatusNoContent {
		t.Errorf("a flat callback got %d", code)
	}
	if len(got) != 2 || !got[0].publish || got[1].publish || got[0].channel != "123456789012345678" {
		t.Fatalf("unexpected sent messages: %+v", got)
	}
	if got[0].message.Embeds[0].Title != "Title" || got[1].message.Embeds[0].Title != "Flat" || len(got[1].message.Embeds[0].Fields) != 1 {
		t.Errorf("unexpected embeds: %+v, %+v", got[0].message.Embeds[0], got[1].message.Embeds[0])
	}

	// No secret in the config: nothing gets through, whatever is sent.
	open := (&BotConfig{}).callbackHandler(func(string, *discordgo.MessageSend, bool) error {
		t.Error("a message was sent without a secret configured")
		return nil
	}, NewRateLimiter())
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest("POST", "/?secret=&chat_id=123456789012345678", strings.NewReader(flat)))
	if rec.Code != http.StatusForbidden {
		t.Errorf("a request to a handler without a secret got %d", rec.Code)
	}
}

func TestVoiceBuffer(t *testing.T) {
	now := time.Now()
	frame := func(v int16) []int16 {
		f := make([]int16, 960)
		for i := range f {
			f[i] = v
		}
		return f
	}
	st := &pcmStream{}
	st.write(1000, frame(1), now)
	st.write(1000+960, frame(2), now)
	st.write(1000, frame(9), now) // duplicate: ignored
	if len(st.samples) != 1920 {
		t.Fatalf("%d samples after two frames and a duplicate", len(st.samples))
	}
	st.write(1000+960*4, frame(3), now) // two frames of silence are filled in
	if len(st.samples) != 960*5 || st.samples[1920] != 0 || st.samples[960*4] != 3 {
		t.Fatalf("the gap wasn't filled with silence: %d samples", len(st.samples))
	}
	ts := uint32(1000 + 960*5)
	for i := 0; i < 2000; i++ { // 40 seconds
		st.write(ts, frame(4), now)
		ts += 960
	}
	if len(st.samples) > 2*maxRingSamples {
		t.Fatalf("the buffer grew to %d samples", len(st.samples))
	}
	if got := st.snapshot(now); len(got) != maxRingSamples {
		t.Fatalf("the snapshot has %d samples; want %d", len(got), maxRingSamples)
	}
	if st.snapshot(now.Add(time.Minute)) != nil {
		t.Fatal("stale audio was returned")
	}
	st.write(ts+maxRingSamples*3, frame(5), now) // a long silence resets the buffer
	if len(st.samples) != 960 {
		t.Fatalf("%d samples after a long gap", len(st.samples))
	}

	if mono := stereoToMono([]int16{100, 300, -32768, -32768, 32767, 32767}); mono[0] != 200 || mono[1] != -32768 || mono[2] != 32767 {
		t.Errorf("stereoToMono = %v", mono)
	}
	wav := encodeWAV([]int16{1, -2, 3}, 48000)
	if len(wav) != 50 || string(wav[:4]) != "RIFF" || string(wav[8:16]) != "WAVEfmt " ||
		binary.LittleEndian.Uint32(wav[24:]) != 48000 || binary.LittleEndian.Uint32(wav[40:]) != 6 ||
		!bytes.Equal(wav[44:], []byte{1, 0, 0xfe, 0xff, 3, 0}) {
		t.Errorf("unexpected WAV: %v", wav)
	}

	// The audio is attributed through the speaking updates.
	vs := &voiceSession{done: make(chan struct{}), ssrcUser: map[uint32]string{}, streams: map[uint32]*pcmStream{}}
	vs.streams[7] = &pcmStream{}
	vs.streams[7].write(0, frame(1), time.Now())
	vs.streams[8] = &pcmStream{}
	vs.streams[8].write(0, frame(2), time.Now())
	vs.onSpeaking(nil, &discordgo.VoiceSpeakingUpdate{UserID: "music-bot", SSRC: 8})
	if samples, _ := vs.audioOf("music-bot"); len(samples) != 960 || samples[0] != 2 {
		t.Errorf("wrong audio for the speaker")
	}
	if samples, _ := vs.audioOf("someone-else"); samples != nil {
		t.Errorf("audio was returned for a user who isn't speaking")
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	write := func(v map[string]any) {
		raw, _ := json.Marshal(v)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{"AudDToken": "a", "DiscordToken": "Bot d", "Triggers": []string{"!Song"},
		"AntiTriggers": []string{"x"}, "AudDTAntiTriggers": []string{"y"}, "MaxReplyDepth": 3})
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DiscordToken != "d" || cfg.Triggers[0] != "!song" || len(cfg.AntiTriggers) != 2 ||
		cfg.RateLimits.UserPerMinute != 5 || cfg.VoiceEnabled || cfg.ResultCacheMinutes != 10 {
		t.Errorf("unexpected config: %+v", cfg)
	}
	write(map[string]any{"AudDToken": "a", "DiscordToken": ""})
	if _, err := loadConfig(path); err == nil {
		t.Error("a config without a Discord token was accepted")
	}
	t.Setenv("DISCORD_TOKEN", "from-env")
	if cfg, err := loadConfig(path); err != nil || cfg.DiscordToken != "from-env" {
		t.Errorf("the token wasn't taken from the environment: %v", err)
	}
	write(map[string]any{"AudDToken": "a", "DiscordToken": "d", "Triggers": []string{""}})
	if _, err := loadConfig(path); err == nil {
		t.Error("an empty trigger was accepted")
	}
	// The example config must stay loadable (once the tokens are set).
	t.Setenv("AUDD_API_TOKEN", "a")
	if _, err := loadConfig("config.example.json"); err != nil {
		t.Errorf("config.example.json: %v", err)
	}
}

func TestCommands(t *testing.T) {
	names := func(voice bool) string {
		bot := &Bot{cfg: &BotConfig{VoiceEnabled: voice}}
		var list []string
		for _, c := range bot.applicationCommands() {
			list = append(list, c.Name)
			if c.Type == discordgo.ChatApplicationCommand && (c.Description == "" || len(c.Description) > 100) {
				t.Errorf("the description of /%s must have 1-100 characters", c.Name)
			}
		}
		if n := utf8.RuneCountInString(bot.helpText()); n > maxContentLength {
			t.Errorf("the help text has %d characters", n)
		}
		return strings.Join(list, ",")
	}
	if got := names(false); got != "help,privacy,Recognize This Song" {
		t.Errorf("commands without voice: %s", got)
	}
	if got := names(true); got != "help,privacy,Recognize This Song,song-vc,listen,disconnect" {
		t.Errorf("commands with voice: %s", got)
	}
}

// TestLiveRecognition calls the real API. It only runs when AUDD_LIVE_TEST_TOKEN is set.
func TestLiveRecognition(t *testing.T) {
	token := os.Getenv("AUDD_LIVE_TEST_TOKEN")
	if token == "" {
		t.Skip("AUDD_LIVE_TEST_TOKEN isn't set")
	}
	cfg := &BotConfig{AudDToken: token, MinScore: 65, UncompressedLimit: 2}
	cfg.applyDefaults()
	bot := newBot(cfg)
	message := bot.recognizeFromURL(context.Background(), "https://audd.tech/example.mp3", "!song", nil, false)
	if len(message.Embeds) < 2 {
		t.Fatalf("no songs were recognized: %q", message.Content)
	}
	t.Logf("recognized %q (%s)", message.Embeds[0].Title, message.Embeds[0].Description)
}

func TestOptOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "optouts.json")
	store, err := loadOptOuts(path)
	if err != nil || store.Has("1") {
		t.Fatalf("a new store: %v", err)
	}
	if err := store.Set("111111111111111111", true); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("222222222222222222", true); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("222222222222222222", false); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadOptOuts(path) // the choice survives a restart
	if err != nil || !reloaded.Has("111111111111111111") || reloaded.Has("222222222222222222") {
		t.Fatalf("the reloaded store is wrong: %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != `["111111111111111111"]` {
		t.Errorf("the file has more than the IDs: %s", raw)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("the file is readable by others: %v", info.Mode())
	}
	if (*optOutStore)(nil).Has("1") {
		t.Error("a missing store must not report anyone")
	}
	// A list that can't be read must stop the bot from starting instead of being treated as empty.
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptOuts(path); err == nil {
		t.Error("a broken opt-out list was accepted")
	}

	// Nobody can make the bot process a message of someone who opted out.
	bot := &Bot{cfg: &BotConfig{MaxReplyDepth: 3}, optOuts: reloaded}
	theirs := &discordgo.Message{
		Author:      &discordgo.User{ID: "111111111111111111"},
		Attachments: []*discordgo.MessageAttachment{{URL: "https://cdn.discordapp.com/attachments/1/2/clip.mp4", ContentType: "video/mp4"}},
	}
	request := &discordgo.Message{
		Author: &discordgo.User{ID: "333333333333333333"}, Content: "!song",
		MessageReference: &discordgo.MessageReference{MessageID: "1"}, ReferencedMessage: theirs,
	}
	if link, err := bot.findMediaURL(nil, request); !errors.Is(err, errOptedOut) || link != "" {
		t.Errorf("a message of an opted-out user was processed: %q, %v", link, err)
	}
	theirs.Author.ID = "222222222222222222" // opted back in
	if link, err := bot.findMediaURL(nil, request); err != nil || link == "" {
		t.Errorf("a message of a user who opted back in wasn't processed: %v", err)
	}

	// The audio of someone who opted out isn't kept in a voice session.
	vs := &voiceSession{done: make(chan struct{}), ssrcUser: map[uint32]string{5: "111111111111111111"},
		streams: map[uint32]*pcmStream{5: {}}, skipUser: reloaded.Has}
	vs.handlePacket(&discordgo.Packet{SSRC: 5, Opus: []byte{0xf8, 0xff, 0xfe}})
	if len(vs.streams) != 0 {
		t.Error("audio of an opted-out user was kept")
	}
}
