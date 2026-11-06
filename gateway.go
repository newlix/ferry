package main

import (
	"bytes"
	"context"
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
	cfg        Config
	client     *http.Client // no overall timeout: streams stay open as long as both sides do
	sticky     stickySessions
	usage      *usageMonitor
	refreshing atomic.Bool
}

func NewGateway(cfg Config) *Gateway {
	return &Gateway{
		cfg:    cfg,
		client: &http.Client{},
		usage:  newUsageMonitor(cfg.Upstreams.Usage),
	}
}

// RefreshUsage synchronously refetches quota data for all keys.
func (g *Gateway) RefreshUsage() {
	g.usage.refresh(g.cfg.APIKeys)
}

// refreshUsageAsync refetches quota data at most once at a time; the failover
// path calls it after a 429 because its cached data is stale.
func (g *Gateway) refreshUsageAsync() {
	if !g.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer g.refreshing.Store(false)
		g.usage.refresh(g.cfg.APIKeys)
	}()
}

// StartUsageLoop refreshes quota data in the background every
// usageRefreshInterval until ctx is done. Never blocks startup: until the
// first refresh lands, key selection falls back to round-robin.
func (g *Gateway) StartUsageLoop(ctx context.Context) {
	g.refreshUsageAsync()
	go func() {
		ticker := time.NewTicker(usageRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				g.usage.refresh(g.cfg.APIKeys)
			}
		}
	}()
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
	case "/v1/usage":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		if !g.authRequest(r) {
			gatewayAuthError(w, false)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": g.usage.snapshot(len(g.cfg.APIKeys))})
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

	obj := parseJSONObject(body)
	requested, mapped, body := applyModelAlias(obj, body, g.cfg.Models)

	u, err := url.Parse(rt.base + rt.path)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("bad upstream URL: %v", err), rt.anthropic)
		return
	}
	u.RawQuery = r.URL.RawQuery

	// Key order comes from quota data (see usageMonitor.candidateOrder):
	// smooth weighted round-robin over the usable keys — equal weights, so
	// traffic spreads evenly, unless a key nears plan expiry and its weight
	// ramps up gradually. Exhausted or expired keys are tried last. A
	// conversation with a stable message prefix sticks to its key while that
	// key is still usable (prompt-cache friendly on multi-account setups);
	// the failover loop below rebinds it. A retry happens only while nothing
	// has been written to the client yet, which is always the case here: the
	// response body is forwarded only after the loop picked the final
	// response.
	order := g.usage.candidateOrder(len(g.cfg.APIKeys), start)
	sessionID := uint64(0)
	if len(g.cfg.APIKeys) > 1 {
		sessionID = affinityID(obj, mapped)
		if sessionID != 0 {
			if k := g.sticky.get(sessionID, start); k >= 0 && g.usage.usable(k) {
				rest := make([]int, 0, len(order)-1)
				for _, idx := range order {
					if idx != k {
						rest = append(rest, idx)
					}
				}
				order = append([]int{k}, rest...)
			}
		}
	}
	var resp *http.Response
	keyIdx := order[0]
	for i, idx := range order {
		keyIdx = idx
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
		if !slices.Contains([]int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests}, resp.StatusCode) || i == len(order)-1 {
			break
		}
		reason := peekError(resp.Body)
		resp.Body.Close()
		next := order[(i+1)%len(order)]
		log.Printf("%s %s model=%s key=#%d status=%d, trying key #%d: %s", r.Method, r.URL.Path, modelLabel(requested, mapped), keyIdx+1, resp.StatusCode, next+1, reason)
		// The cached quota data said this key had room; refetch it.
		g.refreshUsageAsync()
	}
	defer resp.Body.Close()

	// Rebind even when the loop never rotated: this refreshes lastUsed.
	if sessionID != 0 {
		g.sticky.put(sessionID, keyIdx, start)
	}

	out := newStreamWriter(w)
	copyForwardedHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(out, resp.Body); err != nil {
		log.Printf("%s %s: copy response: %v", r.Method, r.URL.Path, err)
	}
	sessionLabel := ""
	if sessionID != 0 {
		sessionLabel = fmt.Sprintf(" session=%08x", sessionID)
	}
	log.Printf("%s %s model=%s key=#%d%s status=%d in=%dB out=%dB dur=%s", r.Method, r.URL.Path, modelLabel(requested, mapped), keyIdx+1, sessionLabel, resp.StatusCode, len(body), out.n, time.Since(start).Round(time.Millisecond))
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

// parseJSONObject returns nil for anything that is not a JSON object.
func parseJSONObject(body []byte) map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	return obj
}

// applyModelAlias maps the top-level "model" through the alias map. The body
// is rebuilt from json.RawMessage so every other value keeps its exact bytes
// (int64 precision, string escaping).
func applyModelAlias(obj map[string]json.RawMessage, body []byte, aliases map[string]string) (requested, mapped string, out []byte) {
	if obj == nil {
		return "", "", body
	}
	raw, ok := obj["model"]
	if !ok || json.Unmarshal(raw, &requested) != nil {
		return "", "", body
	}
	target, ok := aliases[requested]
	if !ok || target == "" || target == requested {
		return requested, requested, body
	}
	modelJSON, err := json.Marshal(target)
	if err != nil {
		return requested, requested, body
	}
	obj["model"] = modelJSON
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return requested, requested, body
	}
	return requested, target, rewritten
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
