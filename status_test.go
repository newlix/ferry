package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestBar(t *testing.T) {
	tests := []struct {
		frac float64
		want string
	}{
		{0, "[··········]"},
		{0.62, "[======····]"},
		{1, "[==========]"},
		{-1, "[··········]"},
		{2, "[==========]"},
	}
	for _, tt := range tests {
		if got := bar(tt.frac); got != tt.want {
			t.Errorf("bar(%v) = %s, want %s", tt.frac, got, tt.want)
		}
	}
}

func TestRenderKeyStatus(t *testing.T) {
	var out bytes.Buffer
	u := keyUsage{ok: true, planName: "GLM Coding Pro", weeklyRem: 0.8, sessionRem: 0.9, expiry: time.Date(2026, 12, 7, 0, 0, 0, 0, time.UTC)}
	renderKeyStatus(&out, "pro", u, nil)
	got := out.String()
	for _, want := range []string{"pro", "GLM Coding Pro", "[========··]", "80% left this week", "90% this session", "expires 2026-12-07"} {
		if !strings.Contains(got, want) {
			t.Errorf("status line missing %q: %s", want, got)
		}
	}

	out.Reset()
	renderKeyStatus(&out, "pro", keyUsage{}, errString("boom"))
	if !strings.Contains(out.String(), "unreachable: boom") {
		t.Errorf("error render = %s", out.String())
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestCmdStatus(t *testing.T) {
	stub := newZaiUsageStub(t)
	store := &fileStore{dir: t.TempDir()}
	if err := store.Add("pro", "k1"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Upstreams.Usage = stub.server.URL
	cfg.Listen = "127.0.0.1:1" // nothing listens: not running
	pinServiceInstalled(t, false)

	var out bytes.Buffer
	if err := cmdStatus(cfg, store, &out); err != nil {
		t.Fatalf("cmdStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{"not running", "GLM Coding Pro", "80% left this week", "expires"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
}

func TestCmdStatusInvalidKey(t *testing.T) {
	stub := newZaiUsageStub(t)
	stub.rejectKeys["k1"] = true
	store := &fileStore{dir: t.TempDir()}
	if err := store.Add("pro", "k1"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Upstreams.Usage = stub.server.URL
	cfg.Listen = "127.0.0.1:1"
	pinServiceInstalled(t, false)

	var out bytes.Buffer
	if err := cmdStatus(cfg, store, &out); err != nil {
		t.Fatalf("cmdStatus: %v", err)
	}
	if !strings.Contains(out.String(), "invalid key — remove it:  glms key remove pro") {
		t.Errorf("status does not call a dead key a dead key:\n%s", out.String())
	}
}

func TestCmdStatusNoKeys(t *testing.T) {
	store := &fileStore{dir: t.TempDir()}
	cfg := DefaultConfig()
	cfg.Listen = "127.0.0.1:1"
	pinServiceInstalled(t, false)

	var out bytes.Buffer
	if err := cmdStatus(cfg, store, &out); err != nil {
		t.Fatalf("cmdStatus: %v", err)
	}
	if !strings.Contains(out.String(), "glms key add") {
		t.Errorf("empty status does not point at the next step:\n%s", out.String())
	}
}
