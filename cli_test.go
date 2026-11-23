package main

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdKeyAddSavesVerifiedKey(t *testing.T) {
	stub := newZaiUsageStub(t)
	dir := t.TempDir()
	store := &fileStore{dir: dir}
	cfg := DefaultConfig()
	cfg.Upstreams.Usage = stub.server.URL
	pinNoRunningFerry(t)
	pinServiceInstalled(t, false)

	var out bytes.Buffer
	in := strings.NewReader("sk-good\npro\n")
	if err := cmdKeyAdd(cfg, store, in, &out, false); err != nil {
		t.Fatalf("cmdKeyAdd: %v (output: %s)", err, out.String())
	}
	names, _ := store.Names()
	if len(names) != 1 || names[0] != "pro" {
		t.Fatalf("Names = %v, want [pro]", names)
	}
	secret, err := store.Get("pro")
	if err != nil || secret != "sk-good" {
		t.Errorf("Get(pro) = %q, %v; want sk-good", secret, err)
	}
	if !strings.Contains(out.String(), "Verified") {
		t.Errorf("output does not confirm verification: %s", out.String())
	}
}

func TestCmdKeyAddRejectsKeyZaiRefuses(t *testing.T) {
	stub := newZaiUsageStub(t)
	stub.rejectKeys["sk-bad"] = true
	store := &fileStore{dir: t.TempDir()}
	cfg := DefaultConfig()
	cfg.Upstreams.Usage = stub.server.URL
	pinNoRunningFerry(t)
	pinServiceInstalled(t, false)

	var out bytes.Buffer
	err := cmdKeyAdd(cfg, store, strings.NewReader("sk-bad\npro\n"), &out, false)
	if err == nil {
		t.Fatal("rejected key saved, want error")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %v, want a rejection message", err)
	}
	if names, _ := store.Names(); len(names) != 0 {
		t.Errorf("Names = %v, want none saved", names)
	}
}

func TestCmdKeyAddSavesUnverifiableKeyWithWarning(t *testing.T) {
	stub := newZaiUsageStub(t)
	stub.failKeys["sk-flaky"] = true
	store := &fileStore{dir: t.TempDir()}
	cfg := DefaultConfig()
	cfg.Upstreams.Usage = stub.server.URL
	pinNoRunningFerry(t)
	pinServiceInstalled(t, false)

	var out bytes.Buffer
	if err := cmdKeyAdd(cfg, store, strings.NewReader("sk-flaky\npro\n"), &out, false); err != nil {
		t.Fatalf("cmdKeyAdd: %v", err)
	}
	if !strings.Contains(out.String(), "Could not verify") {
		t.Errorf("output does not warn: %s", out.String())
	}
	if secret, err := store.Get("pro"); err != nil || secret != "sk-flaky" {
		t.Errorf("Get(pro) = %q, %v; want saved sk-flaky", secret, err)
	}
}

func TestCmdKeyAddRefusesDuplicateName(t *testing.T) {
	store := &fileStore{dir: t.TempDir()}
	if err := store.Add("pro", "sk-1"); err != nil {
		t.Fatal(err)
	}
	err := cmdKeyAdd(DefaultConfig(), store, strings.NewReader("sk-x\npro\n"), &bytes.Buffer{}, false)
	if err == nil {
		t.Fatal("duplicate name accepted, want error")
	}
}

func TestCmdInstallRefusesWithoutKeys(t *testing.T) {
	store := &fileStore{dir: t.TempDir()}
	err := cmdInstall(store)
	if err == nil {
		t.Fatal("install without keys accepted, want refusal")
	}
	if !strings.Contains(err.Error(), "ferry key add") {
		t.Errorf("error = %v, want it to point at ferry key add", err)
	}
}

func TestCmdRoute(t *testing.T) {
	pinNoRunningFerry(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	store := &fileStore{dir: dir}
	if err := store.Add("key1", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Add("key2", "s2"); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}

	if err := cmdRoute(cfg, store, path, nil, out); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out.String(), "automatic") {
		t.Errorf("show = %q, want the automatic line", out.String())
	}

	out.Reset()
	if err := cmdRoute(cfg, store, path, []string{"key2"}, out); err != nil {
		t.Fatalf("fix key2: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after fix: %v", err)
	}
	if loaded.Route != "key2" {
		t.Errorf("Route = %q, want key2 written to the config file", loaded.Route)
	}
	if !strings.Contains(out.String(), "key2") {
		t.Errorf("output = %q, want it to name key2", out.String())
	}

	if err := cmdRoute(cfg, store, path, []string{"gone"}, out); err == nil {
		t.Error("unknown key name accepted, want error")
	}

	out.Reset()
	if err := cmdRoute(cfg, store, path, []string{"auto"}, out); err != nil {
		t.Fatalf("auto: %v", err)
	}
	loaded, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after auto: %v", err)
	}
	if loaded.Route != "" {
		t.Errorf("Route = %q, want empty after auto", loaded.Route)
	}
}

func TestCmdKeyRemoveRefusesTheFixedRoute(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "ferry", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"route": "key2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fileStore{dir: dir}
	if err := store.Add("key2", "s2"); err != nil {
		t.Fatal(err)
	}
	err := cmdKeyRemove(store, []string{"key2"})
	if err == nil {
		t.Fatal("removing the fixed route's key accepted, want refusal")
	}
	if !strings.Contains(err.Error(), "ferry route auto") {
		t.Errorf("error = %v, want it to point at ferry route auto", err)
	}
}

func TestResolveRoute(t *testing.T) {
	cfg := Config{APIKeys: []apiKey{{Name: "key1"}, {Name: "key2"}}}
	if err := resolveRoute(&cfg); err != nil {
		t.Fatalf("resolveRoute empty: %v", err)
	}
	if cfg.RouteIdx != -1 {
		t.Errorf("RouteIdx = %d, want -1 (automatic)", cfg.RouteIdx)
	}

	cfg.Route = "key2"
	if err := resolveRoute(&cfg); err != nil {
		t.Fatalf("resolveRoute key2: %v", err)
	}
	if cfg.RouteIdx != 1 {
		t.Errorf("RouteIdx = %d, want 1", cfg.RouteIdx)
	}

	cfg.Route = "gone"
	err := resolveRoute(&cfg)
	if err == nil {
		t.Fatal("unknown route name accepted, want error")
	}
	if !strings.Contains(err.Error(), "ferry status") {
		t.Errorf("error = %v, want it to point at ferry status", err)
	}
}

func TestPromptDefault(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("myname\n"))
	if got := promptDefault(r, &bytes.Buffer{}, "Name this key", "key1"); got != "myname" {
		t.Errorf("promptDefault = %q, want myname", got)
	}
	r = bufio.NewReader(strings.NewReader("\n"))
	if got := promptDefault(r, &bytes.Buffer{}, "Name this key", "key1"); got != "key1" {
		t.Errorf("promptDefault empty = %q, want default key1", got)
	}
}

// Accepting the default name must never collide, even after a removal made
// len(names)+1 point at an existing name.
func TestDefaultKeyName(t *testing.T) {
	if got := defaultKeyName(nil); got != "key1" {
		t.Errorf("defaultKeyName(nil) = %q, want key1", got)
	}
	if got := defaultKeyName([]string{"key1", "key3"}); got != "key2" {
		t.Errorf("defaultKeyName = %q, want key2 (first unused)", got)
	}
	if got := defaultKeyName([]string{"key1", "key2", "key3"}); got != "key4" {
		t.Errorf("defaultKeyName = %q, want key4", got)
	}
}

func TestReadSecretPlain(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("sk-abc\n"))
	got, err := readSecret(r, nil)
	if err != nil || got != "sk-abc" {
		t.Errorf("readSecret = %q, %v; want sk-abc", got, err)
	}
}

// Pins the fix that the hidden path forwards its stdin to bash: exec without
// Stdin reads /dev/null and the key came back empty (observed 2026-11). bash
// read -s on a pipe reads plainly, so a pipe stands in for the terminal.
func TestReadSecretHiddenForwardsStdin(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.WriteString("sk-abc\n"); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	got, err := readSecret(bufio.NewReader(pr), pr)
	if err != nil || got != "sk-abc" {
		t.Errorf("readSecret hidden = %q, %v; want sk-abc", got, err)
	}
}

func TestPlistContent(t *testing.T) {
	got := plistContent("/usr/local/bin/ferry", "/Users/x/.config/ferry/ferry.log", "ferry")
	for _, want := range []string{
		"<string>ferry</string>",
		"<string>/usr/local/bin/ferry</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>/Users/x/.config/ferry/ferry.log</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q:\n%s", want, got)
		}
	}
}

func TestServePortInUse(t *testing.T) {
	pinNoRunningFerry(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := DefaultConfig()
	cfg.Listen = ln.Addr().String()
	err = serve(cfg, NewNullLogger())
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("serve on a taken port = %v, want the friendly already-in-use error", err)
	}
}

// pinServiceInstalled overrides the machine-derived answer for one test.
func pinServiceInstalled(t *testing.T, installed bool) {
	t.Helper()
	original := serviceInstalled
	serviceInstalled = func() bool { return installed }
	t.Cleanup(func() { serviceInstalled = original })
}

// pinNoRunningFerry points the pidfile at an empty dir, so a test exercising
// applyKeyChange can never signal the machine's real ferry.
func pinNoRunningFerry(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}
