package main

import (
	"fmt"
	"github.com/Mihonarium/discordgo"
	"github.com/getsentry/sentry-go"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"strings"
)

func getBodyToCompare(body string) string {
	return "\n" + strings.ReplaceAll(strings.ToLower(replaceSlice(body, "", "'", "’", "`")), "what is", "whats") + "?"
}
func replaceSlice(s, new string, oldStrings ...string) string {
	for _, old := range oldStrings {
		s = strings.ReplaceAll(s, old, new)
	}
	return s
}

func linksFromMessage(m *discordgo.Message) []string {
	results := make([]string, 0)
	for _, a := range m.Attachments {
		if a == nil {
			continue
		}
		if a.URL != "" {
			results = append(results, a.URL)
		}
	}
	plaintextUrls := rxStrict.FindAllString(m.Content, -1)
	for i := range plaintextUrls {
		plaintextUrls[i] = strings.ReplaceAll(plaintextUrls[i], "\\", "")
		if strings.HasPrefix(plaintextUrls[i], "/") {
			continue
		}
		results = append(results, plaintextUrls[i])
	}
	return results
}

func GetTimeFromText(s string) (int, int) {
	s = strings.ReplaceAll(s, " - ", "")
	words := strings.Split(s, " ")
	Time := 0
	TimeTo := 0
	maxScore := 0
	for _, w := range words {
		score := 0
		w2 := ""
		if strings.Contains(w, "-") {
			w2 = strings.Split(w, "-")[1]
			w = strings.Split(w, "-")[0]
			score += 1
		}
		w = strings.TrimSuffix(w, "s")
		w2 = strings.TrimSuffix(w2, "s")
		if strings.Contains(w, ":") {
			score += 2
		}
		if score > maxScore {
			t, err := TimeStringToSeconds(w)
			if err == nil {
				Time = t
				TimeTo, _ = TimeStringToSeconds(w2) // if w2 is empty or not a correct time, TimeTo is 0
				maxScore = score
			}
		}
	}
	return Time, TimeTo
}

func TimeStringToSeconds(s string) (int, error) {
	list := strings.Split(s, ":")
	if len(list) > 3 {
		return 0, fmt.Errorf("too many : thingies")
	}
	result, multiplier := 0, 1
	for i := len(list) - 1; i >= 0; i-- {
		c, err := strconv.Atoi(list[i])
		if err != nil {
			return 0, err
		}
		result += c * multiplier
		multiplier *= 60
	}
	return result, nil
}
func SecondsToTimeString(i int, includeHours bool) string {
	if includeHours {
		return fmt.Sprintf("%02d:%02d:%02d", i/3600, (i%3600)/60, i%60)
	}
	return fmt.Sprintf("%02d:%02d", i/60, i%60)
}

func GetSkipFirstFromLink(Url string) int {
	skip := 0
	if strings.HasSuffix(Url, ".m3u8") {
		return skip
	}
	u, err := url.Parse(Url)
	if err == nil {
		t := u.Query().Get("t")
		if t == "" {
			t = u.Query().Get("time_continue")
			if t == "" {
				t = u.Query().Get("start")
			}
		}
		if t != "" {
			t = strings.ToLower(strings.ReplaceAll(t, "s", ""))
			tInt := 0
			if strings.Contains(t, "m") {
				s := strings.Split(t, "m")
				tsInt, _ := strconv.Atoi(s[1])
				tInt += tsInt
				if strings.Contains(s[0], "h") {
					s := strings.Split(s[0], "h")
					if tmInt, err := strconv.Atoi(s[1]); !capture(err) {
						tInt += tmInt * 60
					}
					if thInt, err := strconv.Atoi(s[0]); !capture(err) {
						tInt += thInt * 60 * 60
					}
				} else {
					if tmInt, err := strconv.Atoi(s[0]); !capture(err) {
						tInt += tmInt * 60
					}
				}
			} else {
				if tsInt, err := strconv.Atoi(t); !capture(err) {
					tInt = tsInt
				}
			}
			skip += tInt
			fmt.Println("skip:", skip)
		}
	}
	return skip
}

func stringInSlice(slice []string, s string) bool {
	for i := range slice {
		if s == slice[i] {
			return true
		}
	}
	return false
}

func substringInSlice(s string, slice []string) (bool, string) {
	for i := range slice {
		if strings.Contains(s, slice[i]) {
			return true, slice[i]
		}
	}
	return false, ""
}

func filterFrames(frames []sentry.Frame) []sentry.Frame {
	if len(frames) == 0 {
		return nil
	}
	filteredFrames := make([]sentry.Frame, 0, len(frames))
	for _, frame := range frames {
		if frame.Module == "runtime" || frame.Module == "testing" {
			continue
		}
		if frame.Module == "main" && strings.HasPrefix(frame.Function, "capture") {
			continue
		}
		filteredFrames = append(filteredFrames, frame)
	}
	return filteredFrames
}

func capture(err error) bool {
	extractFrames := func(pcs []uintptr) []sentry.Frame {
		var frames []sentry.Frame
		callersFrames := runtime.CallersFrames(pcs)
		for {
			callerFrame, more := callersFrames.Next()

			frames = append([]sentry.Frame{
				sentry.NewFrame(callerFrame),
			}, frames...)

			if !more {
				break
			}
		}
		return frames
	}
	GetStacktrace := func() *sentry.Stacktrace {
		pcs := make([]uintptr, 100)
		n := runtime.Callers(1, pcs)
		if n == 0 {
			return nil
		}
		frames := extractFrames(pcs[:n])
		frames = filterFrames(frames)
		stacktrace := sentry.Stacktrace{
			Frames: frames,
		}
		return &stacktrace
	}
	if err == nil {
		return false
	}
	event := sentry.NewEvent()
	event.Exception = append(event.Exception, sentry.Exception{
		Value:      err.Error(),
		Type:       reflect.TypeOf(err).String(),
		Stacktrace: GetStacktrace(),
	})
	event.Level = sentry.LevelError
	hub := sentry.CurrentHub()
	client, scope := hub.Client(), hub.Scope()
	go client.CaptureEvent(event, &sentry.EventHint{OriginalException: err}, scope)
	/* _, file, no, ok := runtime.Caller(1)
	if ok {
		err = fmt.Errorf("%v from %s#%d", err, file, no)
	}
	go sentry.CaptureException(err) */
	go fmt.Println(err.Error())
	return true
}
func captureFunc(f func() error) bool {
	return capture(f())
}
