package ix

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/nevindra/oasis/sandbox"
)

// monitor periodically health-checks every sandbox and restarts or marks
// failed those that exceed the consecutive failure threshold.
func (m *IXManager) monitor(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.HealthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			snapshot := maps.Clone(m.sandboxes)
			m.mu.RUnlock()

			for sessionID, sb := range snapshot {
				// A VM with an in-flight request is alive by definition; a
				// CPU-bound task can starve /health, so never restart it mid-work.
				if sb.inflight.Load() > 0 || sb.healthCheck(ctx, m.cfg.HealthTimeout) {
					m.mu.Lock()
					if cur, ok := m.sandboxes[sessionID]; ok {
						cur.failCount = 0
					}
					m.mu.Unlock()
					continue
				}

				m.mu.Lock()
				cur, ok := m.sandboxes[sessionID]
				if !ok {
					m.mu.Unlock()
					continue
				}
				cur.failCount++
				failCount := cur.failCount
				restartCount := cur.restartCount
				m.mu.Unlock()

				if failCount >= m.cfg.HealthFailureThreshold {
					if restartCount < m.cfg.MaxRestarts {
						m.restart(ctx, sessionID)
					} else {
						m.markFailed(ctx, sessionID)
					}
				}
			}
		}
	}
}

// restart kills the old VM process, removes the old socket dir, and starts
// a fresh Firecracker VM for the same session ID, preserving the restart count.
func (m *IXManager) restart(ctx context.Context, sessionID string) {
	m.mu.Lock()
	old, ok := m.sandboxes[sessionID]
	if !ok {
		m.mu.Unlock()
		return
	}
	oldVMM := old.vmm
	oldRestartCount := old.restartCount
	idleTTL := old.idleTTL
	remainingIdle := time.Until(time.Unix(0, old.idleDeadline.Load()))
	if remainingIdle < 0 {
		remainingIdle = 0
	}
	m.mu.Unlock()

	// Kill old VM and clean up.
	old.Close()
	m.vmm.cleanup(oldVMM)

	// Create replacement VM.
	sandboxID := uuid.NewString()[:12]
	now := time.Now()

	vcpus := m.cfg.PerSandbox.VCPUs
	if vcpus < 1 {
		vcpus = 1
	}
	memMB := m.cfg.PerSandbox.Memory >> 20
	if memMB < 128 {
		memMB = 128
	}

	envSlice := m.buildEnvSlice(nil, sessionID, nil)

	handle, err := m.vmm.startVM(ctx, sandboxID, vcpus, memMB, m.cfg.RootfsImage, envSlice, nil)
	if err != nil {
		m.logger.Error("restart: start VM failed", "session", sessionID, "error", err)
		return
	}

	if m.vmm.snapshot == nil || !m.vmm.snapshot.Ready() {
		if err := m.vmm.waitReady(ctx, handle); err != nil {
			m.vmm.cleanup(handle)
			m.logger.Error("restart: wait ready failed", "session", sessionID, "error", err)
			return
		}
	}

	transport := vsockTransport(handle.VsockPath)
	httpClient := &http.Client{Transport: transport, Timeout: 2 * time.Minute}
	newSb := &IXSandbox{
		id:           sessionID,
		vmm:          handle,
		baseURL:      "http://localhost",
		client:       newClient("http://localhost", httpClient),
		createdAt:    now,
		idleTTL:      idleTTL,
		restartCount: oldRestartCount + 1,
		shellSession: "default",
	}
	newSb.idleDeadline.Store(now.Add(remainingIdle).UnixNano())

	m.mu.Lock()
	m.sandboxes[sessionID] = newSb
	m.mu.Unlock()

	m.logger.Info("sandbox restarted",
		"session", sessionID,
		"pid", handle.Process.Pid,
		"restartCount", newSb.restartCount,
	)

	m.restartsTotal.Add(1)
}

// markFailed logs a circuit-breaker event and destroys the sandbox.
func (m *IXManager) markFailed(ctx context.Context, sessionID string) {
	m.failuresTotal.Add(1)
	m.logger.Error("circuit breaker: sandbox exceeded max restarts, destroying",
		"session", sessionID,
		"maxRestarts", m.cfg.MaxRestarts,
	)
	if err := m.destroy(ctx, sessionID); err != nil {
		m.logger.Warn("markFailed: destroy failed",
			"session", sessionID,
			"error", fmt.Errorf("destroy: %w", err),
		)
	}
}

// Health returns a passive snapshot of runtime readiness and pool state. It
// reads only already-tracked state — no VM is launched or mutated.
func (m *IXManager) Health(ctx context.Context) (sandbox.Health, error) {
	if err := ctx.Err(); err != nil {
		return sandbox.Health{}, err
	}

	rt := sandbox.RuntimeInfo{
		Backend:       "firecracker",
		KernelPath:    m.cfg.KernelPath,
		KernelOK:      fileReadable(m.cfg.KernelPath),
		RootfsImage:   m.cfg.RootfsImage,
		RootfsOK:      fileReadable(m.cfg.RootfsImage),
		FCBinary:      m.cfg.FCBinary,
		FCBinaryOK:    fileExecutable(m.cfg.FCBinary),
		KVMAccessible: kvmAccessible(),
	}

	m.mu.RLock()
	active := len(m.sandboxes)
	m.mu.RUnlock()

	m.poolMu.Lock()
	ready := len(m.pool)
	m.poolMu.Unlock()

	pool := sandbox.PoolStats{
		Configured: m.cfg.PoolSize,
		Ready:      ready,
		Active:     active,
		Failed:     int(m.failuresTotal.Load()),
		Restarts:   int(m.restartsTotal.Load()),
	}

	egress := sandbox.EgressInfo{}
	if m.cfg.DefaultEgress != nil {
		egress = sandbox.EgressInfo{
			Enabled:   m.cfg.DefaultEgress.Enabled,
			Mode:      m.cfg.DefaultEgress.Mode,
			RuleCount: len(m.cfg.DefaultEgress.Rules),
		}
	}

	snap := sandbox.SnapshotInfo{Enabled: m.cfg.UseSnapshot}
	if m.vmm != nil && m.vmm.snapshot != nil {
		snap.Ready = m.vmm.snapshot.Ready()
	}

	h := sandbox.Health{Runtime: rt, Pool: pool, Egress: egress, Snapshot: snap}
	h.Ready = rt.KernelOK && rt.RootfsOK && rt.FCBinaryOK && rt.KVMAccessible
	return h, nil
}

// fileReadable reports whether path exists and can be opened for reading.
func fileReadable(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// fileExecutable reports whether path is a regular file with any execute bit set.
func fileExecutable(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// kvmAccessible reports whether /dev/kvm exists and can be opened read-write.
func kvmAccessible() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
