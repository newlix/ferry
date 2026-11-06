package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadConfigMissingFileGivesDefaults(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), false)
	if err != nil {
		t.Fatalf("LoadConfig optional missing file: %v", err)
	}
	if cfg.Listen != defaultListen {
		t.Errorf("Listen = %q, want default %q", cfg.Listen, defaultListen)
	}
	if cfg.Upstreams.Usage != defaultUpstreamUsage {
		t.Errorf("Upstreams.Usage = %q, want default %q", cfg.Upstreams.Usage, defaultUpstreamUsage)
	}
	if cfg.Models["glm-5.3"] != "glm-5.3" {
		t.Errorf("Models = %v, want defaults", cfg.Models)
	}
}

func TestLoadConfigExplicitPathMustExist(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), true); err == nil {
		t.Fatal("LoadConfig required missing file, want error")
	}
}

func TestLoadConfigOverridesAndMergesModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{
		"listen": "127.0.0.1:9999",
		"models": {"fast": "glm-5.3-flash"}
	}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q, want 127.0.0.1:9999", cfg.Listen)
	}
	for alias, target := range map[string]string{
		"fast":    "glm-5.3-flash",
		"glm-5.3": "glm-5.3",
	} {
		if cfg.Models[alias] != target {
			t.Errorf("Models[%q] = %q, want %q", alias, cfg.Models[alias], target)
		}
	}
	if !slices.Equal(cfg.ModelNames(), []string{"fast", "glm-5.2", "glm-5.3", "glm-5.3-flash"}) {
		t.Errorf("ModelNames = %v", cfg.ModelNames())
	}
}

func TestLoadConfigRejectsLegacyFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{"apiKeys": ["old-style"]}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfig(path, false)
	if err == nil {
		t.Fatal("config with apiKeys, want loud failure")
	}
	if !strings.Contains(err.Error(), "glms key add") {
		t.Errorf("error does not tell the user where keys live now: %v", err)
	}
}

func TestLoadConfigRejectsEmptyListen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{"listen": ""}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path, false); err == nil {
		t.Fatal("empty listen, want error")
	}
}
