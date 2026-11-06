package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
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

type upstream struct {
	base      string // upstream base URL, e.g. https://api.z.ai/api/anthropic
	path      string // appended to base, e.g. /v1/messages
	anthropic bool   // also authenticate with x-api-key, for the Anthropic protocol
}

type route struct {
	method string
	path   string
	handle http.HandlerFunc
}

// Logger splits output by importance: Quiet lines always print (startup,
// key rotations, quota fetch failures); Verbose lines only with -v (one line
// per request, quota refresh results). Quiet is the product; Verbose is
// debugging.
type Logger struct {
	Quiet   *log.Logger
	Verbose *log.Logger
}

func NewLogger(verbose bool) Logger {
	out := io.Discard
	if verbose {
		out = os.Stderr
	}
	return Logger{
		Quiet:   log.New(os.Stderr, "", log.LstdFlags),
		Verbose: log.New(out, "", log.LstdFlags),
	}
}

func NewNullLogger() Logger {
	discard := log.New(io.Discard, "", 0)
	return Logger{Quiet: discard, Verbose: discard}
}

type Gateway struct {
	cfg        Config
	routes     []route
	client     *http.Client // no overall timeout: streams stay open as long as both sides do
	sticky     stickySessions
	usage      *usageMonitor
	refreshing atomic.Bool
	log        Logger
}

func NewGateway(cfg Config, log Logger) *Gateway {
	g := &Gateway{
		cfg:    cfg,
		client: &http.Client{},
		usage:  newUsageMonitor(cfg.Upstreams.Usage, log),
		log:    log,
	}
	g.routes = []route{
		{http.MethodGet, "/health", g.handleHealth},
		{http.MethodGet, "/v1/models", g.handleModels},
		{http.MethodGet, "/v1/usage", g.handleUsage},
		{http.MethodPost, "/v1/chat/completions", g.proxyRoute(upstream{base: cfg.Upstreams.OpenAIChat, path: "/chat/completions"})},
		{http.MethodPost, "/v1/responses", g.proxyRoute(upstream{base: cfg.Upstreams.OpenAIResponses, path: "/responses"})},
		{http.MethodPost, "/v1/messages", g.proxyRoute(upstream{base: cfg.Upstreams.Anthropic, path: "/v1/messages", anthropic: true})},
		{http.MethodPost, "/v1/messages/count_tokens", g.proxyRoute(upstream{base: cfg.Upstreams.Anthropic, path: "/v1/messages/count_tokens", anthropic: true})},
	}
	return g
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
	for _, rt := range g.routes {
		if r.URL.Path != rt.path {
			continue
		}
		if r.Method != rt.method {
			methodNotAllowed(w, rt.method)
			return
		}
		rt.handle(w, r)
		return
	}
	http.NotFound(w, r)
}

func (g *Gateway) proxyRoute(u upstream) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.proxy(w, r, u)
	}
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (g *Gateway) handleUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": g.usage.snapshot(len(g.cfg.APIKeys))})
}

func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request, u upstream) {
	start := time.Now()
	if len(g.cfg.APIKeys) == 0 {
		writeError(w, http.StatusServiceUnavailable, "glms has no keys; add one: glms key add", u.anthropic)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("read request body: %v", err), u.anthropic)
		return
	}

	obj := parseJSONObject(body)
	requested, mapped, body := applyModelAlias(obj, body, g.cfg.Models)

	target, err := url.Parse(u.base + u.path)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("bad upstream URL: %v", err), u.anthropic)
		return
	}
	target.RawQuery = r.URL.RawQuery

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
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("build upstream request: %v", err), u.anthropic)
			return
		}
		copyForwardedHeaders(req.Header, r.Header)
		setUpstreamAuth(req.Header, u, g.cfg.APIKeys[keyIdx].Secret)

		resp, err = g.client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("could not reach z.ai: %v", err), u.anthropic)
			g.log.Verbose.Printf("%s %s model=%s key=%s status=upstream-error dur=%s: %v", r.Method, r.URL.Path, modelLabel(requested, mapped), g.cfg.APIKeys[keyIdx].Name, time.Since(start).Round(time.Millisecond), err)
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
		g.log.Quiet.Printf("%s %s model=%s key=%s status=%d, trying key %s: %s", r.Method, r.URL.Path, modelLabel(requested, mapped), g.cfg.APIKeys[keyIdx].Name, resp.StatusCode, g.cfg.APIKeys[next].Name, reason)
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
		g.log.Quiet.Printf("%s %s: copy response: %v", r.Method, r.URL.Path, err)
	}
	sessionLabel := ""
	if sessionID != 0 {
		sessionLabel = fmt.Sprintf(" conv=%08x", sessionID)
	}
	g.log.Verbose.Printf("%s %s model=%s key=%s%s status=%d in=%dB out=%dB dur=%s", r.Method, r.URL.Path, modelLabel(requested, mapped), g.cfg.APIKeys[keyIdx].Name, sessionLabel, resp.StatusCode, len(body), out.n, time.Since(start).Round(time.Millisecond))
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	names := g.cfg.ModelNames()
	data := make([]map[string]string, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]string{"id": name, "object": "model", "owned_by": "z-ai"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
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

func setUpstreamAuth(h http.Header, u upstream, key string) {
	h.Set("Authorization", "Bearer "+key)
	if u.anthropic {
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
