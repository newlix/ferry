package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

	APIKeys   []apiKey  `json:"-"`
	Upstreams Upstreams `json:"-"`
}

// apiKey pairs a key's store name with its secret: status and logs show the
// name, requests carry the secret.
type apiKey struct {
	Name   string
	Secret string
}

// servedModels are the only upstream models ferry sends a request to (user 2026-11-14: 「ferry 只支援 glm-5.3 跟
// glm-5.3-flash 其他直接回傳錯誤」); a request for any other, or an alias to one, is refused.
var servedModels = []string{"glm-5.3", "glm-5.3-flash"}

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
		},
	}
}

// LoadConfig reads path on top of the defaults; fields absent from the file
// keep their defaults and the models map is merged with the defaults. A
// missing file means defaults. Unknown fields are rejected so a pre-keychain
// config fails loudly instead of silently losing its keys.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, cfg.validate()
		}
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return Config{}, fmt.Errorf("parse %s: %w (the file holds only \"listen\" and \"models\"; keys now live in the key store: ferry key add)", path, err)
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
	for alias, target := range c.Models {
		if alias == "" || target == "" {
			return errors.New("config: models must map non-empty names to non-empty models")
		}
		if !slices.Contains(servedModels, target) {
			return fmt.Errorf("config: models maps %q to %q; ferry serves only %s", alias, target, strings.Join(servedModels, " and "))
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

func configDirNamed(name string) string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return name
	}
	return filepath.Join(home, ".config", name)
}

func configDir() string { return configDirNamed("ferry") }

// defaultConfigPath follows the ~/.config convention shared with opencode,
// not the macOS Application Support location os.UserConfigDir returns.
func defaultConfigPath() string {
	return filepath.Join(configDir(), "config.json")
}
