package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	defaultListen            = "127.0.0.1:8787"
	defaultUpstreamOpenAI    = "https://api.z.ai/api/coding/paas/v4"
	defaultUpstreamResponses = "https://api.z.ai/api/v1"
	defaultUpstreamAnthropic = "https://api.z.ai/api/anthropic"
	defaultUpstreamUsage     = "https://api.z.ai"
)

// A coding-plan key is accepted only on the coding endpoints below, not on
// the general API endpoint https://api.z.ai/api/paas/v4.
type Upstreams struct {
	OpenAIChat      string
	OpenAIResponses string
	Anthropic       string
	// Usage is the console base the quota/subscription endpoints live on.
	Usage string
}

// Config is the on-disk config file plus runtime-only fields. The file holds
// just listen and models; keys come from the key store, upstreams are fixed.
// The runtime fields carry json:"-" so a legacy config spelling them is an
// unknown field and fails loudly instead of silently loading.
type Config struct {
	Listen string            `json:"listen"`
	Models map[string]string `json:"models"`

	APIKeys   []string  `json:"-"`
	Upstreams Upstreams `json:"-"`
}

func DefaultConfig() Config {
	return Config{
		Listen: defaultListen,
		Upstreams: Upstreams{
			OpenAIChat:      defaultUpstreamOpenAI,
			OpenAIResponses: defaultUpstreamResponses,
			Anthropic:       defaultUpstreamAnthropic,
			Usage:           defaultUpstreamUsage,
		},
		Models: map[string]string{
			"glm-5.3":       "glm-5.3",
			"glm-5.3-flash": "glm-5.3-flash",
			"glm-5.2":       "glm-5.2",
		},
	}
}

// LoadConfig reads path on top of the defaults; fields absent from the file
// keep their defaults and the models map is merged with the defaults. A
// missing file is only an error when the caller required it (an explicit
// -config flag). Unknown fields are rejected so a pre-keychain config fails
// loudly instead of silently losing its keys.
func LoadConfig(path string, required bool) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return cfg, cfg.validate()
		}
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return Config{}, fmt.Errorf("parse %s: %w (the file holds only \"listen\" and \"models\"; keys now live in the key store: glms key add)", path, err)
		}
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if dec.More() {
		return Config{}, fmt.Errorf("parse %s: trailing data after JSON object", path)
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	if c.Listen == "" {
		return errors.New("config: listen must not be empty")
	}
	for name, base := range map[string]string{
		"upstreams.openaiChat":      c.Upstreams.OpenAIChat,
		"upstreams.openaiResponses": c.Upstreams.OpenAIResponses,
		"upstreams.anthropic":       c.Upstreams.Anthropic,
		"upstreams.usage":           c.Upstreams.Usage,
	} {
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: %s must be an http(s) URL, got %q", name, base)
		}
	}
	for alias, target := range c.Models {
		if alias == "" || target == "" {
			return errors.New("config: models must map non-empty names to non-empty models")
		}
	}
	return nil
}

func (c Config) ModelNames() []string {
	names := make([]string, 0, len(c.Models))
	for name := range c.Models {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func configDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "glms")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "glms"
	}
	return filepath.Join(home, ".config", "glms")
}

// defaultConfigPath follows the ~/.config convention shared with opencode,
// not the macOS Application Support location os.UserConfigDir returns.
func defaultConfigPath() string {
	return filepath.Join(configDir(), "config.json")
}
