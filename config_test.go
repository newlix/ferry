package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigMissingFileGivesDefaults(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadConfig optional missing file: %v", err)
	}
	if cfg.Listen != defaultListen {
		t.Errorf("Listen = %q, want default %q", cfg.Listen, defaultListen)
	}
	if cfg.Upstreams.Usage != defaultUpstreamUsage {
		t.Errorf("Upstreams.Usage = %q, want default %q", cfg.Upstreams.Usage, defaultUpstreamUsage)
	}
	if cfg.Route != "" || cfg.RouteIdx != -1 {
		t.Errorf("Route = %q, RouteIdx = %d, want automatic routing", cfg.Route, cfg.RouteIdx)
	}
}

func TestLoadConfigRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{"route": "key2"}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Route != "key2" {
		t.Errorf("Route = %q, want key2", cfg.Route)
	}
}

func TestLoadConfigOverridesListen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{"listen": "127.0.0.1:9999"}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q, want 127.0.0.1:9999", cfg.Listen)
	}
}

// A config from before aliases were removed must fail, not load with its
// aliases silently ignored.
func TestLoadConfigRejectsModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{"models": {"fast": "glm-5.3-flash"}}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), `unknown field "models"`) {
		t.Errorf("LoadConfig with models: err = %v, want unknown field \"models\"", err)
	}
}

func TestLoadConfigRejectsLegacyFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{"apiKeys": ["old-style"]}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfig(path)
	if err == nil {
		t.Fatal("config with apiKeys, want loud failure")
	}
	if !strings.Contains(err.Error(), "ferry key add") {
		t.Errorf("error does not tell the user where keys live now: %v", err)
	}
}

func TestLoadConfigRejectsEmptyListen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{"listen": ""}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("empty listen, want error")
	}
}
