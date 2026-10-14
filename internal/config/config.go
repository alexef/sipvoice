// Package config loads the sipvoice YAML configuration.
//
// Any ${VAR} in the file is replaced with the environment variable VAR.
// Secrets do not have to be in the file: if sip.password, sip.username or
// xai.api_key are empty, SIP_PASSWORD, SIP_USERNAME and XAI_API_KEY are used.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type SIP struct {
	Domain       string `yaml:"domain"`        // SIP domain used in URIs, e.g. sip.example.net
	Proxy        string `yaml:"proxy"`         // outbound proxy host (defaults to domain)
	Port         int    `yaml:"port"`          // default 5060
	LocalPort    int    `yaml:"local_port"`    // 0 = random
	Username     string `yaml:"username"`      // account user part
	AuthUsername string `yaml:"auth_username"` // digest user (defaults to username)
	Password     string `yaml:"password"`
	// PlusReplacement replaces a leading "+" in dialled numbers (e.g. "00"); empty keeps "+".
	PlusReplacement string `yaml:"plus_replacement"`
}

type XAI struct {
	APIKey       string `yaml:"api_key"`
	RealtimeURL  string `yaml:"realtime_url"`
	VoiceModel   string `yaml:"voice_model"`
	Voice        string `yaml:"voice"`
	SummaryModel string `yaml:"summary_model"`
}

type Limits struct {
	MaxCallSeconds int `yaml:"max_call_seconds"`
	RingSeconds    int `yaml:"ring_seconds"`
}

type Config struct {
	SIP       SIP      `yaml:"sip"`
	XAI       XAI      `yaml:"xai"`
	OwnerName string   `yaml:"owner_name"` // who the assistant calls on behalf of
	CallsDir  string   `yaml:"calls_dir"`
	Listen    string   `yaml:"listen"`
	Allowlist []string `yaml:"allowlist"` // numbers that may be called; empty = none
	Limits    Limits   `yaml:"limits"`
}

// DefaultPath returns $XDG_CONFIG_HOME/sipvoice/config.yaml (or ~/.config/...).
func DefaultPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "sipvoice", "config.yaml")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "sipvoice", "config.yaml")
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Expand replaces ${VAR} with the value of VAR.
func Expand(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(m string) string { return os.Getenv(m[2 : len(m)-1]) })
}

// Load reads path (a missing default file is fine) and applies defaults.
func Load(path string, explicit bool) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal([]byte(Expand(string(b))), c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
	default:
		return nil, err
	}
	c.applyDefaults()
	return c, nil
}

func or(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func (c *Config) applyDefaults() {
	c.SIP.Username = or(c.SIP.Username, os.Getenv("SIP_USERNAME"))
	c.SIP.Password = or(c.SIP.Password, os.Getenv("SIP_PASSWORD"))
	c.SIP.AuthUsername = or(c.SIP.AuthUsername, or(os.Getenv("SIP_AUTH_USERNAME"), c.SIP.Username))
	c.SIP.Domain = or(c.SIP.Domain, os.Getenv("SIP_DOMAIN"))
	c.SIP.Proxy = or(c.SIP.Proxy, c.SIP.Domain)
	if c.SIP.Port == 0 {
		c.SIP.Port = 5060
	}
	c.XAI.APIKey = or(c.XAI.APIKey, os.Getenv("XAI_API_KEY"))
	c.XAI.RealtimeURL = or(c.XAI.RealtimeURL, "wss://api.x.ai/v1/realtime")
	c.XAI.VoiceModel = or(c.XAI.VoiceModel, "grok-voice-latest")
	c.XAI.Voice = or(c.XAI.Voice, "eve")
	c.XAI.SummaryModel = or(c.XAI.SummaryModel, "grok-4-fast-non-reasoning")
	c.OwnerName = or(c.OwnerName, "the user")
	if c.CallsDir == "" {
		h, _ := os.UserHomeDir()
		c.CallsDir = filepath.Join(h, ".local", "share", "sipvoice", "calls")
	}
	c.Listen = or(c.Listen, "127.0.0.1:8765")
	if c.Limits.MaxCallSeconds <= 0 {
		c.Limits.MaxCallSeconds = 300
	}
	if c.Limits.RingSeconds <= 0 {
		c.Limits.RingSeconds = 45
	}
}

// Validate checks what is needed to place calls.
func (c *Config) Validate() error {
	var miss []string
	if c.SIP.Domain == "" {
		miss = append(miss, "sip.domain")
	}
	if c.SIP.Username == "" {
		miss = append(miss, "sip.username / SIP_USERNAME")
	}
	if c.SIP.Password == "" {
		miss = append(miss, "sip.password / SIP_PASSWORD")
	}
	if c.XAI.APIKey == "" {
		miss = append(miss, "xai.api_key / XAI_API_KEY")
	}
	if len(miss) > 0 {
		return fmt.Errorf("missing config: %s", strings.Join(miss, ", "))
	}
	return nil
}
