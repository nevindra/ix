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

// Ready returns true if a golden snapshot exists and is ready to clone from.
// It is safe to call from multiple goroutines.
func (sm *SnapshotManager) Ready() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.ready
}

// CreateGolden boots a "golden" VM, waits for its daemon to be ready, takes a
// Full Firecracker snapshot, and shuts down the golden VM.
//
// On success, Ready() returns true and subsequent calls to Restore() will load
// this snapshot instead of cold-booting.
func (sm *SnapshotManager) CreateGolden(ctx context.Context) error {
	if err := os.MkdirAll(sm.snapshotDir, 0o700); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}

	sm.logger.Info("snapshot: booting golden VM")

	handle, err := sm.backend.startVMCold(ctx, "golden-tmp", sm.vcpus, sm.memMB, sm.rootfsImage, nil)
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

	// Pre-warm the Python kernel before snapshotting. This boots ipykernel +
	// ZMQ inside the VM so restored clones skip the ~15s kernel startup.
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

	sm.logger.Info("snapshot: golden snapshot created successfully")

	// cleanup() is called via defer above; mark ready while still holding
	// nothing (cleanup is idempotent and we don't need the lock there).
	sm.mu.Lock()
	sm.ready = true
	sm.mu.Unlock()

	return nil
}

// Restore loads the golden snapshot into a new Firecracker process and returns
// a VMMHandle for that VM.
//
// The restored VM's daemon is already running (it was captured post-ready), so
// Restore only polls /health rather than waiting for a full READY handshake.
//
// CID and PasstPID are 0 — snapshot-restored VMs do not use vsock CIDs or
// passt for networking (those are baked into the snapshot).
func (sm *SnapshotManager) Restore(ctx context.Context, sandboxID string) (*VMMHandle, error) {
	sm.mu.Lock()
	ready := sm.ready
	sm.mu.Unlock()

	if !ready {
		return nil, fmt.Errorf("snapshot not ready: call CreateGolden first")
	}

	socketDir := filepath.Join(os.TempDir(), "ix-"+sandboxID)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	// Skip rootfs copy — all clones share the base image. Firecracker reopens
	// the same path_on_host from the snapshot. The VM uses overlayfs or tmpfs
	// for writes internally, so the base image stays clean.
	// TODO: if rootfs corruption is observed, re-enable copyRootfs or use
	// device-mapper thin snapshots.

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

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	// Wait for the API socket before sending commands.
	if err := waitForFile(apiSocket, 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("firecracker API socket not ready: %w", err)
	}

	apiClient := fcAPIClient(apiSocket)

	sm.logger.Info("snapshot: loading snapshot", "sandbox", sandboxID)

	// Load the snapshot; resume_vm:true means Firecracker starts the VM
	// immediately after loading.
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

	// Build an HTTP client that talks to the guest daemon via vsock transport.
	// The daemon was running when the snapshot was taken, so it comes up
	// immediately; we just poll until it responds.
	guestHTTP := &http.Client{
		Transport: vsockTransport(vsockUDS),
		Timeout:   2 * time.Second,
	}
	if err := waitHealthy(ctx, guestHTTP); err != nil {
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("guest daemon health check failed: %w", err)
	}

	sm.logger.Info("snapshot: restore complete", "sandbox", sandboxID)

	return &VMMHandle{
		Process:   cmd.Process,
		SocketDir: socketDir,
		VsockPath: vsockUDS,
		APISocket: apiSocket,
		CID:       0, // not used for snapshot clones
		PasstPID:  0, // not used for snapshot clones
	}, nil
}

// waitHealthy polls GET /health on the given HTTP client until the guest daemon
// responds with HTTP 200, or until the context deadline / 10 s timeout expires.
func waitHealthy(ctx context.Context, httpClient *http.Client) error {
	deadline := time.Now().Add(10 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("guest daemon health check timed out after 10s")
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/health", nil)
			if err != nil {
				return fmt.Errorf("build health request: %w", err)
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				// Daemon not up yet — keep polling.
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

// copyRootfs copies src to dst using cp with --reflink=auto and --sparse=always
// for efficient copy-on-write on supported filesystems.
func copyRootfs(src, dst string) error {
	cmd := exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp rootfs %s → %s: %w: %s", src, dst, err, out)
	}
	return nil
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
