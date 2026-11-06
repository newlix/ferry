package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadConfigOverlayAndEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{
		"listen": "127.0.0.1:9999",
		"listenKey": "sekret",
		"apiKeys": ["k1", "k2"],
		"models": {"fast": "glm-5.3-flash"}
	}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envAPIKeys, "") // absent

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q, want 127.0.0.1:9999", cfg.Listen)
	}
	if cfg.ListenKey != "sekret" {
		t.Errorf("ListenKey = %q, want sekret", cfg.ListenKey)
	}
	if !slices.Equal(cfg.APIKeys, []string{"k1", "k2"}) {
		t.Errorf("APIKeys = %v, want [k1 k2]", cfg.APIKeys)
	}
	if cfg.Upstreams.OpenAIChat != defaultUpstreamOpenAI {
		t.Errorf("OpenAIChat = %q, want default %q", cfg.Upstreams.OpenAIChat, defaultUpstreamOpenAI)
	}
	for alias, target := range map[string]string{
		"fast":    "glm-5.3-flash",
		"glm-5.3": "glm-5.3",
	} {
		if cfg.Models[alias] != target {
			t.Errorf("Models[%q] = %q, want %q", alias, cfg.Models[alias], target)
		}
	}
}

func TestLoadConfigEnvKeysReplaceFileAndTrim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{"apiKeys": ["file-key"]}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envAPIKeys, " k1 , k2,, ")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !slices.Equal(cfg.APIKeys, []string{"k1", "k2"}) {
		t.Errorf("APIKeys = %v, want [k1 k2]", cfg.APIKeys)
	}
}

func TestLoadConfigMissingFileWithEnvUsesDefaults(t *testing.T) {
	t.Setenv(envAPIKeys, "k1")
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !slices.Equal(cfg.APIKeys, []string{"k1"}) {
		t.Errorf("APIKeys = %v, want [k1]", cfg.APIKeys)
	}
	if cfg.Listen != defaultListen {
		t.Errorf("Listen = %q, want default %q", cfg.Listen, defaultListen)
	}
}

func TestLoadConfigMissingFileWithoutEnvFails(t *testing.T) {
	t.Setenv(envAPIKeys, "")
	_, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("LoadConfig on missing file without env keys, want error")
	}
}

func TestLoadConfigInvalidUpstream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{"upstreams": {"anthropic": "not a url"}}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envAPIKeys, "k1")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig with invalid upstream URL, want error")
	}
}

func TestValidateTrimsEmptyKeys(t *testing.T) {
	cfg := DefaultConfig()
	cfg.APIKeys = []string{" k1 ", "  ", "k2"}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !slices.Equal(cfg.APIKeys, []string{"k1", "k2"}) {
		t.Errorf("APIKeys = %v, want [k1 k2]", cfg.APIKeys)
	}
}
