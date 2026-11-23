package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// cmdStatus answers "am I fine?" in one glance: is the gateway up, which
// plans are attached, how much quota each has left. It fetches quota
// directly from the console endpoints, so it works even when the gateway
// itself is down.
func cmdStatus(cfg Config, store KeyStore, out io.Writer) error {
	running := healthCheck(cfg.Listen)
	line := "ferry not running"
	if running {
		line = "ferry running at http://" + cfg.Listen
	}
	switch {
	case serviceInstalled() && running:
		line += " (starts at login)"
	case serviceInstalled() && !running:
		line += " (starts at login; logs: " + filepath.Join(configDir(), "ferry.log") + ")"
	case !serviceInstalled() && !running:
		line += " (ferry install starts it at login)"
	}
	if cfg.Route != "" {
		line += " · fixed to " + cfg.Route
	}
	fmt.Fprintf(out, "%s\n\n", line)

	names, err := store.Names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(out, "No keys yet. Add one:  ferry key add")
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
		if errors.As(err, &ce) {
			if ce.code == 401 || ce.code == 403 {
				fmt.Fprintf(out, "  %-14s invalid key. Remove it:  ferry key remove %s\n", name, name)
				return
			}
			fmt.Fprintf(out, "  %-14s quota unknown (z.ai error: %s)\n", name, ce.msg)
			return
		}
		fmt.Fprintf(out, "  %-14s quota unknown (could not reach z.ai)\n", name)
		return
	}

	line := fmt.Sprintf("  %-14s %-8s ", name, planLabel(u))
	switch {
	case u.weeklyRem <= 0:
		line += "out of quota this week"
		if !u.weeklyReset.IsZero() {
			line += " · resets in " + relTime(time.Until(u.weeklyReset))
		}
	default:
		line += fmt.Sprintf("%.0f%% this week", u.weeklyRem*100)
		if !u.weeklyReset.IsZero() {
			line += " · resets in " + relTime(time.Until(u.weeklyReset))
		}
	}
	// The 5-hour pool refills by itself; it only deserves a line when it is
	// about to interrupt work.
	if u.sessionRem < sessionLowThreshold {
		state := "low"
		if u.sessionRem <= 0 {
			state = "empty"
		}
		line += " · 5-hour pool " + state
		if !u.sessionReset.IsZero() {
			line += ", refills in " + relTime(time.Until(u.sessionReset))
		}
	}
	fmt.Fprintln(out, line)
}

const sessionLowThreshold = 0.25

func planLabel(u keyUsage) string {
	if u.planName == "" {
		return "unknown"
	}
	return strings.TrimPrefix(u.planName, "GLM Coding ")
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

func healthCheck(listen string) bool {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + listen + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
