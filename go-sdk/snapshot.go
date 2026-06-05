package ix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// SnapshotManager manages a single "golden" Firecracker snapshot that can be
// cloned cheaply for each new sandbox instead of cold-booting a VM.
//
// Lifecycle:
//  1. CreateGolden boots a fresh VM, waits for the daemon READY signal, pauses
//     the VM, writes a Full snapshot to disk, then kills the golden VM.
//  2. Restore clones the snapshot into a new VM by loading it via the
//     Firecracker snapshot/load API.  The resumed VM's daemon is already
//     running, so only a health poll is needed before returning.
type SnapshotManager struct {
	snapshotDir string
	rootfsImage string
	backend     *firecrackerBackend
	logger      *slog.Logger
	vcpus       int
	memMB       int64

	mu    sync.Mutex
	ready bool

	// scratchPool holds clone-ready copies of the golden scratch. Restore
	// renames one in (~µs) instead of paying a cp fork+exec on the hot path.
	//
	// Concurrency discipline: scratchPool and poolStop are assigned once under
	// mu in startScratchPool and never changed afterwards. takePooledScratch
	// reads them under mu then operates on the local copy. StopScratchPool
	// closes poolStop idempotently via poolStopOnce. The filler goroutine
	// closes over local copies of the channels so it never touches sm fields.
	scratchPool  chan string
	poolStop     chan struct{}
	poolStopOnce sync.Once
}

// NewSnapshotManager constructs a SnapshotManager. The manager is not ready
// until CreateGolden is called successfully.
func NewSnapshotManager(
	snapshotDir, rootfsImage string,
	backend *firecrackerBackend,
	vcpus int,
	memMB int64,
	logger *slog.Logger,
) *SnapshotManager {
	return &SnapshotManager{
		snapshotDir: snapshotDir,
		rootfsImage: rootfsImage,
		backend:     backend,
		vcpus:       vcpus,
		memMB:       memMB,
		logger:      logger,
	}
}

// statePath returns the path of the Firecracker snapshot state file.
func (sm *SnapshotManager) statePath() string {
	return filepath.Join(sm.snapshotDir, "snapshot.state")
}

// memPath returns the path of the Firecracker snapshot memory file.
func (sm *SnapshotManager) memPath() string {
	return filepath.Join(sm.snapshotDir, "snapshot.mem")
}

// scratchGoldenPath returns the path of the golden VM's preserved scratch
// disk. Every restored clone boots from a copy of this file: the clone's
// guest kernel resumes with in-memory ext4 state (journal position, page
// cache) that must byte-match the backing file as it was at pause time.
func (sm *SnapshotManager) scratchGoldenPath() string {
	return filepath.Join(sm.snapshotDir, "scratch.golden.ext4")
}

// Ready returns true if a golden snapshot exists and is ready to clone from.
// It is safe to call from multiple goroutines.
func (sm *SnapshotManager) Ready() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if !sm.ready {
		return false
	}
	// The golden scratch is required to clone; guard against a snapshot dir
	// that was created by an older version or partially deleted.
	_, err := os.Stat(sm.scratchGoldenPath())
	return err == nil
}

// CreateGolden boots a "golden" VM, waits for its daemon to be ready, takes a
// Full Firecracker snapshot, and shuts down the golden VM.
//
// On success, Ready() returns true and subsequent calls to Restore() will load
// this snapshot instead of cold-booting.
func (sm *SnapshotManager) CreateGolden(ctx context.Context) error {
	// A backend without a scratch template would boot a golden VM without a
	// scratch disk; the preserve step below would then fail with an opaque cp
	// error. Fail fast with a clear diagnostic instead.
	if sm.backend.scratchTemplate == "" {
		return fmt.Errorf("snapshot: backend has no scratch template configured")
	}

	if err := os.MkdirAll(sm.snapshotDir, 0o700); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}

	sm.logger.Info("snapshot: booting golden VM")

	handle, err := sm.backend.startVMCold(ctx, "golden-tmp", sm.vcpus, sm.memMB, sm.rootfsImage, nil, nil, true) // forceNoNet: golden snapshots stay vsock-only (snapshot networking is unsupported).
	if err != nil {
		return fmt.Errorf("start golden VM: %w", err)
	}
	// Always clean up the golden VM, even on error.
	defer sm.backend.cleanup(handle)

	sm.logger.Info("snapshot: waiting for daemon ready signal")

	if err := sm.backend.waitReady(ctx, handle); err != nil {
		return fmt.Errorf("wait for golden VM ready: %w", err)
	}

	sm.logger.Info("snapshot: daemon ready, pre-warming Python kernel")

	// Pre-warm the Python kernel before snapshotting: trigger a no-op execute
	// so the daemon's pool has a live stdin/stdout REPL process baked into the
	// snapshot. Restored clones skip the REPL spawn cost.
	warmupTransport := vsockTransport(handle.VsockPath)
	warmupHTTP := &http.Client{Transport: warmupTransport, Timeout: 2 * time.Minute}
	warmupClient := newClient("http://localhost", warmupHTTP)

	warmupCtx, warmupCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer warmupCancel()
	reader, err := warmupClient.postSSE(warmupCtx, "/v1/code/execute", map[string]any{
		"language": "python",
		"code":     "import sys; print('kernel ready', sys.version_info[:2])",
		"timeout":  120,
	})
	if err != nil {
		sm.logger.Warn("snapshot: Python warmup request failed, snapshotting without warm kernel", "error", err)
	} else {
		for reader.Next() {
			if reader.Event() == "complete" || reader.Event() == "error" {
				break
			}
		}
		reader.Close()
		sm.logger.Info("snapshot: Python kernel warmed")
	}

	sm.logger.Info("snapshot: pausing VM for snapshot")

	apiClient := fcAPIClient(handle.APISocket)

	// Pause the VM before snapshotting.
	if err := fcPatch(ctx, apiClient, "/vm", map[string]any{
		"state": "Paused",
	}); err != nil {
		return fmt.Errorf("pause golden VM: %w", err)
	}

	sm.logger.Info("snapshot: creating Full snapshot", "state", sm.statePath(), "mem", sm.memPath())

	// Create the snapshot on disk.
	if err := fcPut(ctx, apiClient, "/snapshot/create", map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": sm.statePath(),
		"mem_file_path": sm.memPath(),
	}); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}

	// Preserve the golden VM's scratch disk. Block IO is drained by
	// Firecracker during /snapshot/create (not merely on pause), so this copy
	// MUST stay after that call. Restore copies this per clone — see
	// scratchGoldenPath.
	if err := copySparse(filepath.Join(handle.SocketDir, scratchFileName), sm.scratchGoldenPath()); err != nil {
		return fmt.Errorf("preserve golden scratch: %w", err)
	}

	sm.logger.Info("snapshot: golden snapshot created successfully")

	// cleanup() is called via defer above; mark ready while still holding
	// nothing (cleanup is idempotent and we don't need the lock there).
	sm.mu.Lock()
	sm.ready = true
	sm.mu.Unlock()

	sm.startScratchPool()

	return nil
}

// Restore loads the golden snapshot into a new Firecracker process and returns
// a VMMHandle for that VM.
//
// The restored VM's daemon is already running (it was captured post-ready), so
// Restore only polls /health rather than waiting for a full READY handshake.
//
// CID is 0 and Net is nil — snapshot-restored VMs use neither a vsock CID nor
// a host TAP (networking is baked into the snapshot).
func (sm *SnapshotManager) Restore(ctx context.Context, sandboxID string) (*VMMHandle, error) {
	sm.mu.Lock()
	ready := sm.ready
	sm.mu.Unlock()

	if !ready {
		return nil, fmt.Errorf("snapshot not ready: call CreateGolden first")
	}

	socketDir := sm.backend.vmDir(sandboxID)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	// The rootfs is shared by all clones and attached read-only at the VMM
	// level; guests write to their private scratch disk via overlayfs. Give
	// this clone its own scratch: a byte-identical copy of the golden VM's
	// scratch at pause time, at the relative path recorded in snapshot.state
	// (Firecracker re-resolves "scratch.ext4" against this process's cwd).
	t := time.Now()
	// Per-clone scratch: prefer a pre-copied pool entry (rename, ~µs); fall
	// back to a sparse copy when the pool is empty.
	scratchDst := filepath.Join(socketDir, scratchFileName)
	if !sm.takePooledScratch(scratchDst) {
		if err := copySparse(sm.scratchGoldenPath(), scratchDst); err != nil {
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("create clone scratch: %w", err)
		}
	}
	tracePhase(sm.logger, "restore", "scratch", t)

	apiSocket := filepath.Join(socketDir, "fc.sock")
	vsockUDS := filepath.Join(socketDir, "vsock.uds")

	// Start a bare Firecracker process with working dir = socketDir.
	// The snapshot's relative vsock UDS path ("vsock.uds") resolves here.
	cmd := exec.CommandContext(ctx, sm.backend.fcBinary,
		"--api-sock", apiSocket,
	)
	cmd.Dir = socketDir
	cmd.Stdout = nil
	cmd.Stderr = nil

	t = time.Now()
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	tracePhase(sm.logger, "restore", "spawn", t)

	// Wait for the API socket before sending commands.
	t = time.Now()
	if err := waitForFile(apiSocket, 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("firecracker API socket not ready: %w", err)
	}
	tracePhase(sm.logger, "restore", "apisock", t)

	apiClient := fcAPIClient(apiSocket)

	sm.logger.Info("snapshot: loading snapshot", "sandbox", sandboxID)

	// Load the snapshot; resume_vm:true means Firecracker starts the VM
	// immediately after loading.
	t = time.Now()
	if err := fcPut(ctx, apiClient, "/snapshot/load", map[string]any{
		"snapshot_path": sm.statePath(),
		"mem_backend": map[string]any{
			"backend_path": sm.memPath(),
			"backend_type": "File",
		},
		"resume_vm": true,
	}); err != nil {
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	tracePhase(sm.logger, "restore", "load", t)

	// Build an HTTP client that talks to the guest daemon via vsock transport.
	// The daemon was running when the snapshot was taken, so it comes up
	// immediately; we just poll until it responds.
	guestHTTP := &http.Client{
		Transport: vsockTransport(vsockUDS),
		Timeout:   2 * time.Second,
	}
	t = time.Now()
	if err := waitHealthy(ctx, guestHTTP); err != nil {
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("guest daemon health check failed: %w", err)
	}
	tracePhase(sm.logger, "restore", "health", t)

	sm.logger.Info("snapshot: restore complete", "sandbox", sandboxID)

	return &VMMHandle{
		Process:   cmd.Process,
		SocketDir: socketDir,
		VsockPath: vsockUDS,
		APISocket: apiSocket,
		CID:       0, // not used for snapshot clones
	}, nil
}

// scratchPoolSize is how many clone-ready scratch copies are kept warm.
const scratchPoolSize = 4

// startScratchPool launches the background filler. Idempotent per manager:
// CreateGolden calls it once the golden scratch exists.
//
// The pool directory is placed under the backend's runDir so pool files live
// on the same filesystem as the per-VM scratch destinations (runDir/ix-<id>/).
// os.Rename across filesystems always fails with EXDEV; same-filesystem means
// the rename is atomic and sub-millisecond. The ix-scratch-pool prefix is
// intentional: the manager's recover() sweep (reaper.go) removes orphaned
// ix-* dirs under runDir, so stale pool files are cleaned up after a crash.
//
// Channel assignments are made under mu once, then never changed. The filler
// goroutine closes over LOCAL copies of the channels so it never reads sm
// fields after startup — this is the key discipline that prevents data races.
func (sm *SnapshotManager) startScratchPool() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.scratchPool != nil {
		return // already started
	}

	// Derive pool dir from backend.runDir to guarantee same-filesystem renames.
	// Fall back to snapshotDir when backend is nil (unit-test ergonomics).
	var base string
	if sm.backend != nil && sm.backend.runDir != "" {
		base = sm.backend.runDir
	} else {
		base = sm.snapshotDir
	}
	poolDir := filepath.Join(base, "ix-scratch-pool")
	if err := os.MkdirAll(poolDir, 0o700); err != nil {
		sm.logger.Warn("scratch pool: failed to create pool dir, pool disabled", "error", err)
		return
	}

	// Create channels as locals, assign to fields, pass locals to goroutine.
	// The goroutine NEVER reads sm.scratchPool or sm.poolStop — only the locals.
	pool := make(chan string, scratchPoolSize)
	stop := make(chan struct{})
	sm.scratchPool = pool
	sm.poolStop = stop

	goldenPath := sm.scratchGoldenPath() // capture under lock; immutable after CreateGolden

	go func() {
		for i := 0; ; i++ {
			// Check for stop before each copy to exit promptly.
			select {
			case <-stop:
				return
			default:
			}
			p := filepath.Join(poolDir, fmt.Sprintf("scratch.pool.%d.ext4", i))
			if err := copySparse(goldenPath, p); err != nil {
				sm.logger.Warn("scratch pool copy failed", "error", err)
				select {
				case <-stop:
					return
				case <-time.After(time.Second):
				}
				continue
			}
			select {
			case pool <- p:
			case <-stop:
				_ = os.Remove(p)
				return
			}
		}
	}()
}

// StopScratchPool stops the filler and removes unconsumed pool files.
// Idempotent: safe to call multiple times; poolStopOnce ensures close is
// never repeated. Reads sm fields under mu into locals before operating.
func (sm *SnapshotManager) StopScratchPool() {
	sm.mu.Lock()
	pool := sm.scratchPool
	stop := sm.poolStop
	sm.mu.Unlock()

	if stop == nil {
		return // pool was never started
	}

	// Use poolStopOnce so concurrent or repeated StopScratchPool calls cannot
	// double-close the channel (which would panic with "close of closed channel").
	sm.poolStopOnce.Do(func() { close(stop) })

	// Drain unconsumed pool files and remove them. pool is a buffered channel;
	// after close(stop) the filler will not send any more entries, so this loop
	// terminates once the buffer is empty.
	for {
		select {
		case p := <-pool:
			_ = os.Remove(p)
		default:
			return
		}
	}
}

// takePooledScratch moves one pre-copied scratch into dst. False = pool
// empty or disabled; the caller falls back to copySparse.
//
// Reads sm.scratchPool under mu into a local, then operates on the local —
// this avoids a race with StopScratchPool which also reads the field.
func (sm *SnapshotManager) takePooledScratch(dst string) bool {
	sm.mu.Lock()
	pool := sm.scratchPool
	sm.mu.Unlock()

	if pool == nil {
		return false
	}
	select {
	case p := <-pool:
		if err := os.Rename(p, dst); err != nil {
			sm.logger.Warn("scratch pool rename failed; falling back to copy", "error", err)
			_ = os.Remove(p)
			return false
		}
		return true
	default:
		return false
	}
}

// waitHealthy polls GET /health until the guest daemon responds with HTTP 200,
// or until the context deadline / 10 s timeout expires. Used by snapshot restore
// (ixd's /health needs no auth).
func waitHealthy(ctx context.Context, httpClient *http.Client) error {
	return waitHealthyAuth(ctx, httpClient, "", 10*time.Second)
}

// waitHealthyAuth polls GET /health until HTTP 200, sending an optional Bearer
// token, until ctx is done or the timeout expires. The browser tier uses this
// because pinchtab's /health requires the Authorization header and Chrome
// cold-start can exceed the snapshot-restore 10 s budget.
func waitHealthyAuth(ctx context.Context, httpClient *http.Client, bearer string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Millisecond)
	defer ticker.Stop()

	for {
		// Probe immediately — a snapshot-restored daemon is usually already
		// serving, so waiting a tick before the first probe is pure latency.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/health", nil)
		if err != nil {
			return fmt.Errorf("build health request: %w", err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := httpClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("guest daemon health check timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// fcPatch sends a JSON PATCH request to the Firecracker API and checks the status.
// It has the same semantics as fcPut but uses the HTTP PATCH method.
func fcPatch(ctx context.Context, client *http.Client, path string, body map[string]any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PATCH %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("PATCH %s: HTTP %d: %s", path, resp.StatusCode, respBody)
	}
	return nil
}
