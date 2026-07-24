//go:build integration

package ix

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// These tests assert the sliding-idle + warm-reuse + in-flight-guard behavior
// that lets a chat conversation keep one warm VM across turns. They drive the
// reaper directly (mgr.reapExpired) instead of waiting on its 30s ticker, so
// timing is deterministic. Requires a KVM host + FC artifacts; run with
//   sudo -E env "PATH=$PATH" go test -tags integration ./go-sdk/ -run Lifecycle -v
func newLifecycleManager(t *testing.T, idleTTL time.Duration) *IXManager {
	t.Helper()
	if rootfsImage() == "" || kernelPath() == "" {
		t.Skip("set IX_ROOTFS_IMAGE, IX_KERNEL_PATH, IX_FC_BINARY to run")
	}
	mgr, err := NewManager(context.Background(), ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  idleTTL,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })
	return mgr
}

func vmPID(t *testing.T, s sandbox.Sandbox) int {
	t.Helper()
	ix, ok := s.(*IXSandbox)
	if !ok {
		t.Fatalf("sandbox is %T, not *IXSandbox", s)
	}
	return ix.vmm.Process.Pid
}

// Warm reuse: after a turn finishes we deliberately do NOT destroy the VM, so a
// follow-up turn Gets the same live VM (same pid) with the prior turn's files
// still in it — the whole point of keeping a chat sandbox warm.
func TestLifecycleWarmReuseAcrossTurns(t *testing.T) {
	ctx := context.Background()
	mgr := newLifecycleManager(t, 2*time.Minute)

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "warm-reuse"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pid1 := vmPID(t, sb)

	// Turn 1 writes a file. No Destroy — the sandbox stays warm.
	if err := sb.WriteFile(ctx, sandbox.WriteFileRequest{Path: "/tmp/turn1.txt", Content: "from turn 1"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Turn 2 in the same conversation reuses the VM via Get.
	sb2, err := mgr.Get("warm-reuse")
	if err != nil {
		t.Fatalf("get (warm reuse): %v", err)
	}
	if pid2 := vmPID(t, sb2); pid2 != pid1 {
		t.Fatalf("warm reuse booted a new VM: pid1=%d pid2=%d", pid1, pid2)
	}
	content, err := sb2.ReadFile(ctx, sandbox.ReadFileRequest{Path: "/tmp/turn1.txt"})
	if err != nil {
		t.Fatalf("read turn1 file on reuse: %v", err)
	}
	if !strings.Contains(content.Content, "from turn 1") {
		t.Fatalf("turn 2 lost turn 1's file: got %q", content.Content)
	}
}

// Sliding idle: activity keeps refreshing the deadline so a busy conversation
// outlives its original TTL; once it goes quiet past the window, the reaper
// destroys it.
func TestLifecycleSlidingIdleReap(t *testing.T) {
	ctx := context.Background()
	idle := 300 * time.Millisecond
	mgr := newLifecycleManager(t, idle)

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "sliding-idle"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Stay active across several windows; each call touches the deadline.
	for i := 0; i < 4; i++ {
		if _, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "true"}); err != nil {
			t.Fatalf("keepalive shell: %v", err)
		}
		time.Sleep(idle / 2)
		mgr.reapExpired(ctx)
		if _, err := mgr.Get("sliding-idle"); err != nil {
			t.Fatalf("active sandbox was reaped at iter %d: %v", i, err)
		}
	}

	// Go quiet past the idle window → next reap destroys it.
	time.Sleep(idle * 3)
	mgr.reapExpired(ctx)
	if _, err := mgr.Get("sliding-idle"); err == nil {
		t.Fatal("idle sandbox past its window was not reaped")
	}
}

// In-flight guard: a VM with an in-flight request is never idle-reaped, even
// after its idle deadline has passed — a VM serving an exec is alive by
// definition (a CPU-bound task can starve /health).
func TestLifecycleInFlightNotReaped(t *testing.T) {
	ctx := context.Background()
	idle := 200 * time.Millisecond
	mgr := newLifecycleManager(t, idle)

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "inflight"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ixsb := sb.(*IXSandbox)

	// Long-running exec keeps inflight>0 for ~3s, well past the idle window.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = sb.ExecCode(ctx, sandbox.CodeRequest{
			Language: "python",
			Code:     "import time; time.sleep(3)",
		})
	}()

	// Wait until the exec is actually in-flight.
	deadline := time.Now().Add(5 * time.Second)
	for ixsb.inflight.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("exec never registered as in-flight")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Past the idle window but still in-flight → reap must skip it.
	time.Sleep(idle * 2)
	mgr.reapExpired(ctx)
	if _, err := mgr.Get("inflight"); err != nil {
		t.Fatalf("in-flight sandbox was wrongly reaped: %v", err)
	}

	// After the exec finishes and it goes quiet, it becomes reapable.
	wg.Wait()
	time.Sleep(idle * 2)
	mgr.reapExpired(ctx)
	if _, err := mgr.Get("inflight"); err == nil {
		t.Fatal("quiesced sandbox was not reaped after exec finished")
	}
}
