package main

import (
	"context"
	"sync"
	"time"
)

// RateLimiter is a keyed token-bucket limiter. Each key gets `limit` tokens
// that refill evenly over `per`.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
	per    time.Duration
}

const maxBuckets = 200000

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{buckets: map[string]*bucket{}, now: time.Now}
}

// Allow takes one token for the key and reports whether there was one.
// A negative limit means "no limit"; a zero limit never allows.
func (l *RateLimiter) Allow(key string, limit int, per time.Duration) bool {
	if limit < 0 {
		return true
	}
	if limit == 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			l.pruneLocked(now)
		}
		if len(l.buckets) >= maxBuckets {
			// Under a flood of distinct keys, fail closed instead of growing forever.
			return false
		}
		l.buckets[key] = &bucket{tokens: float64(limit) - 1, last: now, per: per}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * float64(limit) / per.Seconds()
	if b.tokens > float64(limit) {
		b.tokens = float64(limit)
	}
	b.last, b.per = now, per
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Prune forgets the buckets that have fully refilled.
func (l *RateLimiter) Prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(l.now())
}

func (l *RateLimiter) pruneLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) >= b.per {
			delete(l.buckets, k)
		}
	}
}

// resultCache remembers recent recognition results, so repeated requests for
// the same link don't spend API requests.
type resultCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]cacheEntry
	now     func() time.Time
}

type cacheEntry struct {
	songs []Song
	at    time.Time
}

func newResultCache(ttl time.Duration, max int) *resultCache {
	return &resultCache{ttl: ttl, max: max, entries: map[string]cacheEntry{}, now: time.Now}
}

func (c *resultCache) Get(key string) ([]Song, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.now().Sub(e.at) > c.ttl {
		delete(c.entries, key)
		return nil, false
	}
	return append([]Song(nil), e.songs...), true
}

func (c *resultCache) Put(key string, songs []Song) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= c.max {
		for k, e := range c.entries {
			if now.Sub(e.at) > c.ttl {
				delete(c.entries, k)
			}
		}
		for k := range c.entries { // still full: drop arbitrary entries
			if len(c.entries) < c.max {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[key] = cacheEntry{songs: append([]Song(nil), songs...), at: now}
}

// flightGroup makes concurrent requests for the same key share one call.
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	done  chan struct{}
	songs []Song
	err   error
}

func newFlightGroup() *flightGroup {
	return &flightGroup{calls: map[string]*flightCall{}}
}

func (g *flightGroup) Do(ctx context.Context, key string, fn func() ([]Song, error)) ([]Song, error) {
	g.mu.Lock()
	if call, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-call.done:
			return append([]Song(nil), call.songs...), call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &flightCall{done: make(chan struct{})}
	g.calls[key] = call
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		close(call.done)
	}()
	call.songs, call.err = fn()
	return append([]Song(nil), call.songs...), call.err
}
