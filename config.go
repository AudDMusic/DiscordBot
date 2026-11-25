package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// RateLimitConfig bounds how much recognition work a single user or server can
// cause. A value of 0 means "use the default"; a negative value disables that
// particular limit.
type RateLimitConfig struct {
	UserPerMinute  int `json:"UserPerMinute"`
	UserPerDay     int `json:"UserPerDay"`
	GuildPerMinute int `json:"GuildPerMinute"`
	GuildPerDay    int `json:"GuildPerDay"`
	// MaxConcurrent is the number of recognitions that may run at the same time.
	MaxConcurrent int `json:"MaxConcurrent"`
}

type BotConfig struct {
	AudDToken    string `json:"AudDToken"`    // the token from dashboard.audd.io (or the AUDD_API_TOKEN env var)
	DiscordToken string `json:"DiscordToken"` // the bot token from https://discord.com/developers/applications (or DISCORD_TOKEN)
	DiscordAppID string `json:"DiscordAppID"` // optional: detected automatically when empty

	Triggers     []string `json:"Triggers"`     // phrases the bot reacts to
	AntiTriggers []string `json:"AntiTriggers"` // phrases the bot avoids replying to
	// Older configs used this (misspelled) key for the anti-triggers; it is merged into AntiTriggers.
	LegacyAntiTriggers []string `json:"AudDTAntiTriggers,omitempty"`
	// MaxTriggerTextLength makes the bot ignore longer messages. 0 disables the check.
	MaxTriggerTextLength int `json:"MaxTriggerTextLength"`

	SecretCallbackToken string `json:"SecretCallbackToken"` // or CALLBACK_SECRET; the callback server is off when empty
	CallbacksAddr       string `json:"CallbacksAddr"`

	MaxReplyDepth           int  `json:"MaxReplyDepth"`
	MinScore                int  `json:"MinScore"`
	UncompressedLimit       int  `json:"UncompressedLimit"`       // the maximum amount of songs to post as large embeds
	CompressStartingWith    int  `json:"CompressStartingWith"`    // the first result to compress when compressing
	CanCompressWithoutSlash bool `json:"CanCompressWithoutSlash"` // whether replies to plain-text triggers can be compressed to text

	SentryDSN string `json:"SentryDSN"` // or SENTRY_DSN

	// VoiceEnabled turns on /listen, /song-vc and /disconnect. See the README:
	// Discord requires the DAVE protocol for voice, which discordgo doesn't support yet.
	VoiceEnabled bool `json:"VoiceEnabled"`
	// VoiceIdleMinutes is how long a /listen session may stay unused before the bot leaves.
	VoiceIdleMinutes int `json:"VoiceIdleMinutes"`
	// MaxVoiceSessions caps the number of voice channels the bot is connected to at once.
	MaxVoiceSessions int `json:"MaxVoiceSessions"`

	// DisableMessageContentIntent stops the bot from requesting the privileged
	// Message Content intent. Text triggers then only work in DMs and in
	// messages that mention the bot; slash and context-menu commands still work.
	DisableMessageContentIntent bool `json:"DisableMessageContentIntent"`
	// ShardCount is the number of gateway shards. 0 uses Discord's recommendation.
	ShardCount int `json:"ShardCount"`

	RateLimits RateLimitConfig `json:"RateLimits"`
	// ResultCacheMinutes is how long a result for the same link is reused. Negative disables the cache.
	ResultCacheMinutes int `json:"ResultCacheMinutes"`

	BlockedUserIDs  []string `json:"BlockedUserIDs"`
	BlockedGuildIDs []string `json:"BlockedGuildIDs"`
}

func loadConfig(file string) (*BotConfig, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var cfg BotConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("can't parse %s: %w", file, err)
	}
	cfg.applyEnv(os.Getenv)
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyEnv lets secrets come from the environment instead of the config file.
func (c *BotConfig) applyEnv(getenv func(string) string) {
	for name, target := range map[string]*string{
		"AUDD_API_TOKEN":  &c.AudDToken,
		"DISCORD_TOKEN":   &c.DiscordToken,
		"SENTRY_DSN":      &c.SentryDSN,
		"CALLBACK_SECRET": &c.SecretCallbackToken,
	} {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			*target = v
		}
	}
}

func (c *BotConfig) applyDefaults() {
	def := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	def(&c.RateLimits.UserPerMinute, 5)
	def(&c.RateLimits.UserPerDay, 150)
	def(&c.RateLimits.GuildPerMinute, 20)
	def(&c.RateLimits.GuildPerDay, 2000)
	def(&c.RateLimits.MaxConcurrent, 8)
	def(&c.VoiceIdleMinutes, 60)
	def(&c.MaxVoiceSessions, 25)
	def(&c.ResultCacheMinutes, 10)
	if c.RateLimits.MaxConcurrent < 1 {
		c.RateLimits.MaxConcurrent = 1024
	}
	c.AntiTriggers = append(c.AntiTriggers, c.LegacyAntiTriggers...)
	c.LegacyAntiTriggers = nil
	// Triggers are compared with lowercased text that has apostrophes removed.
	for i := range c.Triggers {
		c.Triggers[i] = normalizeTrigger(c.Triggers[i])
	}
	for i := range c.AntiTriggers {
		c.AntiTriggers[i] = normalizeTrigger(c.AntiTriggers[i])
	}
	c.DiscordToken = strings.TrimPrefix(strings.TrimSpace(c.DiscordToken), "Bot ")
}

func (c *BotConfig) validate() error {
	if c.DiscordToken == "" {
		return fmt.Errorf("DiscordToken is empty: set it in the config or in the DISCORD_TOKEN env var")
	}
	if c.AudDToken == "" {
		return fmt.Errorf("AudDToken is empty: set it in the config or in the AUDD_API_TOKEN env var")
	}
	if stringInSlice(c.Triggers, "") {
		return fmt.Errorf("got a config with an empty string in the triggers")
	}
	if stringInSlice(c.AntiTriggers, "") {
		return fmt.Errorf("got a config with an empty string in the anti-triggers")
	}
	if c.MaxReplyDepth < 0 || c.MaxReplyDepth > 10 {
		return fmt.Errorf("MaxReplyDepth must be between 0 and 10")
	}
	if c.ShardCount < 0 {
		return fmt.Errorf("ShardCount can't be negative")
	}
	return nil
}
