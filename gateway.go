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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/at-least/ciderui"
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
	assets     http.Handler // the home page's stylesheet and fonts
	log        Logger
}

func NewGateway(cfg Config, log Logger) *Gateway {
	g := &Gateway{
		cfg:    cfg,
		client: &http.Client{},
		usage:  newUsageMonitor(cfg.Upstreams.Usage, log),
		log:    log,
		assets: ciderui.Assets(),
	}
	g.routes = []route{
		{http.MethodGet, "/", g.handleHome},
		{http.MethodGet, "/health", g.handleHealth},
		{http.MethodGet, "/v1/models", g.handleModels},
		{http.MethodGet, "/v1/quota", g.handleQuota},
		{http.MethodPost, "/v1/chat/completions", g.proxyRoute(upstream{base: cfg.Upstreams.OpenAIChat, path: "/chat/completions"})},
		{http.MethodPost, "/v1/responses", g.proxyRoute(upstream{base: cfg.Upstreams.OpenAIResponses, path: "/responses"})},
		{http.MethodPost, "/v1/messages", g.proxyRoute(upstream{base: cfg.Upstreams.Anthropic, path: "/v1/messages", anthropic: true})},
		{http.MethodPost, "/v1/messages/count_tokens", g.proxyRoute(upstream{base: cfg.Upstreams.Anthropic, path: "/v1/messages/count_tokens", anthropic: true})},
	}
	return g
}

// refreshUsage synchronously refetches quota data for all keys.
func (g *Gateway) refreshUsage() {
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
	if strings.HasPrefix(r.URL.Path, ciderui.AssetsPath) {
		g.assets.ServeHTTP(w, r)
		return
	}
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

func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request, u upstream) {
	start := time.Now()
	if len(g.cfg.APIKeys) == 0 {
		writeError(w, http.StatusServiceUnavailable, "ferry has no keys; add one: ferry key add", u.anthropic)
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

	// Conversation identity for the sticky binding: a stable message prefix
	// (the system prompt for coding agents) is the only element guaranteed
	// identical across turns.
	sessionID := uint64(0)
	if len(g.cfg.APIKeys) > 1 && r.Header.Get(stickyHeader) != stickyOff {
		sessionID = affinityID(obj, mapped)
	}

	// One pass over the keys; when every key answers with a failover status,
	// hold the request and sweep again until RetryWait (default 5m) or
	// success. The z.ai request rate limit (code 1302) is a short-window
	// throttle that clears in seconds to minutes, and agent clients kill a
	// whole run on any 429 — holding here turns a dead run into a short
	// pause. When the deadline passes, the last failover response is
	// forwarded unchanged.
	var resp *http.Response
	keyIdx := 0
	for attempt := 0; ; attempt++ {
		var ok bool
		resp, keyIdx, ok = g.sweep(w, r, u, target, body, obj, requested, mapped, sessionID, start)
		if !ok {
			return
		}
		if !failoverStatus(resp.StatusCode) {
			break
		}
		wait, more := g.holdDuration(attempt, resp, start)
		if !more {
			break
		}
		g.log.Quiet.Printf("%s %s model=%s all keys rate-limited (status %d), holding request for %s", r.Method, r.URL.Path, modelLabel(requested, mapped), resp.StatusCode, wait.Round(time.Millisecond))
		select {
		case <-r.Context().Done():
			g.log.Quiet.Printf("%s %s: client gave up while every key was rate-limited", r.Method, r.URL.Path)
			return
		case <-time.After(wait):
		}
		// Synchronous so the next sweep orders keys by fresh quota data.
		g.refreshUsage()
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

// sweep tries the key order once, replaying the buffered body. Key order
// comes from quota data (see usageMonitor.candidateOrder): smooth weighted
// round-robin over the usable keys — equal weights, so traffic spreads
// evenly, unless a key nears plan expiry and its weight ramps up gradually.
// Exhausted or expired keys are tried last. A conversation with a stable
// message prefix sticks to its key while that key is still usable
// (prompt-cache friendly on multi-account setups); rebinds happen after the
// sweeps. ok=false means an error response was already written to w.
func (g *Gateway) sweep(w http.ResponseWriter, r *http.Request, u upstream, target *url.URL, body []byte, obj map[string]json.RawMessage, requested, mapped string, sessionID uint64, start time.Time) (*http.Response, int, bool) {
	order := g.usage.candidateOrder(len(g.cfg.APIKeys), start)
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
	var resp *http.Response
	keyIdx := order[0]
	for i, idx := range order {
		keyIdx = idx
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("build upstream request: %v", err), u.anthropic)
			return nil, 0, false
		}
		copyForwardedHeaders(req.Header, r.Header)
		setUpstreamAuth(req.Header, u, g.cfg.APIKeys[keyIdx].Secret)

		resp, err = g.client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("could not reach z.ai: %v", err), u.anthropic)
			g.log.Verbose.Printf("%s %s model=%s key=%s status=upstream-error dur=%s: %v", r.Method, r.URL.Path, modelLabel(requested, mapped), g.cfg.APIKeys[keyIdx].Name, time.Since(start).Round(time.Millisecond), err)
			return nil, 0, false
		}
		if !failoverStatus(resp.StatusCode) || i == len(order)-1 {
			break
		}
		reason := peekError(resp.Body)
		resp.Body.Close()
		next := order[(i+1)%len(order)]
		g.log.Quiet.Printf("%s %s model=%s key=%s status=%d, trying key %s: %s", r.Method, r.URL.Path, modelLabel(requested, mapped), g.cfg.APIKeys[keyIdx].Name, resp.StatusCode, g.cfg.APIKeys[next].Name, reason)
		// The cached quota data said this key had room; refetch it.
		g.refreshUsageAsync()
	}
	return resp, keyIdx, true
}

// Retry hold: exponential backoff from retryBaseWait capped at
// retryMaxWait, extended by a longer upstream Retry-After, clamped so the
// last wake happens at the deadline. more=false sends the held response to
// the client.
const (
	retryBaseWait = 5 * time.Second
	retryMaxWait  = 60 * time.Second
)

func (g *Gateway) holdDuration(attempt int, resp *http.Response, start time.Time) (time.Duration, bool) {
	deadline := start.Add(time.Duration(g.cfg.RetryWait))
	wait := retryBaseWait << min(attempt, 8)
	if wait > retryMaxWait || wait <= 0 {
		wait = retryMaxWait
	}
	if ra := retryAfter(resp); ra > wait {
		wait = ra
	}
	if now := time.Now(); now.Add(wait).After(deadline) {
		wait = deadline.Sub(now)
		if wait <= 0 {
			return 0, false
		}
	}
	return wait, true
}

// retryAfter reads a seconds- or date-form Retry-After header, capped at two
// minutes (a longer one would outlive the hold anyway).
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return min(time.Duration(secs)*time.Second, 2*time.Minute)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(time.Until(t), 2*time.Minute)
	}
	return 0
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
		case "Authorization", "X-Api-Key", "Host", "Content-Length", "Accept-Encoding", stickyHeader:
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

// failoverStatus reports whether an upstream status is a key failure another
// key can fix: 401/403 (key invalid or expired), 429 (plan rate limit).
// Everything else is passed through.
func failoverStatus(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusTooManyRequests
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
