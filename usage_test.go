package main

import (
	"encoding/json"
	"fmt"
	"io"
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
	expiry   string
	failKeys map[string]bool // keys whose quota fetch returns 500
}

func newZaiUsageStub(t *testing.T) *zaiUsageStub {
	t.Helper()
	stub := &zaiUsageStub{
		weekly:   map[string]float64{"k1": 0.8, "k2": 0.8},
		session:  map[string]float64{"k1": 0.9, "k2": 0.9},
		plan:     "GLM Coding Pro",
		expiry:   time.Now().AddDate(0, 1, 0).Format("2006-01-02"),
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
		fmt.Fprintf(w, `{"code":200,"data":[{"id":"1","productName":%q,"status":"VALID","valid":"2026-01-12 16:55:13-2026-02-12 16:55:13","nextRenewTime":%q,"billingCycle":"monthly","inCurrentPeriod":true}],"success":true}`, stub.plan, stub.expiry)
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
		if u.expiry.Format("2006-01-02") != stub.expiry {
			t.Errorf("key %s: expiry = %v, want %s", k, u.expiry, stub.expiry)
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

func TestCandidateOrderQuotaAware(t *testing.T) {
	m := newUsageMonitor("http://unused.example")
	now := time.Now()
	m.mu.Lock()
	m.state = map[int]keyUsage{
		0: {ok: true, weeklyRem: 0.9, sessionRem: 0.9, expiry: now.AddDate(0, 0, 60)},
		1: {ok: true, weeklyRem: 0.9, sessionRem: 0.9, expiry: now.AddDate(0, 0, 3)},
		2: {ok: true, weeklyRem: 0.2, sessionRem: 0.9, expiry: now.AddDate(0, 0, 30)},
		3: {ok: true, weeklyRem: 0.5, sessionRem: 0.9, expiry: now.AddDate(0, 0, 30)},
		4: {ok: true, weeklyRem: 0.7, sessionRem: 0.9, expiry: now.AddDate(0, 0, -1)}, // expired
		5: {ok: true, weeklyRem: 0.0, sessionRem: 0.9},                                // weekly exhausted
		6: {ok: true, weeklyRem: 0.7, sessionRem: 0.0},                                // session exhausted
	}
	m.mu.Unlock()
	// key 7 has no entry: unknown.

	got := m.candidateOrder(8, now)
	// Soonest expiry first (1); among equal expiry most weekly remaining
	// (3 with 0.5 before 2 with 0.2); then 0. Unknown 7 next. Exhausted last,
	// most weekly remaining first: expired 4 (0.7), session-out 6 (0.7, ties
	// keep index order), weekly-out 5 (0.0).
	want := []int{1, 3, 2, 0, 7, 4, 6, 5}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
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
	if first.Plan != "GLM Coding Pro" || first.Expires != usageStub.expiry {
		t.Errorf("key 1 = %+v, want plan GLM Coding Pro expiring %s", first, usageStub.expiry)
	}
	if first.Weekly < 79 || first.Weekly > 81 {
		t.Errorf("weekly = %.1f, want ~80", first.Weekly)
	}
}
