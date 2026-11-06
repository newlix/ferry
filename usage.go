package main

import (
	"encoding/json"
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
	ok         bool
	planName   string
	sessionRem float64 // remaining fraction 0..1; 1 when the window is absent
	weeklyRem  float64
	expiry     time.Time // zero = unknown
	fetchedAt  time.Time
}

type usageMonitor struct {
	client *http.Client
	base   string
	log    Logger

	mu    sync.Mutex
	state map[int]keyUsage // keyed by apiKeys index
	cw    map[int]float64  // smooth weighted round-robin current weights
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
func (m *usageMonitor) refresh(keys []string) {
	for i := range keys {
		u, err := m.fetchKey(keys[i])
		if err != nil {
			m.mu.Lock()
			delete(m.state, i)
			m.mu.Unlock()
			m.log.Quiet.Printf("usage key=#%d: fetch failed: %v", i+1, err)
			continue
		}
		m.mu.Lock()
		m.state[i] = u
		m.mu.Unlock()
		m.log.Verbose.Printf("usage key=#%d plan=%q weekly=%.0f%% session=%.0f%% expires=%s", i+1, u.planName, u.weeklyRem*100, u.sessionRem*100, usageExpiryLabel(u.expiry))
	}
}

func (m *usageMonitor) fetchKey(key string) (keyUsage, error) {
	u := keyUsage{ok: true, sessionRem: 1, weeklyRem: 1, fetchedAt: time.Now()}
	if err := m.fetchQuota(key, &u); err != nil {
		return keyUsage{}, err
	}
	// Best-effort: plan name and expiry may stay unknown.
	plan, expiry, _ := m.fetchSubscription(key)
	u.planName, u.expiry = plan, expiry
	return u, nil
}

// fetchQuota parses the limits array. A limit window is identified by
// (unit, number): (3,5) is the 5-hour rolling window, (6,7) the weekly one;
// TIME_LIMIT entries (web-search counts) are ignored. The entry type was
// renamed TOKENS_LIMIT -> CREDIT_LIMIT at some point; both are accepted.
func (m *usageMonitor) fetchQuota(key string, u *keyUsage) error {
	var resp struct {
		Data struct {
			Limits []struct {
				Type    string  `json:"type"`
				Unit    int     `json:"unit"`
				Number  int     `json:"number"`
				Usage   float64 `json:"usage"`
				Current float64 `json:"currentValue"`
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
		switch {
		case l.Unit == 3 && l.Number == 5:
			u.sessionRem = rem
			seen = true
		case l.Unit == 6 && l.Number == 7:
			u.weeklyRem = rem
			seen = true
		}
	}
	if !seen {
		return fmt.Errorf("quota response has no 5h/weekly limit entry")
	}
	return nil
}

func (m *usageMonitor) fetchSubscription(key string) (plan string, expiry time.Time, err error) {
	var resp struct {
		Data []struct {
			ProductName   string `json:"productName"`
			Status        string `json:"status"`
			NextRenewTime string `json:"nextRenewTime"`
		} `json:"data"`
	}
	if err := m.getJSON(usagePathSubscription, key, &resp); err != nil {
		return "", time.Time{}, err
	}
	for _, s := range resp.Data {
		if s.Status != "VALID" {
			continue
		}
		plan = s.ProductName
		if t, perr := time.Parse("2006-01-02", s.NextRenewTime); perr == nil {
			expiry = t
		}
		break
	}
	return plan, expiry, nil
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

// Key weights: equal weight spreads traffic evenly. A key within
// expiryRampWindow of plan expiry gains weight linearly, up to +expiryMaxBoost
// at expiry and scaled by its remaining weekly quota: traffic shifts toward
// it gradually instead of all at once, so its per-key rate limits still share
// the load with the other keys, and an almost-empty plan attracts little
// extra traffic.
const (
	expiryRampWindow = 48 * time.Hour
	expiryMaxBoost   = 3
)

func expiryBoost(u keyUsage, now time.Time) float64 {
	if u.expiry.IsZero() {
		return 0
	}
	left := u.expiry.Sub(now)
	if left <= 0 || left >= expiryRampWindow {
		return 0
	}
	return expiryMaxBoost * (1 - left.Seconds()/expiryRampWindow.Seconds()) * u.weeklyRem
}

// candidateOrder ranks keys for new conversations and failover:
//
//  1. the usable key picked by smooth weighted round-robin (equal weights
//     degenerate to plain round-robin);
//  2. the remaining usable keys in config order;
//  3. exhausted or already expired keys, most weekly remaining first.
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
		case u.weeklyRem <= 0 || u.sessionRem <= 0 || (!u.expiry.IsZero() && !u.expiry.After(now)):
			exhausted = append(exhausted, i)
		default:
			usable = append(usable, i)
			weights[i] = 1 + expiryBoost(u, now)
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
// assumed fine (they order after known-good ones anyway).
func (m *usageMonitor) usable(i int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.state[i]
	return !ok || (u.weeklyRem > 0 && u.sessionRem > 0 && (u.expiry.IsZero() || u.expiry.After(time.Now())))
}

type usageEntry struct {
	Key     int     `json:"key"`
	Plan    string  `json:"plan,omitempty"`
	Weekly  float64 `json:"weeklyRemainingPercent"`
	Session float64 `json:"sessionRemainingPercent"`
	Expires string  `json:"expires,omitempty"`
}

func (m *usageMonitor) snapshot(n int) []usageEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := make([]usageEntry, 0, n)
	for i := range n {
		e := usageEntry{Key: i + 1, Weekly: 100, Session: 100}
		if u, ok := m.state[i]; ok {
			e.Plan = u.planName
			e.Weekly = u.weeklyRem * 100
			e.Session = u.sessionRem * 100
			e.Expires = usageExpiryLabel(u.expiry)
		}
		entries = append(entries, e)
	}
	return entries
}

func usageExpiryLabel(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02")
}
