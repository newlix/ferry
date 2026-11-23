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
	"time"
)

const (
	defaultListen            = "127.0.0.1:8787"
	defaultUpstreamOpenAI    = "https://api.z.ai/api/coding/paas/v4"
	defaultUpstreamResponses = "https://api.z.ai/api/v1"
	defaultUpstreamAnthropic = "https://api.z.ai/api/anthropic"
	defaultUpstreamUsage     = "https://api.z.ai"

	// defaultRetryWait: the z.ai request rate limit observed on live Max
	// plans (code 1302, 2026-11) clears within seconds to minutes; a held
	// request that outlives it beats a failed agent run. Quota exhaustion
	// (1308, 1310) is never held; see shortWindowLimit.
	defaultRetryWait = 5 * time.Minute
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
// listen, models and retryWait; keys come from the key store, upstreams are
// fixed. The runtime fields carry json:"-" so a legacy config spelling them
// is an unknown field and fails loudly instead of silently loading.
type Config struct {
	Listen string            `json:"listen"`
	Models map[string]string `json:"models"`
	// RetryWait caps how long a request is held and retried when every key
	// returned a failover status (401/403/429); 0 disables holding. See the
	// retry loop in gateway.go.
	RetryWait Duration `json:"retryWait"`
	// Route pins every request to the key named here; empty routes
	// automatically (weighted round-robin, sticky conversations, failover).
	// loadKeys resolves it into RouteIdx and fails when no stored key has
	// the name, so a stale name breaks at startup, not per request.
	Route string `json:"route"`

	// RouteIdx indexes APIKeys for fixed routing; -1 routes automatically.
	RouteIdx  int       `json:"-"`
	APIKeys   []apiKey  `json:"-"`
	Upstreams Upstreams `json:"-"`
}

// Duration is time.Duration with a JSON form: a number is seconds, a string
// is a Go duration ("5m", "300ms", "0"). encoding/json has no native
// duration encoding, and "5m" beats asking users to compute seconds.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("duration %s: %w", b, err)
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return fmt.Errorf("duration %s: %w", b, err)
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// apiKey pairs a key's store name with its secret: status and logs show the
// name, requests carry the secret.
type apiKey struct {
	Name   string
	Secret string
}

func DefaultConfig() Config {
	return Config{
		Listen:    defaultListen,
		RetryWait: Duration(defaultRetryWait),
		RouteIdx:  -1,
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
			return Config{}, fmt.Errorf("parse %s: %w (the file holds only \"listen\", \"models\", \"retryWait\" and \"route\"; keys now live in the key store: ferry key add)", path, err)
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
	if c.RetryWait < 0 {
		return errors.New("config: retryWait must not be negative (0 disables holding)")
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

// writeRoute sets route in the config file ("" clears it), keeping every
// other field's value untouched. A missing file gains one holding only
// route; clearing route on a missing file does nothing.
func writeRoute(path, route string) error {
	var doc map[string]json.RawMessage
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if route == "" {
			return nil
		}
		doc = map[string]json.RawMessage{}
	} else {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if route == "" {
		delete(doc, "route")
	} else {
		b, err := json.Marshal(route)
		if err != nil {
			return fmt.Errorf("encode route: %w", err)
		}
		doc["route"] = b
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
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
