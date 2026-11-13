package main

import (
	"net/http"
	"time"
)

// quotaWindow is one meter of a key: Left is the remaining fraction 0..1,
// ResetsAt nil while the window is untouched (z.ai sends no reset then).
type quotaWindow struct {
	Left     float64    `json:"left"`
	ResetsAt *time.Time `json:"resetsAt"`
}

// quotaKey: Weekly and Session are nil when the key's last quota fetch
// failed or has not happened yet, so a client cannot read an unknown key as
// a full one.
type quotaKey struct {
	Name    string       `json:"name"`
	Plan    string       `json:"plan"`
	PlanEnd *time.Time   `json:"planEnd"`
	Weekly  *quotaWindow `json:"weekly"`
	Session *quotaWindow `json:"session"`
}

type quotaReply struct {
	RefreshedAt *time.Time `json:"refreshedAt"`
	Keys        []quotaKey `json:"keys"`
}

// handleQuota is the home page's data as JSON, for scripts.
func (g *Gateway) handleQuota(w http.ResponseWriter, r *http.Request) {
	usage, known, refreshedAt := g.usage.snapshot(len(g.cfg.APIKeys))
	out := quotaReply{RefreshedAt: timeOrNil(refreshedAt), Keys: make([]quotaKey, len(g.cfg.APIKeys))}
	for i, k := range g.cfg.APIKeys {
		out.Keys[i] = quotaKey{Name: k.Name}
		if !known[i] {
			continue
		}
		u := usage[i]
		out.Keys[i].Plan = u.planName
		out.Keys[i].PlanEnd = timeOrNil(u.planEnd)
		out.Keys[i].Weekly = &quotaWindow{Left: u.weeklyRem, ResetsAt: timeOrNil(u.weeklyReset)}
		out.Keys[i].Session = &quotaWindow{Left: u.sessionRem, ResetsAt: timeOrNil(u.sessionReset)}
	}
	writeJSON(w, http.StatusOK, out)
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
