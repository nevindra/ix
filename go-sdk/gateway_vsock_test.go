package ix

// gateway_vsock_test.go — light unit tests for NewGatewayForBrowserVM (Change 2).
//
// These tests do NOT dial a real vsock; they only verify that:
//   1. NewGatewayForBrowserVM returns a non-nil *Gateway.
//   2. The gateway's own GET /health responds 200 (self-health, no upstream dial).

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewGatewayForBrowserVM_NotNil(t *testing.T) {
	g := NewGatewayForBrowserVM("/tmp/fake.sock", "token123", 8, slog.Default())
	if g == nil {
		t.Fatal("NewGatewayForBrowserVM returned nil")
	}
}

func TestNewGatewayForBrowserVM_OwnHealthReturns200(t *testing.T) {
	g := NewGatewayForBrowserVM("/tmp/fake.sock", "", 0, nil)
	h := g.Handler()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
