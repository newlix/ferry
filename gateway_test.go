package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func newTestGateway(t *testing.T, mutate func(*Config)) (*httptest.Server, Config) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.APIKeys = []string{"k1", "k2"}
	if mutate != nil {
		mutate(&cfg)
	}
	srv := httptest.NewServer(NewGateway(cfg))
	t.Cleanup(srv.Close)
	return srv, cfg
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

func TestProxyRewritesModelAndInjectsKey(t *testing.T) {
	upstream, requests := runUpstream(t, http.StatusOK, `{"id":"ok"}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
		c.Models["fast"] = "glm-5.3-flash"
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
	var sent map[string]any
	if err := json.Unmarshal(up.body, &sent); err != nil {
		t.Fatalf("upstream body %s: %v", up.body, err)
	}
	if sent["model"] != "glm-5.3-flash" {
		t.Errorf("upstream model = %v, want glm-5.3-flash", sent["model"])
	}
	if fmt.Sprint(sent["messages"]) != `[map[content:hi role:user]]` {
		t.Errorf("upstream messages = %v, want original messages preserved", sent["messages"])
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

func TestProxyExhaustedKeysReturnsUpstreamError(t *testing.T) {
	upstream, _ := runUpstream(t, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
		c.APIKeys = []string{"k1"}
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

func TestGatewayListenKey(t *testing.T) {
	upstream, requests := runUpstream(t, http.StatusOK, `{}`)
	srv, _ := newTestGateway(t, func(c *Config) {
		c.ListenKey = "sekret"
		c.Upstreams = Upstreams{OpenAIChat: upstream.URL, OpenAIResponses: upstream.URL, Anthropic: upstream.URL}
	})

	resp := postJSON(t, srv.URL+"/v1/chat/completions", nil, `{"model":"glm-5.3"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without key = %d, want 401", resp.StatusCode)
	}

	for _, headers := range []map[string]string{
		{"Authorization": "Bearer sekret"},
		{"X-Api-Key": "sekret"},
	} {
		resp := postJSON(t, srv.URL+"/v1/chat/completions", headers, `{"model":"glm-5.3"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status with headers %v = %d, want 200", headers, resp.StatusCode)
		}
		expectRequest(t, requests)
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

func TestApplyModelAlias(t *testing.T) {
	aliases := map[string]string{"fast": "glm-5.3-flash"}

	in := []byte(`{"model":"fast","seed":9007199254740993,"messages":[{"role":"user"}]}`)
	requested, mapped, out := applyModelAlias(parseJSONObject(in), in, aliases)
	if requested != "fast" || mapped != "glm-5.3-flash" {
		t.Errorf("requested = %q, mapped = %q, want fast -> glm-5.3-flash", requested, mapped)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["model"]) != `"glm-5.3-flash"` {
		t.Errorf("model = %s, want \"glm-5.3-flash\"", got["model"])
	}
	// RawMessage passthrough keeps int64 precision that float64 round-trips lose.
	if string(got["seed"]) != "9007199254740993" {
		t.Errorf("seed = %s, want 9007199254740993 kept exactly", got["seed"])
	}

	in = []byte(`{"model":"unknown","x":1}`)
	requested, mapped, out = applyModelAlias(parseJSONObject(in), in, aliases)
	if requested != "unknown" || mapped != "unknown" || !bytes.Equal(out, in) {
		t.Errorf("unknown model: requested = %q, mapped = %q, out = %s, want passthrough", requested, mapped, out)
	}

	in = []byte(`not json`)
	requested, mapped, out = applyModelAlias(parseJSONObject(in), in, aliases)
	if requested != "" || mapped != "" || !bytes.Equal(out, in) {
		t.Errorf("non-JSON body: requested = %q, mapped = %q, out = %s, want passthrough", requested, mapped, out)
	}

	in = []byte(`{"messages":[]}`)
	requested, mapped, out = applyModelAlias(parseJSONObject(in), in, aliases)
	if requested != "" || mapped != "" || !bytes.Equal(out, in) {
		t.Errorf("missing model: requested = %q, mapped = %q, out = %s, want passthrough", requested, mapped, out)
	}
}
