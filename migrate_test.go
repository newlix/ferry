package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMigrateConfigDirMovesGlmsDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := configDirNamed(glmsName)
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "config.json"), []byte(`{"listen": "127.0.0.1:9000"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := migrateConfigDir(); err != nil {
		t.Fatalf("migrateConfigDir: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old dir still there (stat err = %v), want moved", err)
	}
	data, err := os.ReadFile(filepath.Join(configDir(), "config.json"))
	if err != nil || string(data) != `{"listen": "127.0.0.1:9000"}` {
		t.Errorf("config.json at new dir = %q, %v; want the old content", data, err)
	}
}

func TestMigrateConfigDirNoop(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// No old dir: nothing to do, and no new dir may appear.
	if err := migrateConfigDir(); err != nil {
		t.Fatalf("migrateConfigDir without old dir: %v", err)
	}
	if _, err := os.Stat(configDir()); !os.IsNotExist(err) {
		t.Errorf("ferry dir created without a glms dir (stat err = %v)", err)
	}

	// Both dirs: the ferry dir wins, the stale glms dir is left alone.
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir(), "config.json"), []byte(`{"listen": "127.0.0.1:9999"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDirNamed(glmsName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfigDir(); err != nil {
		t.Fatalf("migrateConfigDir with both dirs: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(configDir(), "config.json"))
	if err != nil || string(data) != `{"listen": "127.0.0.1:9999"}` {
		t.Errorf("existing ferry config overwritten: %q, %v", data, err)
	}
}

// copyKeys must complete a copy interrupted after some keys moved, and leave
// a name the current store already has untouched.
func TestCopyKeysCompletesPartialCopy(t *testing.T) {
	src := &fileStore{dir: t.TempDir()}
	dst := &fileStore{dir: t.TempDir()}
	if err := src.Add("pro", "sk-pro"); err != nil {
		t.Fatal(err)
	}
	if err := src.Add("lite", "sk-lite"); err != nil {
		t.Fatal(err)
	}
	// The state of an interrupted run: pro already copied, lite not yet.
	if err := dst.Add("pro", "sk-pro"); err != nil {
		t.Fatal(err)
	}

	if err := copyKeys(src, dst); err != nil {
		t.Fatalf("copyKeys: %v", err)
	}
	secret, err := dst.Get("lite")
	if err != nil || secret != "sk-lite" {
		t.Errorf("dst.Get(lite) = %q, %v; want copied sk-lite", secret, err)
	}
	if secret, err := dst.Get("pro"); err != nil || secret != "sk-pro" {
		t.Errorf("dst.Get(pro) = %q, %v; want untouched sk-pro", secret, err)
	}
	names, _ := dst.Names()
	if !slices.Equal(names, []string{"lite", "pro"}) {
		t.Errorf("dst.Names = %v, want [lite pro]", names)
	}
}

func TestCopyKeysNoopOnEmptySrc(t *testing.T) {
	src := &fileStore{dir: t.TempDir()}
	dst := &fileStore{dir: t.TempDir()}
	if err := copyKeys(src, dst); err != nil {
		t.Fatalf("copyKeys: %v", err)
	}
	if names, _ := dst.Names(); len(names) != 0 {
		t.Errorf("dst.Names = %v, want none", names)
	}
}
