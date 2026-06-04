package ix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// gateway.go — the Browser Gateway: a host-side HTTP service that fronts a
// shared pinchtab (browser-tier) process for many per-chat daemons.
//
// Each chat gets its own pinchtab instance (profile dir keyed by chat ID) and a
// single tab inside it. Browser ops arriving as POST/GET /v1/browser/* are
// translated to pinchtab's tab-scoped routes (METHOD /tabs/{tabId}/<op>) and
// relayed faithfully (status + body, raw bytes for screenshot/pdf).
//
// Pinchtab API verified against /home/nezhifi/Development/sandbox/pinchtab:
//   - POST /profiles {"name"} -> 200 {"id","name"} (409 if it already exists);
//     a profile MUST exist before /instances/start will accept its profileId
//     (internal/profiles/handlers.go:handleCreate,
//     internal/orchestrator/handlers_profiles.go:resolveProfileName)
//   - POST /instances/start {"profileId","mode"} -> 201 {"id","profileName",...}
//     (internal/orchestrator/handlers_instances.go:280, internal/api/types/types.go:35)
//     NB: returns while status is "starting"; Chrome boots async (monitor in
//     internal/orchestrator/health.go flips it to "running"), so poll
//     GET /instances/{id} until "running" before opening a tab.
//   - GET /instances/{id} -> {"status","error",...} (handlers_instances.go:25)
//   - POST /instances/{id}/tabs/open {"url"} -> {"tabId","url","title"}
//     (internal/orchestrator/handlers_tabs.go:18, internal/handlers/tab_lifecycle.go:102)
//   - tab-scoped METHOD /tabs/{id}/<op> for navigate/snapshot/text/action/find/
//     screenshot/pdf/evaluate (internal/routes/routes.go:40-44, 48-171, 205-217;
//     evaluate is gated behind security.allowEvaluate, routes.go:155)
//   - POST /instances/{id}/stop -> 200 (handlers_instances.go:71)
//   - GET /health (referenced by orchestrator health probes)

// GatewayConfig configures a Gateway. The pinchtab client and base URL are
// injected so the gateway is unit-testable against an httptest.Server.
type GatewayConfig struct {
	// PinchtabBaseURL is the base URL of the pinchtab server-mode process,
	// e.g. "http://10.0.0.2:9867" or "http://localhost" when paired with a
	// vsock transport on PinchtabClient.
	PinchtabBaseURL string

	// PinchtabClient is the HTTP client used for all upstream calls. In
	// production this carries a vsock transport; in tests, an httptest client.
	// Required.
	PinchtabClient *http.Client

	// PinchtabToken, if non-empty, is forwarded as "Authorization: Bearer <token>"
	// on every upstream request.
	PinchtabToken string

	// MaxInflight caps concurrent in-flight browser calls across all chats on
	// this gateway (per-VM semaphore). 0 uses a sane default (32).
	MaxInflight int

	// Logger; defaults to slog.Default().
	Logger *slog.Logger
}

// healthState is the gateway's view of the upstream pinchtab process.
type healthState int

const (
	healthHealthy healthState = iota
	healthDegraded
	healthUnhealthy
)

func (s healthState) String() string {
	switch s {
	case healthHealthy:
		return "healthy"
	case healthDegraded:
		return "degraded"
	case healthUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// chatInstance is the per-chat pinchtab instance + tab binding.
type chatInstance struct {
	instanceID string
	tabID      string
	calls      int64     // browser ops served for this chat
	lastSeen   time.Time // last browser op time
	// mu serializes browser ops for this chat (at most one in-flight per chat).
	mu sync.Mutex
}

// Gateway fronts a shared pinchtab process for multiple per-chat daemons.
type Gateway struct {
	cfg     GatewayConfig
	baseURL string
	client  *http.Client
	logger  *slog.Logger

	// chats maps chatID -> binding; protected by mu.
	mu    sync.Mutex
	chats map[string]*chatInstance

	// sem bounds total in-flight browser calls (per-VM cap).
	sem chan struct{}

	// health state machine.
	healthMu   sync.Mutex
	state      healthState
	failStreak int

	// heartbeat lifecycle.
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

const defaultMaxInflight = 32

// NewGateway constructs a Gateway from cfg. The pinchtab client and base URL
// must be provided by the caller (injected for testability).
func NewGateway(cfg GatewayConfig) *Gateway {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.PinchtabClient == nil {
		cfg.PinchtabClient = http.DefaultClient
	}
	maxInflight := cfg.MaxInflight
	if maxInflight <= 0 {
		maxInflight = defaultMaxInflight
	}
	return &Gateway{
		cfg:     cfg,
		baseURL: strings.TrimRight(cfg.PinchtabBaseURL, "/"),
		client:  cfg.PinchtabClient,
		logger:  cfg.Logger,
		chats:   make(map[string]*chatInstance),
		sem:     make(chan struct{}, maxInflight),
		state:   healthHealthy,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// ---- HTTP surface ----

// browserOp describes how a /v1/browser/<name> request maps to a pinchtab
// tab-scoped route and how its response should be relayed.
type browserOp struct {
	method  string // HTTP method on both gateway and pinchtab
	rawBody bool   // true => relay upstream bytes verbatim (screenshot/pdf)
}

// browserOps is the fixed set of supported /v1/browser/* operations. The map
// key is the op name (final path segment) and equals the pinchtab tab route.
var browserOps = map[string]browserOp{
	"navigate":   {method: http.MethodPost},
	"screenshot": {method: http.MethodGet, rawBody: true},
	"action":     {method: http.MethodPost},
	"snapshot":   {method: http.MethodGet},
	"text":       {method: http.MethodGet},
	"pdf":        {method: http.MethodGet, rawBody: true},
	"evaluate":   {method: http.MethodPost},
	"find":       {method: http.MethodPost},
	"wait":       {method: http.MethodPost},
}

// Handler returns the gateway's HTTP handler.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()

	for name, op := range browserOps {
		mux.HandleFunc(op.method+" /v1/browser/"+name, g.makeBrowserHandler(name, op))
	}

	mux.HandleFunc("DELETE /chats/{chatId}", g.handleDeleteChat)
	mux.HandleFunc("GET /health", g.handleHealth)
	mux.HandleFunc("GET /metrics", g.handleMetrics)

	return mux
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":   "ok",
		"upstream": g.State().String(),
	})
}

func (g *Gateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	type chatMetric struct {
		ChatID     string    `json:"chatId"`
		InstanceID string    `json:"instanceId"`
		Calls      int64     `json:"calls"`
		LastSeen   time.Time `json:"lastSeen"`
	}

	g.mu.Lock()
	metrics := make([]chatMetric, 0, len(g.chats))
	for id, ci := range g.chats {
		metrics = append(metrics, chatMetric{
			ChatID:     id,
			InstanceID: ci.instanceID,
			Calls:      ci.calls,
			LastSeen:   ci.lastSeen,
		})
	}
	g.mu.Unlock()

	sort.Slice(metrics, func(i, j int) bool { return metrics[i].ChatID < metrics[j].ChatID })

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"upstream": g.State().String(),
		"chats":    metrics,
	})
}

// makeBrowserHandler returns the handler for a single /v1/browser/<name> route.
func (g *Gateway) makeBrowserHandler(name string, op browserOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID := strings.TrimSpace(r.Header.Get("X-IX-Chat-Id"))
		if chatID == "" {
			http.Error(w, "missing X-IX-Chat-Id header", http.StatusBadRequest)
			return
		}

		// Reject early while upstream is down.
		if g.State() == healthUnhealthy {
			http.Error(w, "browser upstream unavailable", http.StatusServiceUnavailable)
			return
		}

		// Read the request body once (needed for egress host extraction on
		// navigate, and for forwarding).
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Egress check on navigate: parse the target URL's host and apply the
		// policy from the header before any upstream call.
		if name == "navigate" {
			policy := parseEgressHeader(r.Header.Get("X-IX-Egress-Policy"))
			host := navigateHost(body)
			if host != "" && !isAllowed(host, policy) {
				http.Error(w, "egress denied: "+host, http.StatusForbidden)
				return
			}
		}

		// Per-VM concurrency cap.
		select {
		case g.sem <- struct{}{}:
			defer func() { <-g.sem }()
		default:
			http.Error(w, "too many concurrent browser requests", http.StatusTooManyRequests)
			return
		}

		// Resolve (or provision) the chat's instance+tab. On a navigate, open the
		// tab directly at the target URL so it is non-transient and routable
		// (see openTab); other ops provision a blank tab.
		var initialURL string
		if name == "navigate" {
			initialURL = navigateURL(body)
		}
		ci, err := g.ensureChat(r.Context(), chatID, initialURL)
		if err != nil {
			g.logger.Error("gateway: provision chat failed", "chat", chatID, "error", err)
			http.Error(w, "provision browser instance: "+err.Error(), http.StatusBadGateway)
			return
		}

		// At most one in-flight browser op per chat.
		ci.mu.Lock()
		defer ci.mu.Unlock()

		g.mu.Lock()
		ci.calls++
		ci.lastSeen = time.Now()
		g.mu.Unlock()

		// Forward to the tab-scoped pinchtab route, preserving query params.
		target := g.baseURL + "/tabs/" + ci.tabID + "/" + name
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}

		g.forward(w, r, op, target, body)
	}
}

// forward issues the upstream pinchtab request and relays the response.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, op browserOp, target string, body []byte) {
	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}

	upReq, err := http.NewRequestWithContext(r.Context(), op.method, target, reqBody)
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		upReq.Header.Set("Content-Type", ct)
	} else if len(body) > 0 {
		upReq.Header.Set("Content-Type", "application/json")
	}
	g.applyAuth(upReq)

	resp, err := g.client.Do(upReq)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// pinchtab returns 409 "navigation_changed" when an action (e.g. a click on a
	// link) causes the page to navigate. For an agent that is a normal, successful
	// outcome — the click landed and the page changed — not an error. Surfacing it
	// as an HTTP error makes the daemon return a confusing 500. Convert it into a
	// success result that reports the navigation so the caller knows to re-snapshot.
	if resp.StatusCode == http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		if msg, ok := navigationChangedMessage(raw); ok {
			out, _ := json.Marshal(map[string]any{"success": true, "message": msg})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
			return
		}
		// Some other conflict — relay it verbatim.
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		} else if !op.rawBody {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	// Relay Content-Type (important for raw bytes: image/png, application/pdf).
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else if !op.rawBody {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// navigationChangedMessage reports whether a pinchtab error body is a
// "navigation_changed" conflict (an action triggered a page navigation) and, if
// so, returns a human-readable message describing the navigation.
func navigationChangedMessage(body []byte) (string, bool) {
	var e struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Code != "navigation_changed" {
		return "", false
	}
	msg := strings.TrimSpace(e.Error)
	if msg == "" {
		msg = "the action navigated the page"
	}
	return msg, true
}

func (g *Gateway) applyAuth(req *http.Request) {
	if g.cfg.PinchtabToken != "" {
		req.Header.Set("Authorization", "Bearer "+g.cfg.PinchtabToken)
	}
}

// ---- chat lifecycle ----

// ensureChat returns the existing binding for chatID, provisioning a new
// pinchtab instance + tab if none exists yet. initialURL, when non-empty, is the
// URL the freshly-opened tab is navigated to so it is routable (see openTab); it
// is ignored when a binding already exists.
func (g *Gateway) ensureChat(ctx context.Context, chatID, initialURL string) (*chatInstance, error) {
	g.mu.Lock()
	if ci, ok := g.chats[chatID]; ok {
		g.mu.Unlock()
		return ci, nil
	}
	g.mu.Unlock()

	// Provision outside the map lock (network I/O). A concurrent provision for
	// the same chat is guarded against below by re-checking under the lock.
	instanceID, err := g.startInstance(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("start instance: %w", err)
	}
	// /instances/start returns immediately with status "starting" while Chrome
	// boots in the background; opening a tab before the instance is "running"
	// 503s ("instance is not running"). Wait for readiness first.
	if err := g.waitInstanceRunning(ctx, instanceID); err != nil {
		_ = g.stopInstance(context.WithoutCancel(ctx), instanceID)
		return nil, fmt.Errorf("wait instance running: %w", err)
	}
	tabID, err := g.openTab(ctx, instanceID, initialURL)
	if err != nil {
		// Best-effort cleanup of the orphaned instance.
		_ = g.stopInstance(context.WithoutCancel(ctx), instanceID)
		return nil, fmt.Errorf("open tab: %w", err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if ci, ok := g.chats[chatID]; ok {
		// Lost a race; another goroutine provisioned first. Tear down ours.
		go func() { _ = g.stopInstance(context.Background(), instanceID) }()
		return ci, nil
	}
	ci := &chatInstance{instanceID: instanceID, tabID: tabID, lastSeen: time.Now()}
	g.chats[chatID] = ci
	g.logger.Info("gateway: provisioned chat instance", "chat", chatID, "instance", instanceID, "tab", tabID)
	return ci, nil
}

// ensureProfile idempotently creates the pinchtab profile that backs a chat.
// pinchtab's /instances/start resolves "profileId" against EXISTING profiles and
// returns 404 ("profile not found") if it has never been created
// (internal/orchestrator/handlers_profiles.go:resolveProfileName). A stable
// per-chat profile name also reuses the on-disk profile dir across instance
// restarts, so cookies/sessions survive. POST /profiles returns 200 on create
// and 409 ("already exists") on a repeat — both mean "the profile is ready", so
// 409 is treated as success to stay idempotent across reconnects.
func (g *Gateway) ensureProfile(ctx context.Context, chatID string) error {
	reqBody, _ := json.Marshal(map[string]string{"name": "chat-" + chatID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/profiles", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	g.applyAuth(req)

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300, resp.StatusCode == http.StatusConflict:
		return nil
	default:
		return fmt.Errorf("pinchtab POST /profiles: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// startInstance creates a pinchtab instance for the chat and returns its ID.
func (g *Gateway) startInstance(ctx context.Context, chatID string) (string, error) {
	// The profile must exist before /instances/start can resolve its profileId.
	if err := g.ensureProfile(ctx, chatID); err != nil {
		return "", fmt.Errorf("ensure profile: %w", err)
	}
	reqBody, _ := json.Marshal(map[string]string{
		"mode":      "headless",
		"profileId": "chat-" + chatID,
	})
	var out struct {
		ID string `json:"id"`
	}
	if err := g.upstreamJSON(ctx, http.MethodPost, "/instances/start", reqBody, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("pinchtab returned empty instance id")
	}
	return out.ID, nil
}

// openTab opens a tab in the given instance and returns its tab ID. When
// initialURL is non-empty the tab is navigated to it on open; this is essential,
// not cosmetic: pinchtab's GET /tabs (used by the orchestrator's tab-owner
// routing) filters out transient URLs like about:blank
// (internal/handlers/health_tabs.go:IsTransientURL), so a tab opened blank is
// invisible to routing and every later /tabs/{id}/<op> 404s with "tab not found"
// whenever more than one instance is running. Opening straight to the target URL
// makes the tab non-transient and routable.
func (g *Gateway) openTab(ctx context.Context, instanceID, initialURL string) (string, error) {
	var out struct {
		TabID string `json:"tabId"`
	}
	reqBody, _ := json.Marshal(map[string]string{"url": initialURL})
	path := "/instances/" + instanceID + "/tabs/open"
	if err := g.upstreamJSON(ctx, http.MethodPost, path, reqBody, &out); err != nil {
		return "", err
	}
	if out.TabID == "" {
		return "", fmt.Errorf("pinchtab returned empty tab id")
	}
	return out.TabID, nil
}

// Instance readiness polling. pinchtab's monitor gives the child Chrome up to
// instanceStartupTimeout (45s, internal/orchestrator/health.go) to pass its
// health probe before marking the instance "error", so the gateway waits a hair
// longer than that before giving up.
const (
	instanceReadyTimeout      = 50 * time.Second
	instanceReadyPollInterval = 300 * time.Millisecond
)

// waitInstanceRunning polls GET /instances/{id} until the instance reports
// "running". A terminal status ("error"/"stopped") fails fast so we don't burn
// the whole timeout on an instance that already gave up.
func (g *Gateway) waitInstanceRunning(ctx context.Context, instanceID string) error {
	deadline := time.Now().Add(instanceReadyTimeout)
	last := "starting"
	for {
		var out struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := g.upstreamJSON(ctx, http.MethodGet, "/instances/"+instanceID, nil, &out); err != nil {
			return err
		}
		last = out.Status
		switch out.Status {
		case "running":
			return nil
		case "error", "stopped":
			if out.Error != "" {
				return fmt.Errorf("instance %s entered %q: %s", instanceID, out.Status, out.Error)
			}
			return fmt.Errorf("instance %s entered %q before becoming ready", instanceID, out.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("instance %s not running after %s (last status %q)", instanceID, instanceReadyTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(instanceReadyPollInterval):
		}
	}
}

// stopInstance stops a pinchtab instance (best-effort).
func (g *Gateway) stopInstance(ctx context.Context, instanceID string) error {
	path := "/instances/" + instanceID + "/stop"
	return g.upstreamJSON(ctx, http.MethodPost, path, nil, nil)
}

// upstreamJSON performs a JSON request to pinchtab and optionally decodes the
// response body into out. Non-2xx responses are returned as errors.
func (g *Gateway) upstreamJSON(ctx context.Context, method, path string, body []byte, out any) error {
	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	g.applyAuth(req)

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("pinchtab %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode pinchtab response: %w", err)
		}
	}
	return nil
}

// handleDeleteChat tears down the chat's pinchtab instance. Idempotent: a chat
// with no binding still returns 200.
func (g *Gateway) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	chatID := r.PathValue("chatId")

	g.mu.Lock()
	ci, ok := g.chats[chatID]
	if ok {
		delete(g.chats, chatID)
	}
	g.mu.Unlock()

	if ok {
		if err := g.stopInstance(r.Context(), ci.instanceID); err != nil {
			// Already removed from the map; log but still report success so the
			// caller can move on. The orphaned instance will be reaped by
			// pinchtab's own lifecycle.
			g.logger.Warn("gateway: stop instance failed during teardown", "chat", chatID, "instance", ci.instanceID, "error", err)
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"deleted":true}`))
}

// ---- heartbeat ----

// Start launches the heartbeat goroutine that polls pinchtab /health every 10s
// and drives the health state machine. Safe to call once.
func (g *Gateway) Start(ctx context.Context) {
	go g.heartbeatLoop(ctx)
}

// Stop halts the heartbeat goroutine. Safe to call multiple times.
func (g *Gateway) Stop() {
	g.stopOnce.Do(func() { close(g.stopCh) })
	<-g.doneCh
}

func (g *Gateway) heartbeatLoop(ctx context.Context) {
	defer close(g.doneCh)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.probeOnce()
		}
	}
}

// probeOnce performs a single /health probe (2s timeout) and updates the state
// machine. Exposed (unexported) for deterministic testing without timers.
func (g *Gateway) probeOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok := g.healthCheck(ctx)

	g.healthMu.Lock()
	defer g.healthMu.Unlock()
	if ok {
		// One success → Healthy, streak reset.
		g.failStreak = 0
		g.state = healthHealthy
		return
	}
	g.failStreak++
	if g.failStreak >= 3 {
		g.state = healthUnhealthy
	} else {
		g.state = healthDegraded
	}
}

func (g *Gateway) healthCheck(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	g.applyAuth(req)
	resp, err := g.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// State returns the current upstream health state.
func (g *Gateway) State() healthState {
	g.healthMu.Lock()
	defer g.healthMu.Unlock()
	return g.state
}

// forceState sets the health state directly. Test-only helper.
func (g *Gateway) forceState(s healthState) {
	g.healthMu.Lock()
	defer g.healthMu.Unlock()
	g.state = s
	if s == healthHealthy {
		g.failStreak = 0
	}
}

// ---- helpers ----

// parseEgressHeader parses the X-IX-Egress-Policy header value. A missing or
// malformed header yields a disabled policy (allow everything).
func parseEgressHeader(raw string) EgressPolicy {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return EgressPolicy{Enabled: false}
	}
	var p EgressPolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return EgressPolicy{Enabled: false}
	}
	return p
}

// navigateURL extracts the raw "url" field from a navigate request body.
// Returns "" if absent or unparseable.
func navigateURL(body []byte) string {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	return req.URL
}

// navigateHost extracts the host from a navigate request body {"url":...}.
// Returns "" if absent or unparseable (caller treats "" as "no host to check").
func navigateHost(body []byte) string {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	if req.URL == "" {
		return ""
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
