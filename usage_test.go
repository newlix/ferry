package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// zaiUsageStub serves the two undocumented console endpoints with the
// response shapes z.ai's own subscription UI uses. weekly/session remaining
// per key are given as consumed fractions.
type zaiUsageStub struct {
	server   *httptest.Server
	weekly   map[string]float64 // key -> remaining fraction
	session  map[string]float64
	plan     string
	expiry   map[string]string // key -> renewal date (2006-01-02)
	failKeys map[string]bool   // keys whose quota fetch returns 500
}

func newZaiUsageStub(t *testing.T) *zaiUsageStub {
	t.Helper()
	defaultExpiry := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	stub := &zaiUsageStub{
		weekly:   map[string]float64{"k1": 0.8, "k2": 0.8},
		session:  map[string]float64{"k1": 0.9, "k2": 0.9},
		plan:     "GLM Coding Pro",
		expiry:   map[string]string{"k1": defaultExpiry, "k2": defaultExpiry},
		failKeys: map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/monitor/usage/quota/limit", func(w http.ResponseWriter, r *http.Request) {
		key := bearerKey(r)
		if stub.failKeys[key] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// usage 1000, currentValue derived from the remaining fractions.
		weeklyRem, sessionRem := stub.weekly[key], stub.session[key]
		fmt.Fprintf(w, `{"code":200,"data":{"limits":[
			{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":1000,"currentValue":%.0f},
			{"type":"TOKENS_LIMIT","unit":6,"number":7,"usage":1000,"currentValue":%.0f},
			{"type":"TIME_LIMIT","unit":5,"number":30,"usage":4000,"currentValue":300}
		]},"success":true}`, (1-sessionRem)*1000, (1-weeklyRem)*1000)
	})
	mux.HandleFunc("/api/biz/subscription/list", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":200,"data":[{"id":"1","productName":%q,"status":"VALID","valid":"2026-01-12 16:55:13-2026-02-12 16:55:13","nextRenewTime":%q,"billingCycle":"monthly","inCurrentPeriod":true}],"success":true}`, stub.plan, stub.expiry[bearerKey(r)])
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func bearerKey(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}
	return ""
}

func TestUsageFetchParsesConsoleShapes(t *testing.T) {
	stub := newZaiUsageStub(t)
	m := newUsageMonitor(stub.server.URL)
	m.refresh([]string{"k1", "k2"})

	got := map[string]keyUsage{}
	m.mu.Lock()
	for i, k := range []string{"k1", "k2"} {
		got[k] = m.state[i]
	}
	m.mu.Unlock()

	for _, k := range []string{"k1", "k2"} {
		u := got[k]
		if !u.ok {
			t.Fatalf("key %s: fetch failed", k)
		}
		if u.weeklyRem < 0.79 || u.weeklyRem > 0.81 {
			t.Errorf("key %s: weeklyRem = %.3f, want ~0.80", k, u.weeklyRem)
		}
		if u.sessionRem < 0.89 || u.sessionRem > 0.91 {
			t.Errorf("key %s: sessionRem = %.3f, want ~0.90", k, u.sessionRem)
		}
		if u.planName != "GLM Coding Pro" {
			t.Errorf("key %s: planName = %q, want GLM Coding Pro", k, u.planName)
		}
		if u.expiry.Format("2006-01-02") != stub.expiry[k] {
			t.Errorf("key %s: expiry = %v, want %s", k, u.expiry, stub.expiry[k])
		}
	}
}

func TestUsageFailedFetchFallsBackToUnknown(t *testing.T) {
	stub := newZaiUsageStub(t)
	stub.failKeys["k1"] = true
	m := newUsageMonitor(stub.server.URL)
	m.refresh([]string{"k1", "k2"})

	m.mu.Lock()
	_, k1Known := m.state[0]
	k2 := m.state[1]
	m.mu.Unlock()
	if k1Known {
		t.Error("failed key kept usage state, want unknown")
	}
	if !k2.ok {
		t.Error("healthy key lost usage state")
	}
}

func TestCandidateOrderWeightedSpread(t *testing.T) {
	m := newUsageMonitor("http://unused.example")
	now := time.Now()
	m.mu.Lock()
	m.state = map[int]keyUsage{
		0: {ok: true, weeklyRem: 0.9, sessionRem: 0.9, expiry: now.AddDate(0, 0, 30)},   // weight 1
		1: {ok: true, weeklyRem: 0.5, sessionRem: 0.9, expiry: now.AddDate(0, 0, 60)},   // weight 1
		3: {ok: true, weeklyRem: 0.8, sessionRem: 0.9, expiry: now.Add(24 * time.Hour)}, // 1 + 3*0.5*0.8   = 2.2
		4: {ok: true, weeklyRem: 0.8, sessionRem: 0.9, expiry: now.Add(6 * time.Hour)},  // 1 + 3*0.875*0.8 = 3.1
		5: {ok: true, weeklyRem: 0.0, sessionRem: 0.9},                                  // exhausted
		6: {ok: true, weeklyRem: 0.7, sessionRem: 0.9, expiry: now.AddDate(0, 0, -1)},   // expired
		7: {ok: true, weeklyRem: 0.9, sessionRem: 0.0},                                  // exhausted
	}
	m.mu.Unlock()
	// key 2 unknown: weight 1. Total weight 8.3.

	const picks = 83
	counts := map[int]int{}
	for range picks {
		order := m.candidateOrder(8, now)
		first := order[0]
		if first == 5 || first == 6 || first == 7 {
			t.Fatalf("exhausted key #%d picked first: %v", first+1, order)
		}
		// Exhausted keys always at the tail, most weekly remaining first.
		if order[5] != 7 || order[6] != 6 || order[7] != 5 {
			t.Fatalf("exhausted tail = %v, want [7 6 5]", order[5:])
		}
		counts[first]++
	}

	// Smooth WRR keeps each key within one request of its exact share.
	wantShare := map[int]float64{0: 1, 1: 1, 2: 1, 3: 2.2, 4: 3.1}
	for i, share := range wantShare {
		ideal := share / 8.3 * picks
		if math.Abs(float64(counts[i])-ideal) > 1.5 {
			t.Errorf("key #%d picked %d times, want ~%.1f (weight %.1f)", i+1, counts[i], ideal, share)
		}
	}
}

func TestCandidateOrderEqualWeightsRotate(t *testing.T) {
	m := newUsageMonitor("http://unused.example")
	now := time.Now()
	// No state at all: three keys, all weight 1, must rotate in order.
	var got []int
	for range 6 {
		got = append(got, m.candidateOrder(3, now)[0])
	}
	want := []int{0, 1, 2, 0, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rotation = %v, want %v", got, want)
		}
	}
}

func TestExpiryBoost(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		u    keyUsage
		want float64
	}{
		{"unknown expiry", keyUsage{ok: true, weeklyRem: 0.9}, 0},
		{"far from expiry", keyUsage{ok: true, weeklyRem: 0.9, expiry: now.AddDate(0, 0, 30)}, 0},
		{"already expired", keyUsage{ok: true, weeklyRem: 0.9, expiry: now.Add(-time.Hour)}, 0},
		{"half the window, 80% weekly left", keyUsage{ok: true, weeklyRem: 0.8, expiry: now.Add(24 * time.Hour)}, 3 * 0.5 * 0.8},
		{"at expiry, full weekly", keyUsage{ok: true, weeklyRem: 1, expiry: now.Add(time.Millisecond)}, 3},
		{"at expiry, empty weekly", keyUsage{ok: true, weeklyRem: 0, expiry: now.Add(time.Millisecond)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expiryBoost(tt.u, now)
			if math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("expiryBoost = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProxySkipsExhaustedKey(t *testing.T) {
	usageStub := newZaiUsageStub(t)
	usageStub.weekly["k1"] = 0 // k1 weekly quota gone
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	srv, gw := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL, Usage: usageStub.server.URL}
	})
	gw.RefreshUsage()

	// No messages field: no sticky binding, the quota order decides.
	resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := expectAuth(t, seen); got != "Bearer k2" {
		t.Fatalf("exhausted key used first: %q, want Bearer k2", got)
	}
}

func TestStickySkipsKeyThatRunsOutOfQuota(t *testing.T) {
	usageStub := newZaiUsageStub(t)
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	srv, gw := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL, Usage: usageStub.server.URL}
	})

	// Conversation binds to k1 (round-robin start, both keys healthy).
	chatWithPrefix(t, srv.URL, "proj A")
	if got := expectAuth(t, seen); got != "Bearer k1" {
		t.Fatalf("first request used %q, want Bearer k1", got)
	}

	// k1's weekly quota runs out; the binding must be skipped pre-emptively,
	// without paying a 429 round trip first.
	usageStub.weekly["k1"] = 0
	gw.RefreshUsage()
	chatWithPrefix(t, srv.URL, "proj A")
	if got := expectAuth(t, seen); got != "Bearer k2" {
		t.Fatalf("after quota ran out the conversation used %q, want Bearer k2 directly", got)
	}
}

func TestRoundRobinFallbackWhenUsageUnknown(t *testing.T) {
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	// Usage monitor points at an unroutable port and was never refreshed:
	// no usage data at all, selection must rotate round-robin.
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL, Usage: "http://127.0.0.1:1"}
	})

	// No messages field: no sticky, requests must alternate keys.
	for _, want := range []string{"Bearer k1", "Bearer k2"} {
		resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := expectAuth(t, seen); got != want {
			t.Fatalf("round-robin fallback served %q, want %q", got, want)
		}
	}
}

func TestProxyShiftsTowardKeyAboutToExpire(t *testing.T) {
	usageStub := newZaiUsageStub(t)
	usageStub.expiry["k1"] = time.Now().AddDate(0, 0, 1).Format("2006-01-02") // tomorrow: inside the 48h ramp
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	srv, gw := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL, Usage: usageStub.server.URL}
	})
	gw.RefreshUsage()

	counts := map[string]int{}
	for range 30 {
		resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		counts[expectAuth(t, seen)]++
	}
	// The expiring key gets the larger share, but the other key keeps
	// working: weight ramps up, it must not grab all the work.
	if counts["Bearer k1"] <= counts["Bearer k2"] {
		t.Errorf("expiring key did not get the larger share: %v", counts)
	}
	if counts["Bearer k2"] < 3 {
		t.Errorf("other key starved (ramp grabbed all work): %v", counts)
	}
}

func TestProxyAlternatesKeysWhenNoneIsRamping(t *testing.T) {
	usageStub := newZaiUsageStub(t) // both keys expire in 30 days: no ramp
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	srv, gw := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL, Usage: usageStub.server.URL}
	})
	gw.RefreshUsage()

	for _, want := range []string{"Bearer k1", "Bearer k2"} {
		resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := expectAuth(t, seen); got != want {
			t.Fatalf("even spread broken: got %q, want %q", got, want)
		}
	}
}

func TestUsageEndpoint(t *testing.T) {
	usageStub := newZaiUsageStub(t)
	srv, gw := newTestGateway(t, func(c *Config) {
		c.Upstreams.Usage = usageStub.server.URL
	})
	gw.RefreshUsage()

	resp, err := http.Get(srv.URL + "/v1/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Keys []struct {
			Key     int     `json:"key"`
			Plan    string  `json:"plan"`
			Weekly  float64 `json:"weeklyRemainingPercent"`
			Session float64 `json:"sessionRemainingPercent"`
			Expires string  `json:"expires"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	if len(got.Keys) != 2 {
		t.Fatalf("keys = %d, want 2", len(got.Keys))
	}
	first := got.Keys[0]
	if first.Plan != "GLM Coding Pro" || first.Expires != usageStub.expiry["k1"] {
		t.Errorf("key 1 = %+v, want plan GLM Coding Pro expiring %s", first, usageStub.expiry["k1"])
	}
	if first.Weekly < 79 || first.Weekly > 81 {
		t.Errorf("weekly = %.1f, want ~80", first.Weekly)
	}
}
