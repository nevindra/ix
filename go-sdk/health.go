package ix

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// monitor periodically health-checks every sandbox and restarts or marks
// failed those that exceed the consecutive failure threshold.
func (m *IXManager) monitor(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
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
				if sb.healthCheck(ctx) {
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

				if failCount >= 3 {
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
	remainingTTL := time.Until(old.expiresAt)
	if remainingTTL < 0 {
		remainingTTL = 0
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

	envSlice := m.buildEnvSlice(nil, sessionID)

	handle, err := m.vmm.startVM(ctx, sandboxID, vcpus, memMB, m.cfg.RootfsImage, envSlice)
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
		expiresAt:    now.Add(remainingTTL),
		restartCount: oldRestartCount + 1,
		shellSession: "default",
	}

	m.mu.Lock()
	m.sandboxes[sessionID] = newSb
	m.mu.Unlock()

	m.logger.Info("sandbox restarted",
		"session", sessionID,
		"pid", handle.Process.Pid,
		"restartCount", newSb.restartCount,
	)
}

// markFailed logs a circuit-breaker event and destroys the sandbox.
func (m *IXManager) markFailed(ctx context.Context, sessionID string) {
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
