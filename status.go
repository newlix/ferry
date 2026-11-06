package main

import (
	"errors"
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
		var ce *consoleError
		if errors.As(err, &ce) && (ce.code == 401 || ce.code == 403) {
			fmt.Fprintf(out, "  %-14s invalid key — remove it:  glms key remove %s\n", name, name)
			return
		}
		fmt.Fprintf(out, "  %-14s unreachable: %v\n", name, err)
		return
	}
	fmt.Fprintf(out, "  %-14s %-18s %s %3.0f%% left this week, resets in %s · session %3.0f%%, resets in %s\n",
		name, planLabel(u), bar(u.weeklyRem), u.weeklyRem*100, relTime(time.Until(u.weeklyReset)), u.sessionRem*100, relTime(time.Until(u.sessionReset)))
}

// relTime renders a countdown compactly: "2h 5m", "1d 8h". Negative or zero
// durations mean "about now".
func relTime(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%dh %dm", h, int(d.Minutes())-h*60)
	default:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%dd %dh", days, int(d.Hours())-days*24)
	}
}

func planLabel(u keyUsage) string {
	if u.planName == "" {
		return "unknown plan"
	}
	return u.planName
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
