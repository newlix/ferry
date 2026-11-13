package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAffinityIDStableAcrossConversationTurns(t *testing.T) {
	// Turn 2 and 3 only append messages; messages[0] is unchanged.
	turn1 := parseJSONObject([]byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"proj A"},{"role":"user","content":"t1"}]}`))
	turn2 := parseJSONObject([]byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"proj A"},{"role":"user","content":"t1"},{"role":"assistant","content":"a1"},{"role":"user","content":"t2"}]}`))
	turn3 := parseJSONObject([]byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"proj A"},{"role":"user","content":"t1"},{"role":"assistant","content":"a1"},{"role":"user","content":"t2"},{"role":"assistant","content":"a2"},{"role":"user","content":"t3"}]}`))

	id1 := affinityID(turn1, "glm-5.3")
	if id1 == 0 {
		t.Fatal("affinityID = 0, want a session hash")
	}
	if affinityID(turn2, "glm-5.3") != id1 || affinityID(turn3, "glm-5.3") != id1 {
		t.Error("affinityID changed across turns of one conversation")
	}

	other := parseJSONObject([]byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"proj B"},{"role":"user","content":"t1"}]}`))
	if affinityID(other, "glm-5.3") == id1 {
		t.Error("different first message produced the same affinityID")
	}
	if affinityID(turn1, "glm-5.2") == id1 {
		t.Error("different model produced the same affinityID")
	}

	responses := parseJSONObject([]byte(`{"model":"glm-5.3","input":[{"role":"system","content":"proj A"}]}`))
	if affinityID(responses, "glm-5.3") == 0 {
		t.Error("input field (OpenAI Responses protocol) not hashed")
	}
	if affinityID(nil, "glm-5.3") != 0 {
		t.Error("nil object, want 0")
	}
	if affinityID(parseJSONObject([]byte(`{"model":"glm-5.3"}`)), "glm-5.3") != 0 {
		t.Error("no messages/input, want 0")
	}
}

func TestStickySessionsTTL(t *testing.T) {
	var s stickySessions
	now := time.Now()
	s.put(7, 1, now)

	// get refreshes lastUsed (idle TTL: an active conversation keeps its key).
	if got := s.get(7, now.Add(10*time.Minute)); got != 1 {
		t.Fatalf("get before TTL = %d, want 1", got)
	}
	// stickyTTL after the last touch, not after put.
	if got := s.get(7, now.Add(10*time.Minute+stickyTTL+time.Second)); got != -1 {
		t.Fatalf("get after TTL = %d, want -1 (expired)", got)
	}
	if got := s.get(7, now.Add(10*time.Minute+stickyTTL+2*time.Second)); got != -1 {
		t.Fatalf("get after deletion = %d, want -1", got)
	}
	if got := s.get(42, now); got != -1 {
		t.Fatalf("get unknown id = %d, want -1", got)
	}
}

// chatWithPrefix posts a request whose messages[0] is system; later turns
// would append more messages after it.
func chatWithPrefix(t *testing.T, srvURL, system string) {
	t.Helper()
	body := fmt.Sprintf(`{"model":"glm-5.3","messages":[{"role":"system","content":%q},{"role":"user","content":"hi"}]}`, system)
	resp := postJSON(t, srvURL+"/v1/chat/completions", nil, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func upstreamRecordingKeys(t *testing.T, serve func(auth string, w http.ResponseWriter)) (*httptest.Server, chan string) {
	t.Helper()
	seen := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seen <- auth
		serve(auth, w)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func TestStickyConversationSticksToOneKey(t *testing.T) {
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})

	chatWithPrefix(t, srv.URL, "proj A")
	chatWithPrefix(t, srv.URL, "proj A")
	chatWithPrefix(t, srv.URL, "proj A")
	first := expectAuth(t, seen)
	for range 2 {
		if got := expectAuth(t, seen); got != first {
			t.Fatalf("conversation switched keys mid-flight: %q then %q (round-robin would alternate k1/k2)", first, got)
		}
	}
}

func TestStickyDifferentConversationsSpreadAcrossKeys(t *testing.T) {
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})

	chatWithPrefix(t, srv.URL, "proj A")
	chatWithPrefix(t, srv.URL, "proj B")
	keyA1 := expectAuth(t, seen)
	keyB1 := expectAuth(t, seen)
	if keyA1 == keyB1 {
		t.Fatalf("two conversations both started on %q, want different keys", keyA1)
	}
	chatWithPrefix(t, srv.URL, "proj A")
	chatWithPrefix(t, srv.URL, "proj B")
	keyA2 := expectAuth(t, seen)
	keyB2 := expectAuth(t, seen)
	if keyA2 != keyA1 || keyB2 != keyB1 {
		t.Errorf("binding changed: A %q→%q, B %q→%q", keyA1, keyA2, keyB1, keyB2)
	}
}

func TestStickyRebindsAfterRateLimitFailover(t *testing.T) {
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		if auth == "Bearer k1" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Write([]byte(`{}`))
	})
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})

	// First request: starts on k1, fails over to k2, binding moves to k2.
	chatWithPrefix(t, srv.URL, "proj A")
	expectAuth(t, seen) // k1
	expectAuth(t, seen) // k2
	// Second request of the same conversation must go straight to k2.
	chatWithPrefix(t, srv.URL, "proj A")
	if got := expectAuth(t, seen); got != "Bearer k2" {
		t.Fatalf("after failover the conversation restarted on %q, want Bearer k2 directly", got)
	}
}

// A batch client's independent requests share one system prompt; with
// stickiness off they spread across keys instead of piling onto one, and the
// header does not reach z.ai.
func TestStickyOffSpreadsSamePrefixAcrossKeys(t *testing.T) {
	headers := make(chan string, 16)
	upstream, seen := upstreamRecordingKeys(t, func(auth string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	inner := upstream.Config.Handler
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get(stickyHeader)
		inner.ServeHTTP(w, r)
	})
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})
	body := `{"model":"glm-5.3","messages":[{"role":"system","content":"judge"},{"role":"user","content":"line"}]}`
	for range 2 {
		resp := postJSON(t, srv.URL+"/v1/chat/completions", map[string]string{stickyHeader: stickyOff}, body)
		resp.Body.Close()
	}
	first, second := expectAuth(t, seen), expectAuth(t, seen)
	if first == second {
		t.Fatalf("both requests went to %q, want round-robin across keys", first)
	}
	if h := <-headers; h != "" {
		t.Errorf("upstream saw %s: %q, want it dropped", stickyHeader, h)
	}
}
