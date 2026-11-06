package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// maxRequestBody bounds how much of a client request is buffered for model
// rewriting and key-failover replay.
const maxRequestBody = 512 << 20

// maxLoggedErrorBody bounds how much of an upstream error body is read for
// logging when a key is rotated.
const maxLoggedErrorBody = 8 << 10

type route struct {
	base      string // upstream base URL, e.g. https://api.z.ai/api/anthropic
	path      string // appended to base, e.g. /v1/messages
	anthropic bool   // also authenticate with x-api-key, for the Anthropic protocol
}

type Gateway struct {
	cfg     Config
	client  *http.Client // no overall timeout: streams stay open as long as both sides do
	nextKey atomic.Uint64
}

func NewGateway(cfg Config) *Gateway {
	return &Gateway{cfg: cfg, client: &http.Client{}}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/v1/models":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		if !g.authRequest(r) {
			gatewayAuthError(w, false)
			return
		}
		g.handleModels(w)
	case "/v1/chat/completions":
		g.proxy(w, r, route{base: g.cfg.Upstreams.OpenAIChat, path: "/chat/completions"})
	case "/v1/responses":
		g.proxy(w, r, route{base: g.cfg.Upstreams.OpenAIResponses, path: "/responses"})
	case "/v1/messages":
		g.proxy(w, r, route{base: g.cfg.Upstreams.Anthropic, path: "/v1/messages", anthropic: true})
	case "/v1/messages/count_tokens":
		g.proxy(w, r, route{base: g.cfg.Upstreams.Anthropic, path: "/v1/messages/count_tokens", anthropic: true})
	default:
		http.NotFound(w, r)
	}
}

func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request, rt route) {
	start := time.Now()
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !g.authRequest(r) {
		gatewayAuthError(w, rt.anthropic)
		return
	}
	if len(g.cfg.APIKeys) == 0 {
		writeError(w, http.StatusServiceUnavailable, "gateway has no upstream API keys configured", rt.anthropic)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("read request body: %v", err), rt.anthropic)
		return
	}

	requested, body := rewriteModel(body, g.cfg.Models)
	mapped := requested
	if target, ok := g.cfg.Models[requested]; ok && target != "" {
		mapped = target
	}

	u, err := url.Parse(rt.base + rt.path)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("bad upstream URL: %v", err), rt.anthropic)
		return
	}
	u.RawQuery = r.URL.RawQuery

	// Keys are tried round-robin, starting from a rotating position so a
	// rate-limited key is not hit first on every request. A retry happens
	// only while nothing has been written to the client yet, which is always
	// the case here: the response body is forwarded only after the loop
	// picked the final response.
	var resp *http.Response
	keyIdx := 0
	keyStart := int(g.nextKey.Add(1)-1) % len(g.cfg.APIKeys)
	for i := range g.cfg.APIKeys {
		keyIdx = (keyStart + i) % len(g.cfg.APIKeys)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(body))
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("build upstream request: %v", err), rt.anthropic)
			return
		}
		copyForwardedHeaders(req.Header, r.Header)
		setUpstreamAuth(req.Header, rt, g.cfg.APIKeys[keyIdx])

		resp, err = g.client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("upstream request failed: %v", err), rt.anthropic)
			log.Printf("%s %s model=%s key=#%d status=upstream-error dur=%s: %v", r.Method, r.URL.Path, modelLabel(requested, mapped), keyIdx+1, time.Since(start).Round(time.Millisecond), err)
			return
		}
		// 401/403: key invalid or expired. 429: plan rate limit. These are the
		// failures another key can fix; everything else is passed through.
		if !slices.Contains([]int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests}, resp.StatusCode) || i == len(g.cfg.APIKeys)-1 {
			break
		}
		reason := peekError(resp.Body)
		resp.Body.Close()
		next := (keyIdx + 1) % len(g.cfg.APIKeys)
		log.Printf("%s %s model=%s key=#%d status=%d, trying key #%d: %s", r.Method, r.URL.Path, modelLabel(requested, mapped), keyIdx+1, resp.StatusCode, next+1, reason)
	}
	defer resp.Body.Close()

	out := newStreamWriter(w)
	copyForwardedHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(out, resp.Body); err != nil {
		log.Printf("%s %s: copy response: %v", r.Method, r.URL.Path, err)
	}
	log.Printf("%s %s model=%s key=#%d status=%d in=%dB out=%dB dur=%s", r.Method, r.URL.Path, modelLabel(requested, mapped), keyIdx+1, resp.StatusCode, len(body), out.n, time.Since(start).Round(time.Millisecond))
}

func (g *Gateway) handleModels(w http.ResponseWriter) {
	names := g.cfg.ModelNames()
	data := make([]map[string]string, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]string{"id": name, "object": "model", "owned_by": "z-ai"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// authRequest checks the client-facing listenKey; empty means open access,
// which is safe because the default listen address is localhost only.
func (g *Gateway) authRequest(r *http.Request) bool {
	if g.cfg.ListenKey == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.Header.Get("X-Api-Key")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(g.cfg.ListenKey)) == 1
}

// rewriteModel replaces the top-level "model" field through the alias map.
// The body is rebuilt from json.RawMessage so every other value keeps its
// exact bytes (int64 precision, string escaping).
func rewriteModel(body []byte, aliases map[string]string) (requested string, out []byte) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", body
	}
	raw, ok := obj["model"]
	if !ok {
		return "", body
	}
	if err := json.Unmarshal(raw, &requested); err != nil {
		return "", body
	}
	target, ok := aliases[requested]
	if !ok || target == requested {
		return requested, body
	}
	modelJSON, err := json.Marshal(target)
	if err != nil {
		return requested, body
	}
	obj["model"] = modelJSON
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return requested, body
	}
	return requested, rewritten
}

// copyForwardedHeaders copies end-to-end headers in either direction,
// dropping credentials (replaced upstream) and hop-by-hop fields.
func copyForwardedHeaders(dst, src http.Header) {
	for name, vals := range src {
		switch http.CanonicalHeaderKey(name) {
		case "Authorization", "X-Api-Key", "Host", "Content-Length", "Accept-Encoding":
			continue
		}
		switch http.CanonicalHeaderKey(name) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
			continue
		}
		for _, v := range vals {
			dst.Add(name, v)
		}
	}
}

func setUpstreamAuth(h http.Header, rt route, key string) {
	h.Set("Authorization", "Bearer "+key)
	if rt.anthropic {
		h.Set("X-Api-Key", key)
	}
}

// streamWriter flushes after every write so SSE chunks reach the client as
// the upstream produces them, and counts forwarded bytes for the log line.
type streamWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	n       int64
}

func newStreamWriter(w http.ResponseWriter) *streamWriter {
	f, _ := w.(http.Flusher)
	return &streamWriter{w: w, flusher: f}
}

func (s *streamWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.n += int64(n)
	if err == nil && s.flusher != nil {
		s.flusher.Flush()
	}
	return n, err
}

func peekError(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, maxLoggedErrorBody))
	if err != nil {
		return fmt.Sprintf("(unreadable: %v)", err)
	}
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func modelLabel(requested, mapped string) string {
	switch {
	case requested == "":
		return "-"
	case requested == mapped:
		return requested
	default:
		return requested + "->" + mapped
	}
}

func gatewayAuthError(w http.ResponseWriter, anthropic bool) {
	writeError(w, http.StatusUnauthorized, "invalid gateway key (listenKey)", anthropic)
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf("method not allowed, use %s", allow), false)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string, anthropic bool) {
	if anthropic {
		writeJSON(w, status, map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "gateway_error", "message": msg},
		})
		return
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg}})
}
