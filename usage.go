package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// z.ai's own subscription console reads these endpoints with a coding-plan
// key; they are undocumented but stable in practice (2026-11, several
// independent quota trackers rely on them). quota/limit reports the 5-hour
// and weekly meters, subscription/list the plan name and renewal date.
const (
	usagePathQuota        = "/api/monitor/usage/quota/limit"
	usagePathSubscription = "/api/biz/subscription/list"

	usageRefreshInterval = 5 * time.Minute
	usageHTTPTimeout     = 15 * time.Second
	usageMaxBody         = 1 << 20
)

type keyUsage struct {
	planName     string
	planEnd      time.Time // end of the paid period; zero = unknown
	autoRenew    bool
	sessionRem   float64 // remaining fraction 0..1; 1 when the window is absent
	weeklyRem    float64
	sessionReset time.Time // zero = unknown
	weeklyReset  time.Time
}

type usageMonitor struct {
	client *http.Client
	base   string
	log    Logger

	mu          sync.Mutex
	state       map[int]keyUsage // keyed by apiKeys index
	cw          map[int]float64  // smooth weighted round-robin current weights
	refreshedAt time.Time        // zero until the first refresh finishes
}

func newUsageMonitor(base string, log Logger) *usageMonitor {
	return &usageMonitor{
		client: &http.Client{Timeout: usageHTTPTimeout},
		base:   base,
		log:    log,
		state:  map[int]keyUsage{},
	}
}

// refresh fetches usage for every key. A key whose fetch fails becomes
// "unknown" and keeps working: ordering falls back to config order.
func (m *usageMonitor) refresh(keys []apiKey) {
	for i := range keys {
		u, err := m.fetchKey(keys[i].Secret)
		if err != nil {
			m.mu.Lock()
			delete(m.state, i)
			m.mu.Unlock()
			m.log.Quiet.Printf("usage key=%s: fetch failed: %v", keys[i].Name, err)
			continue
		}
		m.mu.Lock()
		m.state[i] = u
		m.mu.Unlock()
		m.log.Verbose.Printf("usage key=%s plan=%q weekly=%.0f%% (resets %s) session=%.0f%% (resets %s)", keys[i].Name, u.planName, u.weeklyRem*100, relTime(time.Until(u.weeklyReset)), u.sessionRem*100, relTime(time.Until(u.sessionReset)))
	}
	m.mu.Lock()
	m.refreshedAt = time.Now()
	m.mu.Unlock()
}

// snapshot copies the cached usage of keys 0..n-1; known[i] is false for a
// key whose last fetch failed or has not happened yet.
func (m *usageMonitor) snapshot(n int) (usage []keyUsage, known []bool, refreshedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	usage = make([]keyUsage, n)
	known = make([]bool, n)
	for i := range n {
		usage[i], known[i] = m.state[i]
	}
	return usage, known, m.refreshedAt
}

func (m *usageMonitor) fetchKey(key string) (keyUsage, error) {
	u := keyUsage{sessionRem: 1, weeklyRem: 1}
	if err := m.fetchQuota(key, &u); err != nil {
		return keyUsage{}, err
	}
	// Best-effort: the plan may stay unknown; quota alone drives key choice.
	if err := m.fetchPlan(key, &u); err != nil {
		m.log.Verbose.Printf("usage: plan lookup failed: %v", err)
	}
	return u, nil
}

// fetchQuota parses the limits array. A credit window is identified by its
// unit only (3 = the 5-hour session window, 6 = the weekly window): on live
// Max-plan data (2026-11) the pairs are (3,5) and (6,1), and third-party
// docs claiming (6,7) do not match, so number is not trusted. TIME_LIMIT
// entries (web-search counts) are ignored. The entry type was renamed
// TOKENS_LIMIT -> CREDIT_LIMIT at some point; both are accepted.
// nextResetTime is epoch milliseconds.
func (m *usageMonitor) fetchQuota(key string, u *keyUsage) error {
	var resp struct {
		Data struct {
			Limits []struct {
				Type          string  `json:"type"`
				Unit          int     `json:"unit"`
				Usage         float64 `json:"usage"`
				Current       float64 `json:"currentValue"`
				NextResetTime int64   `json:"nextResetTime"`
			} `json:"limits"`
		} `json:"data"`
	}
	if err := m.getJSON(usagePathQuota, key, &resp); err != nil {
		return err
	}
	seen := false
	for _, l := range resp.Data.Limits {
		if l.Type != "TOKENS_LIMIT" && l.Type != "CREDIT_LIMIT" {
			continue
		}
		if l.Usage <= 0 {
			continue
		}
		rem := (l.Usage - l.Current) / l.Usage
		var reset time.Time // an untouched window omits nextResetTime
		if l.NextResetTime != 0 {
			reset = time.UnixMilli(l.NextResetTime)
		}
		switch l.Unit {
		case 3:
			u.sessionRem = rem
			u.sessionReset = reset
			seen = true
		case 6:
			u.weeklyRem = rem
			u.weeklyReset = reset
			seen = true
		}
	}
	if !seen {
		return errors.New("quota response has no credit window")
	}
	return nil
}

// fetchPlan reads the VALID subscription. Its "valid" field is the paid
// subscription term, "2027-08-12 20:08:05-2028-08-12 20:08:05"; the plan
// ends at the second timestamp (user, 2026-11-14). nextRenewTime and
// currentRenewTime are not the term's end: on a yearly plan bought for a
// year ahead they lag it by a year.
func (m *usageMonitor) fetchPlan(key string, u *keyUsage) error {
	var resp struct {
		Data []struct {
			ProductName string `json:"productName"`
			Status      string `json:"status"`
			Valid       string `json:"valid"`
			AutoRenew   int    `json:"autoRenew"`
		} `json:"data"`
	}
	if err := m.getJSON(usagePathSubscription, key, &resp); err != nil {
		return err
	}
	for _, s := range resp.Data {
		if s.Status != "VALID" {
			continue
		}
		u.planName = s.ProductName
		u.autoRenew = s.AutoRenew == 1
		if s.Valid == "" {
			return nil
		}
		end, err := parseValidEnd(s.Valid)
		if err != nil {
			return err
		}
		u.planEnd = end
		return nil
	}
	return nil
}

// parseValidEnd takes the end of a "valid" range: two time.DateTime
// timestamps joined by "-" (each contains "-" itself, so it splits by length).
func parseValidEnd(valid string) (time.Time, error) {
	if len(valid) != 2*len(time.DateTime)+1 || valid[len(time.DateTime)] != '-' {
		return time.Time{}, fmt.Errorf("valid %q: want \"<start>-<end>\" in %s", valid, time.DateTime)
	}
	end, err := time.ParseInLocation(time.DateTime, valid[len(time.DateTime)+1:], time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("valid %q: %w", valid, err)
	}
	return end, nil
}

// consoleError marks an endpoint reply the caller can act on: the console
// API answers a bad key with HTTP 200 and {"code":401,"msg":"token expired
// or incorrect"}, so the envelope code, not the HTTP status, says the key is
// bad. Transport failures reuse the type with the HTTP status as the code.
type consoleError struct {
	path string
	code int
	msg  string
}

func (e *consoleError) Error() string {
	return fmt.Sprintf("GET %s: code %d: %s", e.path, e.code, e.msg)
}

func (m *usageMonitor) getJSON(path, key string, out any) error {
	req, err := http.NewRequest(http.MethodGet, m.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, usageMaxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &consoleError{path: path, code: resp.StatusCode, msg: "http status"}
	}
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("GET %s: parse: %w", path, err)
	}
	if envelope.Code != 0 && envelope.Code != 200 {
		return &consoleError{path: path, code: envelope.Code, msg: envelope.Msg}
	}
	return json.Unmarshal(body, out)
}

// Key weights: equal weight spreads traffic evenly. A key whose weekly
// window resets within resetRampWindow gains weight linearly, up to
// +resetMaxBoost at the reset and scaled by its remaining weekly quota:
// unused weekly credits do not roll over, so they are burned down before the
// reset — gradually instead of all at once, so the other keys keep their
// share and no account's rate limits become the bottleneck. An almost-empty
// window attracts little extra traffic.
const (
	resetRampWindow = 48 * time.Hour
	resetMaxBoost   = 3
)

func resetBoost(u keyUsage, now time.Time) float64 {
	if u.weeklyReset.IsZero() {
		return 0
	}
	left := u.weeklyReset.Sub(now)
	if left <= 0 || left >= resetRampWindow {
		return 0
	}
	return resetMaxBoost * (1 - left.Seconds()/resetRampWindow.Seconds()) * u.weeklyRem
}

// candidateOrder ranks keys for new conversations and failover:
//
//  1. the usable key picked by smooth weighted round-robin (equal weights
//     degenerate to plain round-robin);
//  2. the remaining usable keys in config order;
//  3. exhausted keys, most weekly remaining first.
func (m *usageMonitor) candidateOrder(n int, now time.Time) []int {
	m.mu.Lock()
	defer m.mu.Unlock()

	var usable, exhausted []int
	weights := make(map[int]float64, n)
	for i := range n {
		u, ok := m.state[i]
		switch {
		case !ok:
			usable = append(usable, i)
			weights[i] = 1
		case u.weeklyRem <= 0 || u.sessionRem <= 0:
			exhausted = append(exhausted, i)
		default:
			usable = append(usable, i)
			weights[i] = 1 + resetBoost(u, now)
		}
	}
	sort.SliceStable(exhausted, func(a, b int) bool {
		return m.state[exhausted[a]].weeklyRem > m.state[exhausted[b]].weeklyRem
	})
	if len(usable) == 0 {
		return exhausted
	}
	first := m.pickWeighted(usable, weights)
	order := make([]int, 0, n)
	order = append(order, first)
	for _, i := range usable {
		if i != first {
			order = append(order, i)
		}
	}
	return append(order, exhausted...)
}

// pickWeighted advances the smooth weighted round-robin state (the nginx
// algorithm: add each weight to its current weight, pick the max, subtract
// the total weight from the winner) and returns the chosen index. Equal
// weights rotate perfectly; unequal weights interleave without bursts, each
// key keeping its share within one request of perfect proportion.
func (m *usageMonitor) pickWeighted(usable []int, weights map[int]float64) int {
	if m.cw == nil {
		m.cw = map[int]float64{}
	}
	total := 0.0
	for _, i := range usable {
		total += weights[i]
	}
	best := -1
	for _, i := range usable {
		m.cw[i] += weights[i]
		if best == -1 || m.cw[i] > m.cw[best] {
			best = i
		}
	}
	m.cw[best] -= total
	return best
}

// usable reports whether key i should be tried at all: unknown keys are
// assumed fine (they order after known-good ones anyway). A weekly reset in
// the past refills the window rather than emptying it, so it is not a
// demotion — the next refresh reports the new window.
func (m *usageMonitor) usable(i int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.state[i]
	return !ok || (u.weeklyRem > 0 && u.sessionRem > 0)
}
