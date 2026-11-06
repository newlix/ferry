package main

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// cmdStatus answers "am I fine?" in one glance: is the gateway up, which
// plans are attached, how much quota each has left. It fetches quota
// directly from the console endpoints, so it works even when the gateway
// itself is down.
func cmdStatus(cfg Config, store KeyStore, out io.Writer) error {
	state, install := "not running", "not a service — glms install starts it at login"
	if healthCheck(cfg.Listen) {
		state = "running at http://" + cfg.Listen
	}
	if serviceInstalled() {
		install = "LaunchAgent installed"
	}
	fmt.Fprintf(out, "glms — %s (%s)\n\n", state, install)

	names, err := store.Names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(out, "No keys yet. Add one:  glms key add")
		return nil
	}
	m := newUsageMonitor(cfg.Upstreams.Usage, NewNullLogger())
	for _, name := range names {
		secret, err := store.Get(name)
		if err != nil {
			fmt.Fprintf(out, "  %-14s unavailable: %v\n", name, err)
			continue
		}
		u, err := m.fetchKey(secret)
		renderKeyStatus(out, name, u, err)
	}
	return nil
}

func renderKeyStatus(out io.Writer, name string, u keyUsage, err error) {
	if err != nil {
		fmt.Fprintf(out, "  %-14s unreachable: %v\n", name, err)
		return
	}
	fmt.Fprintf(out, "  %-14s %-18s %s %3.0f%% left this week, %3.0f%% this session, %s\n",
		name, planLabel(u), bar(u.weeklyRem), u.weeklyRem*100, u.sessionRem*100, expiryLabel(u.expiry))
}

func planLabel(u keyUsage) string {
	if u.planName == "" {
		return "unknown plan"
	}
	return u.planName
}

func expiryLabel(t time.Time) string {
	if t.IsZero() {
		return "no expiry date"
	}
	return "expires " + t.Format("2006-01-02")
}

func bar(frac float64) string {
	const width = 10
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(math.Round(frac * width))
	return "[" + strings.Repeat("=", filled) + strings.Repeat("·", width-filled) + "]"
}

func healthCheck(listen string) bool {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + listen + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
