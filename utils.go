package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getsentry/sentry-go"
)

// maxSkipSeconds bounds the timestamps people can ask for (24 hours).
const maxSkipSeconds = 24 * 60 * 60

func normalizeTrigger(s string) string {
	return strings.ToLower(replaceSlice(s, "", "'", "’", "`"))
}

func getBodyToCompare(body string) string {
	return "\n" + strings.ReplaceAll(normalizeTrigger(body), "what is", "whats") + "?"
}

func replaceSlice(s, replaceWith string, oldStrings ...string) string {
	for _, old := range oldStrings {
		s = strings.ReplaceAll(s, old, replaceWith)
	}
	return s
}

// GetTimeFromText finds a timestamp ("1:23") or an interval ("1:23-2:00", "50-70") in the text.
func GetTimeFromText(s string) (timeFrom, timeTo int) {
	s = strings.ReplaceAll(s, " - ", "-")
	maxScore := 0
	for _, w := range strings.Fields(s) {
		score := 0
		w2 := ""
		if strings.Contains(w, "-") {
			parts := strings.SplitN(w, "-", 2)
			w, w2 = parts[0], parts[1]
			score++
		}
		w = strings.TrimSuffix(w, "s")
		w2 = strings.TrimSuffix(w2, "s")
		if strings.Contains(w, ":") {
			score += 2
		}
		if score > maxScore {
			t, err := TimeStringToSeconds(w)
			if err == nil {
				timeFrom = t
				timeTo, _ = TimeStringToSeconds(w2) // if w2 is empty or not a correct time, timeTo is 0
				maxScore = score
			}
		}
	}
	return clampSeconds(timeFrom), clampSeconds(timeTo)
}

func clampSeconds(t int) int {
	if t < 0 {
		return 0
	}
	if t > maxSkipSeconds {
		return maxSkipSeconds
	}
	return t
}

func TimeStringToSeconds(s string) (int, error) {
	list := strings.Split(s, ":")
	if len(list) > 3 {
		return 0, fmt.Errorf("too many : thingies")
	}
	seconds, multiplier := 0, 1
	for i := len(list) - 1; i >= 0; i-- {
		c, err := strconv.Atoi(list[i])
		if err != nil {
			return 0, err
		}
		if c < 0 || c > maxSkipSeconds {
			return 0, fmt.Errorf("time is out of range")
		}
		seconds += c * multiplier
		multiplier *= 60
	}
	return seconds, nil
}

func SecondsToTimeString(i int, includeHours bool) string {
	if includeHours {
		return fmt.Sprintf("%02d:%02d:%02d", i/3600, (i%3600)/60, i%60)
	}
	return fmt.Sprintf("%02d:%02d", i/60, i%60)
}

// GetSkipFirstFromLink reads the start time from links like ?t=90, ?t=1m30s or ?start=1:30.
func GetSkipFirstFromLink(link string) int {
	u, err := url.Parse(link)
	if err != nil || strings.HasSuffix(u.Path, ".m3u8") {
		return 0
	}
	q := u.Query()
	t := ""
	for _, name := range []string{"t", "time_continue", "start"} {
		if t = strings.ToLower(strings.TrimSpace(q.Get(name))); t != "" {
			break
		}
	}
	if t == "" || len(t) > 16 {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSuffix(t, "s")); err == nil {
		return clampSeconds(seconds)
	}
	if strings.Contains(t, ":") {
		seconds, err := TimeStringToSeconds(t)
		if err != nil {
			return 0
		}
		return clampSeconds(seconds)
	}
	if last := t[len(t)-1]; last >= '0' && last <= '9' {
		t += "s" // 1m30 -> 1m30s
	}
	d, err := time.ParseDuration(t)
	if err != nil {
		return 0
	}
	return clampSeconds(int(d.Seconds()))
}

func stringInSlice(slice []string, s string) bool {
	for i := range slice {
		if s == slice[i] {
			return true
		}
	}
	return false
}

func substringInSlice(s string, slice []string) (exists bool, substring string) {
	for i := range slice {
		if strings.Contains(s, slice[i]) {
			return true, slice[i]
		}
	}
	return
}

// truncate shortens s to at most max runes.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return string([]rune(s)[:max-1]) + "…"
}

// validateMediaURL makes sure a link from a message is something that's okay
// to hand over for downloading: a plain http(s) link to a public host.
func validateMediaURL(raw string) (string, bool) {
	if len(raw) > 2000 || strings.ContainsAny(raw, " \t\r\n<>`\"") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || host == "localhost" || !strings.Contains(host, ".") && net.ParseIP(host) == nil {
		return "", false
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home", ".corp", ".onion"} {
		if strings.HasSuffix(host, suffix) {
			return "", false
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
			return "", false
		}
		if ip4 := ip.To4(); ip4 != nil && (ip4[0] == 0 || ip4[0] == 100 && ip4[1]&0xc0 == 64 || ip4[0] >= 240) {
			return "", false // "this network", carrier-grade NAT, reserved
		}
	}
	return u.String(), true
}

// isOwnLink reports whether the link is one of the song pages the bot posts.
func isOwnLink(link string) bool {
	u, err := url.Parse(link)
	return err == nil && strings.EqualFold(u.Hostname(), "lis.tn")
}

// cacheKeyForURL drops the signature parameters Discord adds to attachment
// links, so the same file gives the same key every time it's requested.
func cacheKeyForURL(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	host := strings.ToLower(u.Hostname())
	if host == "cdn.discordapp.com" || host == "media.discordapp.net" {
		q := u.Query()
		q.Del("ex")
		q.Del("is")
		q.Del("hm")
		u.RawQuery = q.Encode()
	}
	u.Fragment = ""
	return u.String()
}

// capture logs the error, reports it to Sentry and tells whether there was one.
func capture(err error) bool {
	if err == nil {
		return false
	}
	log.Println("error:", err)
	if !errors.Is(err, context.Canceled) {
		sentry.CaptureException(err)
	}
	return true
}

// recoverPanic keeps a panic in one handler from taking the whole bot down.
func recoverPanic(where string) {
	if r := recover(); r != nil {
		log.Printf("panic in %s: %v\n%s", where, r, debug.Stack())
		sentry.CurrentHub().Recover(r)
	}
}
