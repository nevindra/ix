package ix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// VMMHandle holds all state for a running VM managed by the firecrackerBackend.
type VMMHandle struct {
	Process   *os.Process
	SocketDir string
	VsockPath string // path to vsock UDS file (used by Firecracker vsock device)
	APISocket string // Firecracker API socket path
	CID       uint32 // AF_VSOCK context ID assigned to this VM
	Net       *vmNet // per-VM TAP networking; nil = vsock-only (snapshot/disabled)
}

// FirecrackerConfig holds Firecracker-specific configuration.
type FirecrackerConfig struct {
	Binary      string // path to firecracker binary; empty searches PATH
	KernelPath  string // path to vmlinux kernel (required)
	RootfsImage string // path to ext4 rootfs image (required)
}

// Validate checks that required fields are set.
func (c *FirecrackerConfig) Validate() error {
	if c.KernelPath == "" {
		return fmt.Errorf("KernelPath is required")
	}
	if c.RootfsImage == "" {
		return fmt.Errorf("RootfsImage is required")
	}
	return nil
}

// driveSpec is a Firecracker /drives/{id} PUT body.
type driveSpec map[string]any

// buildDriveSpec builds a non-root Firecracker drive body. readOnly toggles
// is_read_only; is_root_device is always false (rootfs uses its own setup).
func buildDriveSpec(id, pathOnHost string, readOnly bool) driveSpec {
	return driveSpec{
		"drive_id":       id,
		"path_on_host":   pathOnHost,
		"is_root_device": false,
		"is_read_only":   readOnly,
	}
}

// cidCounter allocates unique CIDs for each VM.
// CIDs 0-2 are reserved (VMADDR_CID_ANY, VMADDR_CID_HYPERVISOR, VMADDR_CID_HOST).
var cidCounter atomic.Uint32

// tombstoneCounter disambiguates concurrent tombstones for the same dir name.
var tombstoneCounter atomic.Uint64

func init() {
	cidCounter.Store(2) // next allocation returns 3
}

// allocateCID returns the next available vsock context ID.
func allocateCID() uint32 {
	return cidCounter.Add(1)
}

// buildKernelBootArgs constructs the kernel command line for the Firecracker VM.
// Environment variables from envSlice are injected as ix.env.KEY=VALUE entries
// so the ix-init script can read them from /proc/cmdline. When net is non-nil,
// an ip= argument autoconfigures eth0 at boot.
//
// withConsole routes the guest serial console to Firecracker stdout (set
// IX_VM_CONSOLE=1 on the host). Default is OFF: every serial byte is an
// emulated-UART vmexit, so console output taxes both boot time and any guest
// process that logs to stdout. 8250.nr_uarts=0 removes the serial driver
// entirely; quiet suppresses printk to the (absent) console.
func buildKernelBootArgs(envSlice []string, net *vmNet, withConsole bool) string {
	var parts []string
	if withConsole {
		parts = append(parts, "console=ttyS0")
	} else {
		parts = append(parts, "8250.nr_uarts=0", "quiet")
	}
	parts = append(parts,
		"reboot=k",
		"panic=1",
		"pci=off",
		"nomodules",
		"random.trust_cpu=on",
		"i8042.noaux",
		"i8042.nomux",
		"i8042.nopnp",
		"i8042.dumbkbd",
		"root=/dev/vda",
		"ro", // shared rootfs: all writes go to the per-VM scratch via overlayfs (ix-stage0)
		"init=/sbin/ix-stage0",
	)
	if net != nil {
		parts = append(parts, fmt.Sprintf(
			"ip=%s::%s:%s::eth0:off:8.8.8.8", net.guestIP, net.hostIP, net.mask))
	}
	hasRustLog := false
	for _, e := range envSlice {
		if strings.HasPrefix(e, "RUST_LOG=") {
			hasRustLog = true
		}
		parts = append(parts, "ix.env."+e)
	}
	if !hasRustLog {
		// ixd logs at info per request; with the console wired to an emulated
		// UART that is measurable per-op latency. warn keeps real problems.
		parts = append(parts, "ix.env.RUST_LOG=warn")
	}
	return strings.Join(parts, " ")
}

// firecrackerBackend implements VM lifecycle management using Firecracker.
type firecrackerBackend struct {
	fcBinary        string
	kernelPath      string
	rootfsImage     string
	logger          *slog.Logger
	snapshot        *SnapshotManager // optional; when set, startVM uses snapshot restore
	tapAlloc        *tapAllocator    // TAP index allocator (set by the manager; nil only in tests that never cold-boot)
	disableNet      bool             // skip TAP setup entirely (vsock-only VM)
	runDir          string           // base dir for per-VM dirs (sockets + scratch); empty = os.TempDir()
	scratchTemplate string           // empty sparse ext4 copied per VM as the scratch drive; empty = no scratch (bare test backends)
}

// vmDir returns the per-VM runtime directory (Firecracker CWD: API socket,
// vsock UDS, and the per-VM scratch disk all live here).
func (fb *firecrackerBackend) vmDir(sandboxID string) string {
	base := fb.runDir
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "ix-"+sandboxID)
}

// startVM is the primary VM-creation entry point.  When a SnapshotManager is
// attached and has a ready golden snapshot, it delegates to snapshot.Restore for
// ~10x faster VM startup.  Otherwise it falls back to a full cold boot.
func (fb *firecrackerBackend) startVM(ctx context.Context, sandboxID string, vcpus int, memMB int64, rootfsImage string, envSlice []string, extraDrives []driveSpec) (*VMMHandle, error) {
	if fb.snapshot != nil && fb.snapshot.Ready() && rootfsImage == fb.rootfsImage {
		// extraDrives are intentionally not forwarded here: a snapshot-restored
		// VM uses the drive set baked into the golden snapshot, so extra drives
		// cannot be injected. Callers needing extra drives (e.g. the browser
		// tier's state disk) cold-boot via startVMCold directly.
		return fb.snapshot.Restore(ctx, sandboxID)
	}
	return fb.startVMCold(ctx, sandboxID, vcpus, memMB, rootfsImage, envSlice, extraDrives, false)
}

// startVMCold launches a Firecracker VM and returns a VMMHandle on success.
//
// Sequence:
//  1. Allocate CID and create the per-VM dir (runDir/ix-{sandboxID}) with its scratch disk
//  2. Create a per-VM TAP and wire it via PUT /network-interfaces
//  3. Start Firecracker process with --api-sock
//  4. Wait for API socket file to appear (5 s timeout)
//  5. Configure VM via PUT requests to the Firecracker API
//  6. Start the VM via PUT /actions InstanceStart
//
// On any error after process creation, the process and TAP are torn down and
// the socket dir is removed before returning.
//
// When forceNoNet is true (used by the snapshot golden boot), no TAP is created
// and the VM is vsock-only regardless of fb.disableNet.
func (fb *firecrackerBackend) startVMCold(ctx context.Context, sandboxID string, vcpus int, memMB int64, rootfsImage string, envSlice []string, extraDrives []driveSpec, forceNoNet bool) (*VMMHandle, error) {
	cid := allocateCID()

	socketDir := fb.vmDir(sandboxID)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	// Per-VM scratch disk: every VM gets a private writable ext4 copied from
	// the empty template. The rootfs itself is attached read-only below — the
	// guest writes only to this scratch via overlayfs (ix-stage0).
	tScratch := time.Now()
	if fb.scratchTemplate != "" {
		if err := copySparse(fb.scratchTemplate, filepath.Join(socketDir, scratchFileName)); err != nil {
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("create scratch disk: %w", err)
		}
	}
	tracePhase(fb.logger, "coldboot", "scratch", tScratch)

	apiSocket := filepath.Join(socketDir, "fc.sock")
	vsockUDS := filepath.Join(socketDir, "vsock.uds")

	// Per-VM TAP networking (skipped when disabled). On any later error we must
	// tear the tap down and release its index, so track them for the closure.
	var vn *vmNet
	if !fb.disableNet && !forceNoNet {
		tTap := time.Now()
		if fb.tapAlloc == nil {
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("networking enabled but tap allocator not configured")
		}
		idx, err := fb.tapAlloc.alloc()
		if err != nil {
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("allocate tap index: %w", err)
		}
		net, err := setupTap(ctx, idx)
		if err != nil {
			fb.tapAlloc.release(idx)
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("setup tap: %w", err)
		}
		vn = &net
		tracePhase(fb.logger, "coldboot", "tap", tTap)
	}

	// cleanupOnErr undoes everything created so far. Call on every error path
	// after this point.
	cleanupOnErr := func(proc *os.Process) {
		if proc != nil {
			_ = proc.Kill()
		}
		if vn != nil {
			if err := teardownTap(context.Background(), *vn); err != nil && fb.logger != nil {
				fb.logger.Warn("teardown tap (error path)", "tap", vn.tapName, "error", err)
			}
			if fb.tapAlloc != nil {
				fb.tapAlloc.release(vn.idx)
			}
		}
		_ = os.RemoveAll(socketDir)
	}

	cmd := exec.CommandContext(ctx, fb.fcBinary, "--api-sock", apiSocket)
	cmd.Dir = socketDir
	cmd.Stdout = nil
	cmd.Stderr = nil
	// The guest serial console (console=ttyS0) is routed to Firecracker's stdout.
	// Discarded by default; set IX_VM_CONSOLE=1 to surface boot + init logs on
	// stderr for debugging (e.g. why the browser-tier pinchtab never gets healthy).
	if os.Getenv("IX_VM_CONSOLE") != "" {
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
	}

	tSpawn := time.Now()
	if err := cmd.Start(); err != nil {
		cleanupOnErr(nil)
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	tracePhase(fb.logger, "coldboot", "spawn", tSpawn)

	tAPISock := time.Now()
	if err := waitForFile(apiSocket, 5*time.Second); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("firecracker API socket not ready: %w", err)
	}
	tracePhase(fb.logger, "coldboot", "apisock", tAPISock)

	apiClient := fcAPIClient(apiSocket)
	bootArgs := buildKernelBootArgs(envSlice, vn, os.Getenv("IX_VM_CONSOLE") != "")

	tConfig := time.Now()
	if err := fcPut(ctx, apiClient, "/boot-source", map[string]any{
		"kernel_image_path": fb.kernelPath,
		"boot_args":         bootArgs,
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("set boot source: %w", err)
	}

	// The rootfs is one shared image for every VM on this host: it MUST be
	// read-only at the VMM level. Concurrent rw mounts of one ext4 image from
	// multiple guest kernels corrupt the filesystem and let one chat's writes
	// persist into the template all future VMs boot from.
	if err := fcPut(ctx, apiClient, "/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsImage,
		"is_root_device": true,
		"is_read_only":   true,
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("set rootfs drive: %w", err)
	}

	// Per-VM writable scratch (overlay upper layer). Registered with a
	// RELATIVE path resolved against cmd.Dir (= socketDir), like vsock.uds:
	// snapshot.state records the relative string, so each restored clone
	// re-resolves it to its own scratch copy.
	if fb.scratchTemplate != "" {
		if err := fcPut(ctx, apiClient, "/drives/scratch", map[string]any{
			"drive_id":       "scratch",
			"path_on_host":   scratchFileName,
			"is_root_device": false,
			"is_read_only":   false,
		}); err != nil {
			cleanupOnErr(cmd.Process)
			return nil, fmt.Errorf("set scratch drive: %w", err)
		}
	}

	for _, d := range extraDrives {
		id, _ := d["drive_id"].(string)
		if err := fcPut(ctx, apiClient, "/drives/"+id, d); err != nil {
			cleanupOnErr(cmd.Process)
			return nil, fmt.Errorf("set drive %s: %w", id, err)
		}
	}

	// Network interface (TAP). Must precede InstanceStart. Skipped when disabled.
	if vn != nil {
		if err := fcPut(ctx, apiClient, "/network-interfaces/eth0", map[string]any{
			"iface_id":      "eth0",
			"guest_mac":     vn.guestMAC,
			"host_dev_name": vn.tapName,
		}); err != nil {
			cleanupOnErr(cmd.Process)
			return nil, fmt.Errorf("set network interface: %w", err)
		}
	}

	if err := fcPut(ctx, apiClient, "/machine-config", map[string]any{
		"vcpu_count":   vcpus,
		"mem_size_mib": memMB,
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("set machine config: %w", err)
	}

	if err := fcPut(ctx, apiClient, "/vsock", map[string]any{
		"guest_cid": cid,
		"uds_path":  "vsock.uds",
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("set vsock: %w", err)
	}
	tracePhase(fb.logger, "coldboot", "config", tConfig)

	tInstanceStart := time.Now()
	if err := fcPut(ctx, apiClient, "/actions", map[string]any{
		"action_type": "InstanceStart",
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("start VM instance: %w", err)
	}
	tracePhase(fb.logger, "coldboot", "instancestart", tInstanceStart)

	return &VMMHandle{
		Process:   cmd.Process,
		SocketDir: socketDir,
		VsockPath: vsockUDS,
		APISocket: apiSocket,
		CID:       cid,
		Net:       vn,
	}, nil
}

// cleanup kills the Firecracker process, tears down the VM's TAP (if any), and
// removes the socket directory.
func (fb *firecrackerBackend) cleanup(handle *VMMHandle) {
	if handle == nil {
		return
	}
	if handle.Process != nil {
		_ = handle.Process.Kill()
		_, _ = handle.Process.Wait()
	}
	if handle.Net != nil {
		if err := teardownTap(context.Background(), *handle.Net); err != nil && fb.logger != nil {
			fb.logger.Warn("teardown tap", "tap", handle.Net.tapName, "error", err)
		}
		if fb.tapAlloc != nil {
			fb.tapAlloc.release(handle.Net.idx)
		}
	}
	if handle.SocketDir != "" {
		// Two-phase destroy: synchronously rename the dir to a tombstone so
		// the sandbox path is fenced (no new VM can collide with stale files),
		// then delete in the background. Same total IO, off the caller's
		// latency path. Trade-offs (documented in BENCHMARKS.md v0.7):
		//   - disk reclaim lags by ms-to-seconds under churn (ENOSPC window
		//     on a nearly-full host; the reaper's disk-pressure path remains
		//     the backstop)
		//   - "Destroy returned" no longer implies "disk clean"; a manager
		//     crash can orphan tombstones — recover() sweeps ix-* dirs
		//     (tombstones keep the ix- prefix) at startup.
		tomb := fmt.Sprintf("%s.deleting.%d", handle.SocketDir, tombstoneCounter.Add(1))
		if err := os.Rename(handle.SocketDir, tomb); err != nil {
			// Rename failed (already gone, cross-device, ...) — fall back to
			// the old synchronous delete rather than leak.
			_ = os.RemoveAll(handle.SocketDir)
			return
		}
		go func() { _ = os.RemoveAll(tomb) }()
	}
}

// --- Helpers ---

// fcAPIClient creates an HTTP client that dials the Firecracker API Unix socket.
func fcAPIClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// fcPut sends a JSON PUT request to the Firecracker API and checks the status.
func fcPut(ctx context.Context, client *http.Client, path string, body map[string]any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("PUT %s: HTTP %d: %s", path, resp.StatusCode, respBody)
	}
	return nil
}
