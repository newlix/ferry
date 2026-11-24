package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type capturedRequest struct {
	auth    string
	apiKey  string
	path    string
	query   string
	version string
	body    []byte
}

// runUpstream starts an upstream stub that sends every request it receives
// to the channel and replies with status and body.
func runUpstream(t *testing.T, status int, body string) (*httptest.Server, chan capturedRequest) {
	t.Helper()
	requests := make(chan capturedRequest, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		requests <- capturedRequest{
			auth:    r.Header.Get("Authorization"),
			apiKey:  r.Header.Get("X-Api-Key"),
			path:    r.URL.Path,
			query:   r.URL.RawQuery,
			version: r.Header.Get("Anthropic-Version"),
			body:    b,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, requests
}

func expectRequest(t *testing.T, requests chan capturedRequest) capturedRequest {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("upstream received no request within 5s")
		return capturedRequest{}
	}
}

func expectAuth(t *testing.T, seen chan string) string {
	t.Helper()
	select {
	case auth := <-seen:
		return auth
	case <-time.After(5 * time.Second):
		t.Fatal("upstream received no request within 5s")
		return ""
	}
}

func newTestGateway(t *testing.T, mutate func(*Config)) (*httptest.Server, *Gateway) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.APIKeys = []apiKey{{Name: "key1", Secret: "k1"}, {Name: "key2", Secret: "k2"}}
	if mutate != nil {
		mutate(&cfg)
	}
	gw := NewGateway(cfg, &fileStore{dir: t.TempDir()}, NewNullLogger())
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv, gw
}

func postJSON(t *testing.T, url string, headers map[string]string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func TestProxyForwardsBodyUnchangedAndInjectsKey(t *testing.T) {
	upstream, requests := runUpstream(t, http.StatusOK, `{"id":"ok"}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})

	in := `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`
	resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != `{"id":"ok"}` {
		t.Errorf("response body = %s, want {\"id\":\"ok\"}", gotBody)
	}

	up := expectRequest(t, requests)
	if up.auth != "Bearer k1" {
		t.Errorf("upstream Authorization = %q, want Bearer k1", up.auth)
	}
	if up.apiKey != "" {
		t.Errorf("upstream X-Api-Key = %q, want empty on OpenAI route", up.apiKey)
	}
	if string(up.body) != in {
		t.Errorf("upstream body = %s, want %s unchanged", up.body, in)
	}
}

func TestProxyRoutesPathMapping(t *testing.T) {
	upstream, requests := runUpstream(t, http.StatusOK, `{}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})

	tests := []struct {
		route     string
		wantPath  string
		wantQuery string
	}{
		{"/v1/chat/completions", "/chat/completions", ""},
		{"/v1/responses", "/responses", ""},
		{"/v1/messages", "/v1/messages", ""},
		{"/v1/messages?beta=true", "/v1/messages", "beta=true"},
		{"/v1/messages/count_tokens", "/v1/messages/count_tokens", ""},
	}
	for _, tt := range tests {
		t.Run(tt.route, func(t *testing.T) {
			resp := postJSON(t, srv.URL+tt.route, nil, `{"model":"glm-5.3"}`)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			up := expectRequest(t, requests)
			if up.path != tt.wantPath {
				t.Errorf("upstream path = %q, want %q", up.path, tt.wantPath)
			}
			if up.query != tt.wantQuery {
				t.Errorf("upstream query = %q, want %q", up.query, tt.wantQuery)
			}
		})
	}
}

func TestProxyAnthropicAuthHeadersForwarded(t *testing.T) {
	upstream, requests := runUpstream(t, http.StatusOK, `{}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})

	resp := postJSON(t, srv.URL+"/v1/messages", map[string]string{"Anthropic-Version": "2023-06-01"}, `{"model":"glm-5.3"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	up := expectRequest(t, requests)
	if up.auth != "Bearer k1" {
		t.Errorf("upstream Authorization = %q, want Bearer k1", up.auth)
	}
	if up.apiKey != "k1" {
		t.Errorf("upstream X-Api-Key = %q, want k1 on Anthropic route", up.apiKey)
	}
	if up.version != "2023-06-01" {
		t.Errorf("upstream Anthropic-Version = %q, want 2023-06-01 forwarded", up.version)
	}
}

func TestProxyFailsOverOnRateLimit(t *testing.T) {
	// k1 is rate limited, k2 succeeds.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer k1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})

	resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 after failover (body: %s)", resp.StatusCode, body)
	}
}

// fixedRoute pins a test gateway's routing to key2, the way loadKeys does.
func fixedRoute(c *Config) {
	c.Route = "key2"
	if err := resolveRoute(c); err != nil {
		panic(err)
	}
}

func TestFixedRoutePinsOneKey(t *testing.T) {
	upstream, requests := runUpstream(t, http.StatusOK, `{"id":"ok"}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
		fixedRoute(c)
	})

	in := `{"model":"glm-5.3"}`
	for range 3 {
		resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, in)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}
	for range 3 {
		if up := expectRequest(t, requests); up.auth != "Bearer k2" {
			t.Fatalf("upstream Authorization = %q, want Bearer k2 (fixed routing)", up.auth)
		}
	}
	select {
	case up := <-requests:
		t.Fatalf("unexpected extra upstream request with %s", up.auth)
	case <-time.After(100 * time.Millisecond):
	}
}

// Reload must apply a route change to the running gateway: pinning a key in
// the config file takes over without a restart.
func TestReloadAppliesRouteChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	store := &fileStore{dir: dir}
	if err := store.Add("key1", "k1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Add("key2", "k2"); err != nil {
		t.Fatal(err)
	}
	upstream, requests := runUpstream(t, http.StatusOK, `{"id":"ok"}`)
	cfg := DefaultConfig()
	cfg.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	if err := loadKeys(&cfg, store); err != nil {
		t.Fatal(err)
	}
	gw := NewGateway(cfg, store, NewNullLogger())
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)

	post := func() string {
		resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		return expectRequest(t, requests).auth
	}
	if auth := post(); auth != "Bearer k1" {
		t.Fatalf("before reload: %q, want Bearer k1", auth)
	}

	if err := writeRoute(cfgPath, "key2"); err != nil {
		t.Fatal(err)
	}
	if err := gw.Reload(cfgPath); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if auth := post(); auth != "Bearer k2" {
		t.Fatalf("after reload: %q, want Bearer k2 (fixed routing applied live)", auth)
	}
}

// Fixed routing never rotates: the pinned key's 429 forwards after the hold
// deadline instead of trying another key.
func TestFixedRouteDoesNotRotateOnRateLimit(t *testing.T) {
	upstream, requests := runUpstream(t, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
		c.RetryWait = 0 // no holding: the 429 forwards at once
		fixedRoute(c)
	})

	resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the pinned key's 429 forwarded", resp.StatusCode)
	}
	if up := expectRequest(t, requests); up.auth != "Bearer k2" {
		t.Fatalf("upstream Authorization = %q, want Bearer k2", up.auth)
	}
	select {
	case up := <-requests:
		t.Fatalf("rotated to another key: %s", up.auth)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestProxyExhaustedKeysReturnsUpstreamError(t *testing.T) {
	upstream, _ := runUpstream(t, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
		c.APIKeys = []apiKey{{Name: "key1", Secret: "k1"}}
		c.RetryWait = 0 // disable holding: this test pins the pass-through
	})

	resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("rate limited")) {
		t.Errorf("body = %s, want upstream error body passed through", body)
	}
}

func TestProxyHoldsRequestUntilAKeyRecovers(t *testing.T) {
	// Both keys answer 429 on the first sweep; k2 recovers before the hold
	// deadline, so the client sees 200 instead of the 429 that would kill an
	// agent run. main's short-window 1302 and fool's weekly 1310 in one
	// sweep is the live incident this reproduces (2026-11-18).
	var k2Calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer k2" {
			// k1 is the live main-key stand-in: its short-window rate limit
			// never clears within the test.
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		if k2Calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
		c.RetryWait = Duration(3 * time.Second) // short deadline: the first backoff is clamped to it
	})

	began := time.Now()
	resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 after the held retry (body: %s)", resp.StatusCode, body)
	}
	if elapsed := time.Since(began); elapsed < 2*time.Second {
		t.Errorf("client waited %s, want the clamped hold (~3s) before the retry", elapsed.Round(time.Millisecond))
	}
	if k2Calls.Load() < 2 {
		t.Errorf("k2 got %d calls, want >=2 (429, then the recovered retry)", k2Calls.Load())
	}
}

func TestProxyHoldDeadlineForwardsLastResponse(t *testing.T) {
	upstream, _ := runUpstream(t, http.StatusTooManyRequests, `{"error":{"message":"still limited"}}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
		c.RetryWait = Duration(time.Second)
	})

	began := time.Now()
	resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 forwarded after the deadline", resp.StatusCode)
	}
	if elapsed := time.Since(began); elapsed < time.Second {
		t.Errorf("client waited %s, want the hold to run to the deadline", elapsed.Round(time.Millisecond))
	}
}

func TestProxyStreamsIncrementally(t *testing.T) {
	// The upstream holds the stream open until release; if the gateway
	// buffered instead of streaming, the first chunk would never arrive
	// before upstream EOF and the test would time out.
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: chunk1\n\n")
		flusher.Flush()
		<-release
		fmt.Fprint(w, "data: chunk2\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})

	resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3","stream":true}`)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	first := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil {
			first <- fmt.Sprintf("read error: %v", err)
			return
		}
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: chunk1\n" {
			t.Fatalf("first chunk = %q, want \"data: chunk1\\n\"", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first chunk not received while upstream still open: gateway buffers streams")
	}
	close(release)
}

func TestGatewayNeedsNoClientAuth(t *testing.T) {
	// localhost is the boundary: no gateway key, any Authorization header is
	// simply replaced with the upstream key.
	upstream, requests := runUpstream(t, http.StatusOK, `{}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})

	resp := postJSON(t, srv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer whatever"}, `{"model":"glm-5.3"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if up := expectRequest(t, requests); up.auth != "Bearer k1" {
		t.Errorf("upstream Authorization = %q, want Bearer k1", up.auth)
	}
}

func TestModelsAndHealthAndUnknownRoutes(t *testing.T) {
	srv, _ := newTestGateway(t, nil)

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range list.Data {
		ids[m.ID] = true
	}
	for _, want := range []string{"glm-5.3", "glm-5.3-flash", "glm-5.2"} {
		if !ids[want] {
			t.Errorf("/v1/models missing %q, got %v", want, ids)
		}
	}

	resp, err = http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET chat/completions status = %d, want 405", resp.StatusCode)
	}

	resp, err = http.Post(srv.URL+"/v1/nope", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown route status = %d, want 404", resp.StatusCode)
	}
}

func TestRequestModel(t *testing.T) {
	for in, want := range map[string]string{
		`{"model":"fast","x":1}`: "fast",
		`{"messages":[]}`:        "",
		`{"model":7}`:            "",
		`not json`:               "",
	} {
		if got := requestModel(parseJSONObject([]byte(in))); got != want {
			t.Errorf("requestModel(%s) = %q, want %q", in, got, want)
		}
	}
}

// quotaBody is a z.ai 5-hour quota error in the Anthropic shape, as logged
// live on 2026-11-23.
func quotaBody(reset string) string {
	return `{"type":"error","error":{"type":"rate_limit_error","code":"1308","message":"[1308][Usage limit reached for 5 hour. Your limit will reset at ` + reset + `][20261123164705448118f4ee28402b]"}}`
}

func TestProxyQuotaExhaustedAnswersAtOnceWithEarliestReset(t *testing.T) {
	// Both keys' 5-hour pools are empty: holding cannot help, so the client
	// gets the 429 at once, carrying the reset of the key that refills first.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		if r.Header.Get("Authorization") == "Bearer k1" {
			w.Write([]byte(quotaBody("2026-11-23 19:10:27")))
			return
		}
		w.Write([]byte(quotaBody("2026-11-23 19:09:01")))
	}))
	defer upstream.Close()

	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
		c.RetryWait = Duration(3 * time.Second)
	})

	began := time.Now()
	resp := postJSON(t, srv.URL+"/v1/messages", nil, `{"model":"glm-5.3"}`)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body: %s)", resp.StatusCode, body)
	}
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Errorf("client waited %s, want the 429 at once", elapsed.Round(time.Millisecond))
	}
	if !bytes.Contains(body, []byte("19:09:01")) {
		t.Errorf("body = %s, want the earliest reset (19:09:01)", body)
	}
}

func TestProxyHoldsWhenOneKeyIsOnlyThrottled(t *testing.T) {
	// k1's 5-hour pool is empty but k2 is only throttled (1302) and recovers:
	// the hold still applies, since one key can clear within it.
	var k2Calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer k1" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(quotaBody("2026-11-23 19:10:27")))
			return
		}
		if k2Calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":"1302","message":"Rate limit reached for requests"}}`))
			return
		}
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
		c.RetryWait = Duration(3 * time.Second)
	})

	resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 after the held retry (body: %s)", resp.StatusCode, body)
	}
}

func TestUpstreamErrorCode(t *testing.T) {
	for _, tc := range []struct{ body, code, reset string }{
		{quotaBody("2026-11-23 19:10:27"), "1308", "2026-11-23 19:10:27"},
		{`{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at 2026-11-23 19:10:27"}}`, "1308", "2026-11-23 19:10:27"},
		{`{"error":{"code":1302,"message":"Rate limit reached for requests"}}`, "1302", ""},
		{`not json`, "", ""},
	} {
		code, reset := upstreamErrorCode([]byte(tc.body))
		if code != tc.code || reset != tc.reset {
			t.Errorf("upstreamErrorCode(%s) = %q, %q; want %q, %q", tc.body, code, reset, tc.code, tc.reset)
		}
	}
}
