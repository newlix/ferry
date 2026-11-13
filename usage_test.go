package main

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// zaiUsageStub serves the two console endpoints with the response shapes the
// live API returns (verified 2026-11-07): credit windows carry unit (3 =
// session, 6 = weekly) and nextResetTime in epoch milliseconds, and a bad
// key gets HTTP 200 with {"code":401,...}.
type zaiUsageStub struct {
	server       *httptest.Server
	weekly       map[string]float64 // key -> remaining fraction
	session      map[string]float64
	weeklyReset  map[string]time.Duration // key -> time until the weekly window resets
	sessionReset map[string]time.Duration
	plan         string
	failKeys     map[string]bool // keys whose quota fetch returns 500
	rejectKeys   map[string]bool // keys whose quota fetch returns the 401 envelope
}

func newZaiUsageStub(t *testing.T) *zaiUsageStub {
	t.Helper()
	stub := &zaiUsageStub{
		weekly:       map[string]float64{"k1": 0.8, "k2": 0.8},
		session:      map[string]float64{"k1": 0.9, "k2": 0.9},
		weeklyReset:  map[string]time.Duration{"k1": 5 * 24 * time.Hour, "k2": 5 * 24 * time.Hour},
		sessionReset: map[string]time.Duration{"k1": 2 * time.Hour, "k2": 2 * time.Hour},
		plan:         "GLM Coding Pro",
		failKeys:     map[string]bool{},
		rejectKeys:   map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/monitor/usage/quota/limit", func(w http.ResponseWriter, r *http.Request) {
		key := bearerKey(r)
		if stub.failKeys[key] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if stub.rejectKeys[key] {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":401,"msg":"token expired or incorrect","success":false}`)
			return
		}
		// usage 1000, currentValue derived from the remaining fractions.
		weeklyRem, sessionRem := stub.weekly[key], stub.session[key]
		fmt.Fprintf(w, `{"code":200,"data":{"limits":[
			{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":1000,"currentValue":%.0f,"nextResetTime":%d},
			{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":1000,"currentValue":%.0f,"nextResetTime":%d}
		]},"success":true}`,
			(1-sessionRem)*1000, time.Now().Add(stub.sessionReset[key]).UnixMilli(),
			(1-weeklyRem)*1000, time.Now().Add(stub.weeklyReset[key]).UnixMilli())
	})
	mux.HandleFunc("/api/biz/subscription/list", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":200,"data":[{"id":"1","productName":%q,"status":"VALID"}],"success":true}`, stub.plan)
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
	m := newUsageMonitor(stub.server.URL, NewNullLogger())
	m.refresh([]apiKey{{Secret: "k1"}, {Secret: "k2"}})

	got := map[string]keyUsage{}
	m.mu.Lock()
	for i, k := range []string{"k1", "k2"} {
		got[k] = m.state[i]
	}
	m.mu.Unlock()

	for _, k := range []string{"k1", "k2"} {
		u := got[k]
		if u.weeklyRem < 0.79 || u.weeklyRem > 0.81 {
			t.Errorf("key %s: weeklyRem = %.3f, want ~0.80", k, u.weeklyRem)
		}
		if u.sessionRem < 0.89 || u.sessionRem > 0.91 {
			t.Errorf("key %s: sessionRem = %.3f, want ~0.90", k, u.sessionRem)
		}
		if u.planName != "GLM Coding Pro" {
			t.Errorf("key %s: planName = %q, want GLM Coding Pro", k, u.planName)
		}
		if left := time.Until(u.weeklyReset); left < 4*24*time.Hour || left > 5*24*time.Hour+time.Minute {
			t.Errorf("key %s: weeklyReset in %v, want ~5d", k, left)
		}
		if left := time.Until(u.sessionReset); left < time.Hour || left > 2*time.Hour+time.Minute {
			t.Errorf("key %s: sessionReset in %v, want ~2h", k, left)
		}
	}
}

func TestUsageFailedFetchFallsBackToUnknown(t *testing.T) {
	stub := newZaiUsageStub(t)
	stub.failKeys["k1"] = true
	m := newUsageMonitor(stub.server.URL, NewNullLogger())
	m.refresh([]apiKey{{Secret: "k1"}, {Secret: "k2"}})

	m.mu.Lock()
	_, k1Known := m.state[0]
	k2 := m.state[1]
	m.mu.Unlock()
	if k1Known {
		t.Error("failed key kept usage state, want unknown")
	}
	if k2.weeklyRem < 0.79 || k2.weeklyRem > 0.81 {
		t.Errorf("healthy key: weeklyRem = %.3f, want ~0.80", k2.weeklyRem)
	}
}

func TestCandidateOrderWeightedSpread(t *testing.T) {
	m := newUsageMonitor("http://unused.example", NewNullLogger())
	now := time.Now()
	m.mu.Lock()
	m.state = map[int]keyUsage{
		0: {weeklyRem: 0.9, sessionRem: 0.9, weeklyReset: now.Add(5 * 24 * time.Hour)}, // weight 1
		1: {weeklyRem: 0.5, sessionRem: 0.9, weeklyReset: now.Add(6 * 24 * time.Hour)}, // weight 1
		3: {weeklyRem: 0.8, sessionRem: 0.9, weeklyReset: now.Add(24 * time.Hour)},     // 1 + 3*0.5*0.8   = 2.2
		4: {weeklyRem: 0.8, sessionRem: 0.9, weeklyReset: now.Add(6 * time.Hour)},      // 1 + 3*0.875*0.8 = 3.1
		5: {weeklyRem: 0.0, sessionRem: 0.9},                                           // exhausted
		6: {weeklyRem: 0.7, sessionRem: 0.9, weeklyReset: now.Add(-time.Hour)},         // reset already passed: still usable, no boost
		7: {weeklyRem: 0.9, sessionRem: 0.0},                                           // exhausted
	}
	m.mu.Unlock()
	// key 2 unknown: weight 1. Total weight 8.3.

	const picks = 83
	counts := map[int]int{}
	for range picks {
		order := m.candidateOrder(8, now)
		first := order[0]
		if first == 5 || first == 7 {
			t.Fatalf("exhausted key #%d picked first: %v", first+1, order)
		}
		// Exhausted keys always at the tail, most weekly remaining first.
		if order[6] != 7 || order[7] != 5 {
			t.Fatalf("exhausted tail = %v, want [7 5]", order[6:])
		}
		counts[first]++
	}

	// Smooth WRR keeps each key within one request of its exact share; a
	// passed reset time refills rather than demotes, so key 6 is back to
	// weight 1.
	wantShare := map[int]float64{0: 1, 1: 1, 2: 1, 3: 2.2, 4: 3.1, 6: 1}
	for i, share := range wantShare {
		ideal := share / 9.3 * picks
		if math.Abs(float64(counts[i])-ideal) > 1.5 {
			t.Errorf("key #%d picked %d times, want ~%.1f (weight %.1f)", i+1, counts[i], ideal, share)
		}
	}
}

func TestCandidateOrderEqualWeightsRotate(t *testing.T) {
	m := newUsageMonitor("http://unused.example", NewNullLogger())
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

func TestResetBoost(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		u    keyUsage
		want float64
	}{
		{"unknown reset", keyUsage{weeklyRem: 0.9}, 0},
		{"far from reset", keyUsage{weeklyRem: 0.9, weeklyReset: now.Add(5 * 24 * time.Hour)}, 0},
		{"reset already passed", keyUsage{weeklyRem: 0.9, weeklyReset: now.Add(-time.Hour)}, 0},
		{"half the window, 80% weekly left", keyUsage{weeklyRem: 0.8, weeklyReset: now.Add(24 * time.Hour)}, 3 * 0.5 * 0.8},
		{"at reset, full weekly", keyUsage{weeklyRem: 1, weeklyReset: now.Add(time.Millisecond)}, 3},
		{"at reset, empty weekly", keyUsage{weeklyRem: 0, weeklyReset: now.Add(time.Millisecond)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resetBoost(tt.u, now)
			if math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("resetBoost = %v, want %v", got, tt.want)
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
	gw.refreshUsage()

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
	gw.refreshUsage()
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
	usageStub.weeklyReset["k1"] = 24 * time.Hour // inside the 48h ramp
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	srv, gw := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL, Usage: usageStub.server.URL}
	})
	gw.refreshUsage()

	counts := map[string]int{}
	for range 30 {
		resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		counts[expectAuth(t, seen)]++
	}
	// The key about to lose its leftover weekly quota gets the larger share,
	// but the other key keeps working: weight ramps up, it must not grab all
	// the work.
	if counts["Bearer k1"] <= counts["Bearer k2"] {
		t.Errorf("soon-resetting key did not get the larger share: %v", counts)
	}
	if counts["Bearer k2"] < 3 {
		t.Errorf("other key starved (ramp grabbed all work): %v", counts)
	}
}

func TestProxyAlternatesKeysWhenNoneIsRamping(t *testing.T) {
	usageStub := newZaiUsageStub(t) // both windows reset in days: no ramp
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	srv, gw := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL, Usage: usageStub.server.URL}
	})
	gw.refreshUsage()

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

// An untouched 5-hour window comes back without nextResetTime (live, 2026-11-14):
// its reset is unknown, not the Unix epoch.
func TestUsageWindowWithoutResetTimeHasUnknownReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":200,"data":{"limits":[
			{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":28000,"currentValue":0,"remaining":28000,"percentage":0},
			{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":140000,"currentValue":6490,"nextResetTime":1795068517976}
		]},"success":true}`)
	}))
	defer server.Close()
	m := newUsageMonitor(server.URL, NewNullLogger())
	var u keyUsage
	if err := m.fetchQuota("k", &u); err != nil {
		t.Fatal(err)
	}
	if !u.sessionReset.IsZero() {
		t.Errorf("sessionReset = %v, want zero (unknown)", u.sessionReset)
	}
	if u.weeklyReset.IsZero() {
		t.Error("weeklyReset is zero, want the reported time")
	}
}
