package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestCmdKeyAddSavesVerifiedKey(t *testing.T) {
	stub := newZaiUsageStub(t)
	dir := t.TempDir()
	store := &fileStore{dir: dir}
	cfg := DefaultConfig()
	cfg.Upstreams.Usage = stub.server.URL
	pinServiceInstalled(t, false)

	var out bytes.Buffer
	in := strings.NewReader("pro\nsk-good\n")
	if err := cmdKeyAdd(cfg, store, in, &out); err != nil {
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
	pinServiceInstalled(t, false)

	var out bytes.Buffer
	err := cmdKeyAdd(cfg, store, strings.NewReader("pro\nsk-bad\n"), &out)
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
	pinServiceInstalled(t, false)

	var out bytes.Buffer
	if err := cmdKeyAdd(cfg, store, strings.NewReader("pro\nsk-flaky\n"), &out); err != nil {
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
	err := cmdKeyAdd(DefaultConfig(), store, strings.NewReader("pro\nsk-x\n"), &bytes.Buffer{})
	if err == nil {
		t.Fatal("duplicate name accepted, want error")
	}
}

func TestPromptDefault(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("myname\n"))
	if got := promptDefault(r, &bytes.Buffer{}, "Name", "key1"); got != "myname" {
		t.Errorf("promptDefault = %q, want myname", got)
	}
	r = bufio.NewReader(strings.NewReader("\n"))
	if got := promptDefault(r, &bytes.Buffer{}, "Name", "key1"); got != "key1" {
		t.Errorf("promptDefault empty = %q, want default key1", got)
	}
}

func TestReadSecretPlain(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("sk-abc\n"))
	got, err := readSecret(r, false)
	if err != nil || got != "sk-abc" {
		t.Errorf("readSecret = %q, %v; want sk-abc", got, err)
	}
}

func TestPlistContent(t *testing.T) {
	got := plistContent("/usr/local/bin/glms", "/Users/x/.config/glms/glms.log", "glms")
	for _, want := range []string{
		"<string>glms</string>",
		"<string>/usr/local/bin/glms</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>/Users/x/.config/glms/glms.log</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q:\n%s", want, got)
		}
	}
}

// pinServiceInstalled overrides the machine-derived answer for one test.
func pinServiceInstalled(t *testing.T, installed bool) {
	t.Helper()
	original := serviceInstalled
	serviceInstalled = func() bool { return installed }
	t.Cleanup(func() { serviceInstalled = original })
}
