package main

import (
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

	// envAPIKeys holds comma-separated coding-plan keys and replaces the
	// config file's apiKeys, so secrets can stay out of files.
	envAPIKeys = "GLMS_API_KEY"
)

// A coding-plan key is accepted only on the coding endpoints below, not on
// the general API endpoint https://api.z.ai/api/paas/v4.
type Upstreams struct {
	OpenAIChat      string `json:"openaiChat"`
	OpenAIResponses string `json:"openaiResponses"`
	Anthropic       string `json:"anthropic"`
	// Usage is the console base the quota/subscription endpoints live on
	// (https://api.z.ai, or https://open.bigmodel.cn for BigModel plans).
	Usage string `json:"usage"`
}

type Config struct {
	Listen    string            `json:"listen"`
	ListenKey string            `json:"listenKey"`
	APIKeys   []string          `json:"apiKeys"`
	Upstreams Upstreams         `json:"upstreams"`
	Models    map[string]string `json:"models"`
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
// keep their defaults and the models map is merged with the defaults.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && os.Getenv(envAPIKeys) != "" {
			if err := applyEnvKeys(&cfg); err != nil {
				return Config{}, err
			}
			return cfg, cfg.validate()
		}
		return Config{}, fmt.Errorf("load config: %w (copy config.example.json to create one, or set %s)", err, envAPIKeys)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := applyEnvKeys(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
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
	var keys []string
	for _, key := range c.APIKeys {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	c.APIKeys = keys
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

func applyEnvKeys(c *Config) error {
	raw := os.Getenv(envAPIKeys)
	if raw == "" {
		return nil
	}
	c.APIKeys = nil
	for _, part := range strings.Split(raw, ",") {
		if key := strings.TrimSpace(part); key != "" {
			c.APIKeys = append(c.APIKeys, key)
		}
	}
	return nil
}

// defaultConfigPath follows the ~/.config convention shared with opencode,
// not the macOS Application Support location os.UserConfigDir returns.
func defaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "glms", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "glms.json"
	}
	return filepath.Join(home, ".config", "glms", "config.json")
}
