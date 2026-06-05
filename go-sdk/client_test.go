package ix

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientPost(t *testing.T) {
	type reqBody struct {
		Name string `json:"name"`
	}
	type respBody struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}

		var req reqBody
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(respBody{ID: 42, Name: req.Name})
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.Client())

	var resp respBody
	err := c.post(context.Background(), "/test", reqBody{Name: "hello"}, &resp)
	if err != nil {
		t.Fatalf("post() returned error: %v", err)
	}
	if resp.ID != 42 {
		t.Errorf("expected ID 42, got %d", resp.ID)
	}
	if resp.Name != "hello" {
		t.Errorf("expected Name 'hello', got %q", resp.Name)
	}
}

func TestClientPostError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.Client())

	var dst map[string]any
	err := c.post(context.Background(), "/fail", map[string]string{"k": "v"}, &dst)
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error should mention HTTP 500, got: %v", err)
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Errorf("error should include response body, got: %v", err)
	}
}

func TestClientGetRaw(t *testing.T) {
	want := "raw response body content"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Write([]byte(want))
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.Client())

	rc, err := c.getRaw(context.Background(), "/raw")
	if err != nil {
		t.Fatalf("getRaw() returned error: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != want {
		t.Errorf("expected %q, got %q", want, string(got))
	}
}

func TestClientGetRawError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal error: gateway request failed: timed out"}`))
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.Client())

	_, err := c.getRaw(context.Background(), "/v1/browser/screenshot")
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error should mention HTTP 500, got: %v", err)
	}
	// The daemon's error body is the only diagnostic for raw-byte routes
	// (screenshot/pdf) — discarding it made the v0.7 bench failure unreadable.
	if !strings.Contains(err.Error(), "gateway request failed") {
		t.Errorf("error should include response body, got: %v", err)
	}
}

func TestClientUpload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/form-data") {
			t.Errorf("expected multipart/form-data Content-Type, got %s", ct)
		}

		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart form: %v", err)
		}

		pathField := r.FormValue("path")
		if pathField != "/workspace/test.txt" {
			t.Errorf("expected path field '/workspace/test.txt', got %q", pathField)
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("get form file: %v", err)
		}
		defer file.Close()

		if header.Filename != "test.txt" {
			t.Errorf("expected filename 'test.txt', got %q", header.Filename)
		}

		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatalf("read file: %v", err)
		}
		if string(data) != "file contents here" {
			t.Errorf("expected 'file contents here', got %q", string(data))
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.Client())

	err := c.upload(
		context.Background(),
		"/upload",
		"/workspace/test.txt",
		strings.NewReader("file contents here"),
	)
	if err != nil {
		t.Fatalf("upload() returned error: %v", err)
	}
}

func TestPostSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if accept := r.Header.Get("Accept"); accept != "text/event-stream" {
			t.Errorf("expected Accept: text/event-stream, got %q", accept)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		w.Write([]byte("event: stdout\ndata: {\"text\": \"hello\\n\"}\n\n"))
		flusher.Flush()
		w.Write([]byte("event: complete\ndata: {\"exit_code\": 0, \"elapsed_ms\": 100}\n\n"))
		flusher.Flush()
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.Client())

	reader, err := c.postSSE(context.Background(), "/v1/shell/exec", map[string]string{"command": "echo hello"})
	if err != nil {
		t.Fatalf("postSSE() returned error: %v", err)
	}
	defer reader.Close()

	// First event: stdout
	if !reader.Next() {
		t.Fatal("expected first event, got EOF")
	}
	if reader.Event() != "stdout" {
		t.Errorf("expected event 'stdout', got %q", reader.Event())
	}

	// Second event: complete
	if !reader.Next() {
		t.Fatal("expected second event, got EOF")
	}
	if reader.Event() != "complete" {
		t.Errorf("expected event 'complete', got %q", reader.Event())
	}

	// No more events
	if reader.Next() {
		t.Error("expected EOF after complete event")
	}
	if reader.Err() != nil {
		t.Errorf("unexpected error: %v", reader.Err())
	}
}

func TestPostSSEError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.Client())

	_, err := c.postSSE(context.Background(), "/v1/shell/exec", nil)
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error should mention HTTP 500, got: %v", err)
	}
}

func TestSSEReaderPingSkip(t *testing.T) {
	body := io.NopCloser(strings.NewReader(
		": ping\n\nevent: stdout\ndata: {\"text\": \"hi\"}\n\n: ping\n\nevent: complete\ndata: {\"exit_code\": 0}\n\n",
	))

	reader := newSSEReader(body, context.Background())

	// First event should be stdout (pings skipped)
	if !reader.Next() {
		t.Fatal("expected first event")
	}
	if reader.Event() != "stdout" {
		t.Errorf("expected 'stdout', got %q", reader.Event())
	}

	// Second event should be complete (ping skipped)
	if !reader.Next() {
		t.Fatal("expected second event")
	}
	if reader.Event() != "complete" {
		t.Errorf("expected 'complete', got %q", reader.Event())
	}

	if reader.Next() {
		t.Error("expected EOF")
	}
}

func TestSSEReaderEmptyStream(t *testing.T) {
	body := io.NopCloser(strings.NewReader(""))
	reader := newSSEReader(body, context.Background())

	if reader.Next() {
		t.Error("expected false for empty stream")
	}
	if reader.Err() != nil {
		t.Errorf("unexpected error: %v", reader.Err())
	}
}

func TestSSEReaderPingOnly(t *testing.T) {
	body := io.NopCloser(strings.NewReader(": ping\n\n: ping\n\n"))
	reader := newSSEReader(body, context.Background())

	if reader.Next() {
		t.Error("expected false for ping-only stream")
	}
}

// fakeVsockProxy speaks the Firecracker vsock UDS handshake
// ("CONNECT <port>\n" -> "OK <port>\n") and then serves HTTP on the
// connection, counting dials so tests can assert keep-alive reuse.
type fakeVsockProxy struct {
	dials atomic.Int64
}

type chanListener struct {
	ch     chan net.Conn
	addr   net.Addr
	closed chan struct{}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c, ok := <-l.ch:
		if !ok {
			return nil, net.ErrClosed
		}
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error {
	select {
	case <-l.closed:
		// already closed
	default:
		close(l.closed)
	}
	return nil
}
func (l *chanListener) Addr() net.Addr { return l.addr }

// bufferedConn replays bytes the handshake reader already buffered.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func startFakeVsockProxy(t *testing.T, sockPath string, handler http.Handler) *fakeVsockProxy {
	t.Helper()
	p := &fakeVsockProxy{}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	httpConns := &chanListener{ch: make(chan net.Conn, 16), addr: ln.Addr(), closed: make(chan struct{})}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(httpConns.ch)
				return
			}
			p.dials.Add(1)
			go func(conn net.Conn) {
				r := bufio.NewReader(conn)
				if _, err := r.ReadString('\n'); err != nil { // CONNECT 1024\n
					conn.Close()
					return
				}
				if _, err := conn.Write([]byte("OK 1024\n")); err != nil {
					conn.Close()
					return
				}
				// Guard against the listener closing mid-handshake: a bare
				// send would panic if the accept loop closed the channel.
				select {
				case httpConns.ch <- &bufferedConn{Conn: conn, r: r}:
				case <-httpConns.closed:
					conn.Close()
				}
			}(conn)
		}
	}()
	srv := &http.Server{Handler: handler}
	go srv.Serve(httpConns) //nolint:errcheck
	// Use Shutdown with a short timeout so stale keep-alive connections don't
	// block cleanup. The transport's CloseIdleConnections (called earlier via
	// the test's own t.Cleanup) drains idle conns; any remaining active conns
	// are abandoned when the context expires.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck
	})
	return p
}

// TestSSEConnectionReuse: consecutive SSE requests must reuse one vsock
// connection. Before the drain-on-Close fix, every request re-dialed.
//
// The handler writes the terminal event, flushes, then sends the final
// newline after a 10 ms pause. This ensures:
//   - The event bytes arrive before the client calls rd.Close() (flushed),
//     so the body is genuinely not at EOF when Close begins.
//   - The remaining data (empty trailing newline → EOF) arrives within
//     the 100 ms drain window in the fixed Close, so the drain succeeds and
//     the connection is returned to the keep-alive pool.
func TestSSEConnectionReuse(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "vsock.uds")
	proxy := startFakeVsockProxy(t, sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not implement http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: complete\ndata: {\"exit_code\":0,\"elapsed_ms\":1}\n\n")
		flusher.Flush()
		// Keep the body open briefly so the client encounters a non-EOF body
		// when it calls rd.Close(). Return after 10 ms so the drain window
		// (100 ms) can read EOF and allow connection reuse.
		time.Sleep(10 * time.Millisecond)
	}))

	tr := vsockTransport(sock).(*http.Transport)
	client := newClient("http://localhost", &http.Client{Transport: tr})
	// Close idle connections after the test so srv.Shutdown() can complete.
	t.Cleanup(func() { tr.CloseIdleConnections() })

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		rd, err := client.postSSE(ctx, "/v1/shell/exec", map[string]any{"command": "true"})
		if err != nil {
			t.Fatalf("postSSE %d: %v", i, err)
		}
		for rd.Next() {
			if rd.Event() == "complete" {
				break
			}
		}
		rd.Close()
	}

	if got := proxy.dials.Load(); got != 1 {
		t.Errorf("expected 1 vsock dial across 3 SSE requests, got %d", got)
	}
}
