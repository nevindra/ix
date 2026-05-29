package ix

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// mockPinchtab is an httptest-backed fake of the pinchtab server-mode API.
// It records the sequence of (method, path) calls and serves canned responses.
type mockPinchtab struct {
	mu        sync.Mutex
	calls     []string          // "METHOD /path" in arrival order
	bodies    map[string]string // last body seen for a given "METHOD /path"
	lastQuery string            // RawQuery of the most recent call

	// instanceID/tabID handed out on start/open.
	instanceID string
	tabID      string

	// healthFails, when >0, makes /health return 500 that many times before
	// recovering. Decremented per /health hit. Use a large value to keep failing.
	healthFails atomic.Int32

	// startCount counts /instances/start calls (to assert reuse).
	startCount atomic.Int32
	stopCount  atomic.Int32

	// lastProfileID captures the profileId sent on the most recent start.
	lastProfileID atomic.Value // string
}

func newMockPinchtab() *mockPinchtab {
	return &mockPinchtab{
		instanceID: "inst_abc123",
		tabID:      "tab_def456",
		bodies:     map[string]string{},
	}
}

func (m *mockPinchtab) record(r *http.Request, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	m.calls = append(m.calls, key)
	m.bodies[key] = body
	m.lastQuery = r.URL.RawQuery
}

func (m *mockPinchtab) callSeq() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *mockPinchtab) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		m.record(r, "")
		if m.healthFails.Load() > 0 {
			m.healthFails.Add(-1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("POST /instances/start", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.record(r, string(body))
		m.startCount.Add(1)
		var req struct {
			ProfileID string `json:"profileId"`
			Mode      string `json:"mode"`
		}
		_ = json.Unmarshal(body, &req)
		m.lastProfileID.Store(req.ProfileID)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          m.instanceID,
			"profileId":   "prof_x",
			"profileName": req.ProfileID,
			"port":        "9001",
			"mode":        req.Mode,
			"status":      "running",
		})
	})

	mux.HandleFunc("POST /instances/{id}/tabs/open", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.record(r, string(body))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tabId": m.tabID,
			"url":   "about:blank",
			"title": "",
		})
	})

	mux.HandleFunc("POST /instances/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		m.record(r, "")
		m.stopCount.Add(1)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "stopped", "id": r.PathValue("id")})
	})

	// Tab-scoped ops.
	mux.HandleFunc("POST /tabs/{id}/navigate", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.record(r, string(body))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /tabs/{id}/screenshot", func(w http.ResponseWriter, r *http.Request) {
		m.record(r, "")
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nFAKE"))
	})

	mux.HandleFunc("GET /tabs/{id}/pdf", func(w http.ResponseWriter, r *http.Request) {
		m.record(r, "")
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("%PDF-1.4 FAKE"))
	})

	mux.HandleFunc("GET /tabs/{id}/snapshot", func(w http.ResponseWriter, r *http.Request) {
		m.record(r, "")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"snapshot": "tree"})
	})

	mux.HandleFunc("GET /tabs/{id}/text", func(w http.ResponseWriter, r *http.Request) {
		m.record(r, "")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "hello"})
	})

	mux.HandleFunc("POST /tabs/{id}/action", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.record(r, string(body))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /tabs/{id}/find", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.record(r, string(body))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"matches": []string{}})
	})

	mux.HandleFunc("POST /tabs/{id}/evaluate", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.record(r, string(body))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": 2})
	})

	return mux
}

// newTestGateway wires a Gateway to a fresh mock pinchtab server and returns
// both plus a cleanup func. The heartbeat is NOT started by default; tests that
// need a known health state set it explicitly via forceState.
func newTestGateway(t *testing.T) (*Gateway, *mockPinchtab, func()) {
	t.Helper()
	mock := newMockPinchtab()
	srv := httptest.NewServer(mock.handler())
	g := NewGateway(GatewayConfig{
		PinchtabBaseURL: srv.URL,
		PinchtabClient:  srv.Client(),
	})
	return g, mock, srv.Close
}

func doReq(t *testing.T, h http.Handler, method, target, chatID string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if chatID != "" {
		req.Header.Set("X-IX-Chat-Id", chatID)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- lifecycle: first navigate provisions instance+tab; reuse on second ---

func TestGateway_FirstNavigateProvisionsThenReuses(t *testing.T) {
	g, mock, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	rec := doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", nil, `{"url":"https://example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("navigate status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	seq := mock.callSeq()
	want := []string{
		"POST /instances/start",
		"POST /instances/" + mock.instanceID + "/tabs/open",
		"POST /tabs/" + mock.tabID + "/navigate",
	}
	if len(seq) != len(want) {
		t.Fatalf("call sequence = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("call[%d] = %q, want %q (full seq %v)", i, seq[i], want[i], seq)
		}
	}

	// profileId must be chat-<id>.
	if got, _ := mock.lastProfileID.Load().(string); got != "chat-chat1" {
		t.Fatalf("profileId = %q, want %q", got, "chat-chat1")
	}

	// Second navigate for same chat: no new /instances/start.
	rec2 := doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", nil, `{"url":"https://example.com/page2"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second navigate status = %d, want 200", rec2.Code)
	}
	if got := mock.startCount.Load(); got != 1 {
		t.Fatalf("instances/start called %d times, want 1 (instance should be reused)", got)
	}
}

// --- header validation: missing chat id ---

func TestGateway_MissingChatID(t *testing.T) {
	g, _, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	rec := doReq(t, h, http.MethodPost, "/v1/browser/navigate", "", nil, `{"url":"https://example.com"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing chat id status = %d, want 400", rec.Code)
	}
}

// --- egress: deny blocks before forwarding, allow forwards ---

func TestGateway_EgressDeniedDoesNotForward(t *testing.T) {
	g, mock, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	policy := EgressPolicy{Enabled: true, Mode: "allow", Rules: []string{"example.com"}}
	pj, _ := json.Marshal(policy)
	headers := map[string]string{"X-IX-Egress-Policy": string(pj)}

	rec := doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", headers, `{"url":"https://evil.com"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied navigate status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "evil.com") {
		t.Fatalf("403 body should mention host, got %q", rec.Body.String())
	}
	if len(mock.callSeq()) != 0 {
		t.Fatalf("denied navigate must NOT forward to pinchtab; calls=%v", mock.callSeq())
	}
}

func TestGateway_EgressAllowedForwards(t *testing.T) {
	g, mock, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	policy := EgressPolicy{Enabled: true, Mode: "allow", Rules: []string{"example.com"}}
	pj, _ := json.Marshal(policy)
	headers := map[string]string{"X-IX-Egress-Policy": string(pj)}

	rec := doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", headers, `{"url":"https://example.com/x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed navigate status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	seq := mock.callSeq()
	if len(seq) == 0 || seq[len(seq)-1] != "POST /tabs/"+mock.tabID+"/navigate" {
		t.Fatalf("allowed navigate should reach pinchtab navigate; seq=%v", seq)
	}
}

// --- screenshot streams raw bytes ---

func TestGateway_ScreenshotRawBytes(t *testing.T) {
	g, _, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	// Provision the chat first.
	doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", nil, `{"url":"https://example.com"}`)

	rec := doReq(t, h, http.MethodGet, "/v1/browser/screenshot?raw=true", "chat1", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("screenshot status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("screenshot Content-Type = %q, want image/png", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "\x89PNG") {
		t.Fatalf("screenshot body should be raw PNG bytes, got %q", rec.Body.String()[:8])
	}
}

func TestGateway_PDFRawBytes(t *testing.T) {
	g, _, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()
	doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", nil, `{"url":"https://example.com"}`)

	rec := doReq(t, h, http.MethodGet, "/v1/browser/pdf", "chat1", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pdf status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("pdf Content-Type = %q, want application/pdf", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "%PDF") {
		t.Fatalf("pdf body should be raw PDF bytes, got %q", rec.Body.String())
	}
}

// --- unhealthy upstream → 503 ---

func TestGateway_UnhealthyReturns503(t *testing.T) {
	g, _, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	g.forceState(healthUnhealthy)

	rec := doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", nil, `{"url":"https://example.com"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy navigate status = %d, want 503", rec.Code)
	}
}

// --- heartbeat state machine via direct probe (no goroutine timing) ---

func TestGateway_HeartbeatStateMachine(t *testing.T) {
	g, mock, cleanup := newTestGateway(t)
	defer cleanup()

	// Start healthy.
	if got := g.State(); got != healthHealthy {
		t.Fatalf("initial state = %v, want Healthy", got)
	}

	// 3 consecutive failures → Unhealthy (Degraded at 1, still degraded at 2, unhealthy at 3).
	mock.healthFails.Store(100) // keep failing
	g.probeOnce()
	if got := g.State(); got != healthDegraded {
		t.Fatalf("after 1 fail state = %v, want Degraded", got)
	}
	g.probeOnce()
	if got := g.State(); got != healthDegraded {
		t.Fatalf("after 2 fails state = %v, want Degraded", got)
	}
	g.probeOnce()
	if got := g.State(); got != healthUnhealthy {
		t.Fatalf("after 3 fails state = %v, want Unhealthy", got)
	}

	// One success → back to Healthy.
	mock.healthFails.Store(0)
	g.probeOnce()
	if got := g.State(); got != healthHealthy {
		t.Fatalf("after recovery state = %v, want Healthy", got)
	}
}

// --- teardown is idempotent ---

func TestGateway_DeleteChatIdempotent(t *testing.T) {
	g, mock, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	// Provision.
	doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", nil, `{"url":"https://example.com"}`)

	rec := doReq(t, h, http.MethodDelete, "/chats/chat1", "", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", rec.Code)
	}
	if got := mock.stopCount.Load(); got != 1 {
		t.Fatalf("instances/stop called %d times, want 1", got)
	}

	// Second delete still 200, no extra stop call.
	rec2 := doReq(t, h, http.MethodDelete, "/chats/chat1", "", nil, "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("second delete status = %d, want 200 (idempotent)", rec2.Code)
	}
	if got := mock.stopCount.Load(); got != 1 {
		t.Fatalf("instances/stop called %d times after 2nd delete, want 1 (idempotent)", got)
	}
}

// --- gateway's own /health and /metrics ---

func TestGateway_OwnHealthEndpoint(t *testing.T) {
	g, _, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	rec := doReq(t, h, http.MethodGet, "/health", "", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", rec.Code)
	}
}

func TestGateway_MetricsCountsCalls(t *testing.T) {
	g, _, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chatM", nil, `{"url":"https://example.com"}`)

	rec := doReq(t, h, http.MethodGet, "/metrics", "", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "chatM") {
		t.Fatalf("/metrics should mention chat id chatM, got %q", rec.Body.String())
	}
}

// --- snapshot/text/action/find/evaluate are forwarded to tab-scoped routes ---

func TestGateway_ForwardsAllTabOps(t *testing.T) {
	g, mock, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	// Provision once.
	doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", nil, `{"url":"https://example.com"}`)

	cases := []struct {
		method, target, body, wantCall string
	}{
		{http.MethodGet, "/v1/browser/snapshot?filter=&depth=3", "", "GET /tabs/" + mock.tabID + "/snapshot"},
		{http.MethodGet, "/v1/browser/text?mode=raw&maxChars=100", "", "GET /tabs/" + mock.tabID + "/text"},
		{http.MethodPost, "/v1/browser/action", `{"kind":"click","ref":"e5"}`, "POST /tabs/" + mock.tabID + "/action"},
		{http.MethodPost, "/v1/browser/find", `{"query":"login"}`, "POST /tabs/" + mock.tabID + "/find"},
		{http.MethodPost, "/v1/browser/evaluate", `{"expression":"1+1"}`, "POST /tabs/" + mock.tabID + "/evaluate"},
	}
	for _, tc := range cases {
		rec := doReq(t, h, tc.method, tc.target, "chat1", nil, tc.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s status = %d, want 200; body=%s", tc.method, tc.target, rec.Code, rec.Body.String())
		}
		seq := mock.callSeq()
		if seq[len(seq)-1] != tc.wantCall {
			t.Fatalf("%s %s last call = %q, want %q", tc.method, tc.target, seq[len(seq)-1], tc.wantCall)
		}
	}
}

// --- query params are forwarded to pinchtab ---

func TestGateway_ForwardsQueryParams(t *testing.T) {
	g, mock, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()
	doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", nil, `{"url":"https://example.com"}`)

	rec := doReq(t, h, http.MethodGet, "/v1/browser/text?mode=raw&maxChars=42", "chat1", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("text status = %d, want 200", rec.Code)
	}

	mock.mu.Lock()
	gotQuery := mock.lastQuery
	mock.mu.Unlock()
	if gotQuery != "mode=raw&maxChars=42" {
		t.Fatalf("forwarded query = %q, want %q", gotQuery, "mode=raw&maxChars=42")
	}
}

// --- Authorization forwarded when token configured ---

func TestGateway_ForwardsAuthToken(t *testing.T) {
	var gotAuth atomic.Value
	gotAuth.Store("")
	mock := newMockPinchtab()
	base := mock.handler()
	// Wrap to capture the Authorization header on the navigate forward.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/navigate") {
			gotAuth.Store(r.Header.Get("Authorization"))
		}
		base.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	g := NewGateway(GatewayConfig{
		PinchtabBaseURL: srv.URL,
		PinchtabClient:  srv.Client(),
		PinchtabToken:   "secret-token",
	})
	h := g.Handler()

	rec := doReq(t, h, http.MethodPost, "/v1/browser/navigate", "chat1", nil, `{"url":"https://example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("navigate status = %d, want 200", rec.Code)
	}
	if got := gotAuth.Load().(string); got != "Bearer secret-token" {
		t.Fatalf("forwarded Authorization = %q, want %q", got, "Bearer secret-token")
	}
}
