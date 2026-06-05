//go:build integration

package ix

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// benchPageHTML is the hermetic navigation target: served from the host so
// browser benchmarks measure OUR stack (gateway + pinchtab + Chrome), not the
// internet. A button mutates the DOM so action benchmarks have a side effect.
const benchPageHTML = `<!DOCTYPE html>
<html><head><title>ix bench page</title></head>
<body>
<h1 id="title">ix benchmark page</h1>
<button id="btn" onclick="document.getElementById('out').textContent='clicked'">Click me</button>
<div id="out"></div>
<p>Stable filler content for snapshot and text extraction benchmarks.</p>
</body></html>`

// browserBenchEnv builds a manager with the shared browser tier and starts a
// local page server on the gateway IP (routable from guests via TAP).
// Skips unless IX_BROWSER_VM_IMAGE is set.
//
// NOTE: UseSnapshot is incompatible with browser sandboxes (snapshot-restored
// VMs are vsock-only — no TAP, no route to the gateway), so the pool is the
// fast-create path here.
func browserBenchEnv(b *testing.B) (*IXManager, string) {
	b.Helper()
	img := os.Getenv("IX_BROWSER_VM_IMAGE")
	if img == "" {
		b.Skip("set IX_BROWSER_VM_IMAGE to run browser benchmarks")
	}
	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage:    rootfsImage(),
		KernelPath:     kernelPath(),
		FCBinary:       fcBinary(),
		BrowserMode:    "remote",
		BrowserVMImage: img,
		PoolSize:       2,
		DefaultTTL:     10 * time.Minute,
		// The bench page server sits on the gateway's link-local IP, which
		// pinchtab's SSRF guard would otherwise 403 ("navigation target
		// resolves to blocked private/internal IP"). Trust ONLY that range.
		BrowserTrustedResolveCIDRs: []string{"169.254.0.0/16"},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { mgr.Close() })

	// The manager pinned 169.254.0.1 on ixgw0; bind the page server there so
	// guest Chrome can reach it through its TAP default route.
	ln, err := net.Listen("tcp", "169.254.0.1:0")
	if err != nil {
		b.Fatalf("bind page server on gateway IP: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, benchPageHTML)
	})}
	go srv.Serve(ln) //nolint:errcheck
	b.Cleanup(func() { srv.Close() })

	return mgr, "http://" + ln.Addr().String() + "/"
}

// browserBenchSandbox creates one browser-enabled sandbox and navigates it
// once so per-op benchmarks measure steady-state (instance + tab exist).
func browserBenchSandbox(b *testing.B, mgr *IXManager, pageURL, sid string) sandbox.Sandbox {
	b.Helper()
	ctx := context.Background()
	yes := true
	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid, Browser: &yes})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = mgr.Destroy(context.Background(), sb.(*IXSandbox).id) })
	if err := sb.BrowserNavigate(ctx, pageURL); err != nil {
		b.Fatalf("warmup navigate: %v", err)
	}
	return sb
}

// BenchmarkBrowserNavigate measures steady-state navigation (instance/tab warm).
func BenchmarkBrowserNavigate(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-nav")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sb.BrowserNavigate(ctx, pageURL); err != nil {
			b.Fatalf("BrowserNavigate: %v", err)
		}
	}
}

// BenchmarkBrowserSnapshot measures accessibility-snapshot extraction.
func BenchmarkBrowserSnapshot(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-snap")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sb.BrowserSnapshot(ctx, sandbox.SnapshotOpts{}); err != nil {
			b.Fatalf("BrowserSnapshot: %v", err)
		}
	}
}

// BenchmarkBrowserScreenshot measures full-page screenshot capture.
func BenchmarkBrowserScreenshot(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-shot")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sb.BrowserScreenshot(ctx); err != nil {
			b.Fatalf("BrowserScreenshot: %v", err)
		}
	}
}

// BenchmarkBrowserAction measures a coordinate click round-trip.
func BenchmarkBrowserAction(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-act")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sb.BrowserAction(ctx, sandbox.BrowserAction{
			Type: "click", X: 100, Y: 100,
		}); err != nil {
			b.Fatalf("BrowserAction: %v", err)
		}
	}
}

// BenchmarkBrowserEval measures a trivial JS evaluation round-trip — the
// floor of the gateway → pinchtab → Chrome → back path.
func BenchmarkBrowserEval(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-eval")
	ix := sb.(*IXSandbox)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ix.BrowserEval(ctx, "1+1"); err != nil {
			b.Fatalf("BrowserEval: %v", err)
		}
	}
}

// BenchmarkBrowserFirstUse measures the first browser op of a fresh chat:
// gateway ensureChat (start pinchtab instance + open tab) + navigation.
// Sandbox creation runs untimed.
func BenchmarkBrowserFirstUse(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	yes := true

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		waitPoolFill(mgr, 1)
		sid := fmt.Sprintf("bench-br-first-%d", i)
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid, Browser: &yes})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		b.StartTimer()

		if err := sb.BrowserNavigate(ctx, pageURL); err != nil {
			b.Fatalf("first navigate: %v", err)
		}

		b.StopTimer()
		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
		b.StartTimer()
	}
}

// BenchmarkBrowserE2E measures the full browser agent cycle:
// create (pool) → navigate → snapshot → action → text → destroy.
func BenchmarkBrowserE2E(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	yes := true

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		waitPoolFill(mgr, 1)
		b.StartTimer()

		sid := fmt.Sprintf("bench-br-e2e-%d", i)
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid, Browser: &yes})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		ix := sb.(*IXSandbox)
		if err := sb.BrowserNavigate(ctx, pageURL); err != nil {
			b.Fatalf("BrowserNavigate: %v", err)
		}
		if _, err := sb.BrowserSnapshot(ctx, sandbox.SnapshotOpts{}); err != nil {
			b.Fatalf("BrowserSnapshot: %v", err)
		}
		if _, err := sb.BrowserAction(ctx, sandbox.BrowserAction{Type: "click", X: 100, Y: 100}); err != nil {
			b.Fatalf("BrowserAction: %v", err)
		}
		if _, err := ix.BrowserText(ctx, sandbox.TextOpts{}); err != nil {
			b.Fatalf("BrowserText: %v", err)
		}
		if err := mgr.Destroy(ctx, ix.id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
	}
}

// BenchmarkBrowserNavigateRealSite is the opt-in realism check. Hermetic
// benchmarks above are the defaults; set IX_BENCH_REAL_SITE=https://example.com
// to also measure a real network fetch (noisy by nature).
func BenchmarkBrowserNavigateRealSite(b *testing.B) {
	site := os.Getenv("IX_BENCH_REAL_SITE")
	if site == "" {
		b.Skip("set IX_BENCH_REAL_SITE (e.g. https://example.com) to run")
	}
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-real")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sb.BrowserNavigate(ctx, site); err != nil {
			b.Fatalf("BrowserNavigate(%s): %v", site, err)
		}
	}
}
