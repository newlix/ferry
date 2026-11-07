package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRenderKeyStatus(t *testing.T) {
	render := func(u keyUsage) string {
		var out bytes.Buffer
		renderKeyStatus(&out, "pro", u, nil)
		return out.String()
	}

	// The 30-minute margins keep the rounded countdowns stable while the
	// test runs.
	healthy := keyUsage{ok: true, planName: "GLM Coding Max", weeklyRem: 0.8, sessionRem: 0.9,
		weeklyReset: time.Now().Add(60*time.Hour + 30*time.Minute), sessionReset: time.Now().Add(3*time.Hour + 30*time.Minute)}
	got := render(healthy)
	for _, want := range []string{"Max", "80% this week", "· resets in 2d 12h"} {
		if !strings.Contains(got, want) {
			t.Errorf("healthy line missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "5-hour") {
		t.Errorf("healthy line shows the 5-hour pool: %s", got)
	}

	sessionLow := healthy
	sessionLow.sessionRem = 0.2
	sessionLow.sessionReset = time.Now().Add(2 * time.Hour)
	got = render(sessionLow)
	if !strings.Contains(got, "5-hour pool low, refills in") {
		t.Errorf("low-pool line missing the refill warning: %s", got)
	}

	sessionEmpty := healthy
	sessionEmpty.sessionRem = 0
	got = render(sessionEmpty)
	if !strings.Contains(got, "5-hour pool empty") {
		t.Errorf("empty-pool line missing: %s", got)
	}

	weeklyOut := healthy
	weeklyOut.weeklyRem = 0
	got = render(weeklyOut)
	for _, want := range []string{"out of quota this week", "· resets in"} {
		if !strings.Contains(got, want) {
			t.Errorf("exhausted line missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "% this week") {
		t.Errorf("exhausted line shows a percentage: %s", got)
	}

	unknownPlan := healthy
	unknownPlan.planName = ""
	if !strings.Contains(render(unknownPlan), "unknown") {
		t.Errorf("unknown plan not labeled: %s", render(unknownPlan))
	}

	var out bytes.Buffer
	renderKeyStatus(&out, "pro", keyUsage{}, errString("boom"))
	if !strings.Contains(out.String(), "quota unknown (could not reach z.ai)") {
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
	for _, want := range []string{"not running", "Pro", "80% this week", "resets in"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "5-hour") {
		t.Errorf("healthy key shows the 5-hour pool:\n%s", got)
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
	if !strings.Contains(out.String(), "invalid key. Remove it:  ferry key remove pro") {
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
	if !strings.Contains(out.String(), "ferry key add") {
		t.Errorf("empty status does not point at the next step:\n%s", out.String())
	}
}
