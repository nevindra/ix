package ix

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/container"
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

// restart destroys the old sandbox container and replaces it with a fresh one,
// preserving the session ID and incrementing the restart counter.
func (m *IXManager) restart(ctx context.Context, sessionID string) {
	m.mu.Lock()
	old, ok := m.sandboxes[sessionID]
	if !ok {
		m.mu.Unlock()
		return
	}
	oldContainerID := old.containerID
	oldNetworkID := old.networkID
	oldRestartCount := old.restartCount
	remainingTTL := time.Until(old.expiresAt)
	if remainingTTL < 0 {
		remainingTTL = 0
	}
	m.mu.Unlock()

	// Destroy old container + network + socket dir.
	old.Close()
	_ = m.destroyContainer(ctx, oldContainerID, oldNetworkID)
	if old.socketDir != "" {
		_ = os.RemoveAll(old.socketDir)
	}

	// Create replacement.
	sandboxID := uuid.NewString()[:12]
	now := time.Now()

	networkID, networkName, err := m.allocateNetwork(ctx, sandboxID, sessionID)
	if err != nil {
		m.logger.Error("restart: allocate network failed", "session", sessionID, "error", err)
		return
	}

	socketDir := filepath.Join(os.TempDir(), "ix-"+sandboxID)
	if err := os.MkdirAll(socketDir, 0o777); err != nil {
		m.cleanupNetwork(ctx, networkID)
		m.logger.Error("restart: create socket dir failed", "session", sessionID, "error", err)
		return
	}
	_ = os.Chmod(socketDir, 0o777)
	socketPath := filepath.Join(socketDir, "daemon.sock")

	var pidsLimit int64 = 256
	hostCfg := &container.HostConfig{
		Runtime: m.cfg.Runtime,
		Resources: container.Resources{
			Memory:    m.cfg.PerSandbox.Memory,
			CPUQuota:  int64(m.cfg.PerSandbox.CPU) * 100000,
			CPUPeriod: 100000,
			PidsLimit: &pidsLimit,
		},
		NetworkMode: container.NetworkMode(networkName),
		Binds:       []string{socketDir + ":/run/ix"},
		RestartPolicy: container.RestartPolicy{
			Name: container.RestartPolicyDisabled,
		},
		SecurityOpt: []string{"no-new-privileges:true"},
		CapDrop:     []string{"ALL"},
		CapAdd:      []string{"CHOWN", "SETUID", "SETGID", "KILL", "NET_BIND_SERVICE"},
	}

	containerCfg := &container.Config{
		Image: m.cfg.Image,
		Env:   []string{"IX_SOCKET=/run/ix/daemon.sock"},
		Labels: map[string]string{
			"oasis.sandbox": "true",
			"oasis.session": sessionID,
			"oasis.created": now.Format(time.RFC3339),
			"oasis.expires": now.Add(remainingTTL).Format(time.RFC3339),
		},
	}

	resp, err := m.docker.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "sandbox-"+sandboxID)
	if err != nil {
		m.cleanupNetwork(ctx, networkID)
		_ = os.RemoveAll(socketDir)
		m.logger.Error("restart: create container failed", "session", sessionID, "error", err)
		return
	}

	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		m.cleanupNetwork(ctx, networkID)
		_ = os.RemoveAll(socketDir)
		m.logger.Error("restart: start container failed", "session", sessionID, "error", err)
		return
	}

	socketTransport := unixSocketTransport(socketPath)
	if err := m.waitReady(ctx, "http://localhost", socketTransport, socketPath); err != nil {
		_ = m.destroyContainer(ctx, resp.ID, networkID)
		_ = os.RemoveAll(socketDir)
		m.logger.Error("restart: wait ready failed", "session", sessionID, "error", err)
		return
	}

	httpClient := &http.Client{Transport: socketTransport, Timeout: 2 * time.Minute}
	newSb := &IXSandbox{
		id:           sessionID,
		containerID:  resp.ID,
		baseURL:      "http://localhost",
		client:       newClient("http://localhost", httpClient),
		networkID:    networkID,
		socketDir:    socketDir,
		createdAt:    now,
		expiresAt:    now.Add(remainingTTL),
		restartCount: oldRestartCount + 1,
	}

	m.mu.Lock()
	m.sandboxes[sessionID] = newSb
	m.mu.Unlock()

	m.logger.Info("sandbox restarted",
		"session", sessionID,
		"container", resp.ID[:12],
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
