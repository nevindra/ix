package ix

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// reaper periodically destroys expired sandboxes and evicts the oldest
// sandbox when host disk space on the rootfs path falls below 5 GB.
func (m *IXManager) reaper(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapExpired(ctx)
			m.reapDisk(ctx)
		}
	}
}

// reapExpired destroys sandboxes whose TTL has elapsed.
func (m *IXManager) reapExpired(ctx context.Context) {
	m.mu.RLock()
	snapshot := maps.Clone(m.sandboxes)
	m.mu.RUnlock()

	now := time.Now()
	for sessionID, sb := range snapshot {
		if now.After(sb.expiresAt) {
			m.logger.Info("reaper: TTL expired, destroying", "session", sessionID)
			if err := m.destroy(ctx, sessionID); err != nil {
				m.logger.Warn("reaper: destroy failed", "session", sessionID, "error", err)
			}
		}
	}
}

// reapDisk evicts the oldest sandbox when free disk space on the rootfs image
// path falls below 5 GB.
func (m *IXManager) reapDisk(ctx context.Context) {
	if diskFreeGB(m.cfg.RootfsImage) >= 5 {
		return
	}

	m.mu.RLock()
	var oldestSID string
	var oldestTime time.Time
	for sid, sb := range m.sandboxes {
		if oldestSID == "" || sb.createdAt.Before(oldestTime) {
			oldestSID = sid
			oldestTime = sb.createdAt
		}
	}
	m.mu.RUnlock()

	if oldestSID == "" {
		return
	}

	m.logger.Warn("reaper: low disk space, evicting oldest sandbox",
		"session", oldestSID,
		"freeGB", diskFreeGB(m.cfg.RootfsImage),
	)
	if err := m.destroy(ctx, oldestSID); err != nil {
		m.logger.Warn("reaper: evict failed", "session", oldestSID, "error", err)
	}
}

// recover scans /tmp for orphaned ix-* socket directories left by a previous
// manager instance and removes them. VM process recovery is not supported in
// Phase 1 — we cannot reliably identify which processes belong to us across
// restarts without a process registry.
func (m *IXManager) recover(ctx context.Context) error {
	m.logger.Info("recover: process-based recovery not supported in Phase 1, cleaning up orphaned socket dirs")

	// Collect dirs that must not be removed: active sandboxes + snapshot dir.
	active := make(map[string]bool)
	m.mu.RLock()
	for _, sb := range m.sandboxes {
		if sb.vmm != nil && sb.vmm.SocketDir != "" {
			active[sb.vmm.SocketDir] = true
		}
	}
	m.mu.RUnlock()
	if m.cfg.SnapshotDir != "" {
		active[m.cfg.SnapshotDir] = true
	}

	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil // non-fatal
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !strings.HasPrefix(entry.Name(), "ix-") {
			continue
		}
		fullPath := filepath.Join(tmpDir, entry.Name())
		if active[fullPath] {
			continue
		}
		m.logger.Info("recover: removing orphaned socket dir", "path", fullPath)
		if err := os.RemoveAll(fullPath); err != nil {
			m.logger.Warn("recover: remove failed", "path", fullPath, "error", err)
		}
	}

	return nil
}

// diskFreeGB returns the free disk space in gigabytes at the given path.
// Returns 999 (assume plenty) if the check fails.
func diskFreeGB(path string) int {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 999 // assume plenty if can't check
	}
	return int((stat.Bavail * uint64(stat.Bsize)) / (1 << 30))
}
