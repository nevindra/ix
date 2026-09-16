package ix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// ErrVolumeBusy is returned when a Volume is already attached to a live sandbox.
var ErrVolumeBusy = errors.New("volume is attached to another sandbox")

var volumeKeyRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// volumeEnvKey is how the guest learns where to mount /dev/vdc (ix-stage0
// reads it from the kernel cmdline as ix.env.IX_VOLUME_PATH).
const volumeEnvKey = "IX_VOLUME_PATH"

// VolumeInfo describes one Volume file on the host.
type VolumeInfo struct {
	Key       string
	Size      int64 // apparent size (bytes)
	Allocated int64 // bytes actually allocated on disk
	ModTime   time.Time
	Attached  bool
}

func (m *IXManager) volumeDir() string {
	return filepath.Join(m.cfg.RunDir, "volumes")
}

func (m *IXManager) volumeFile(key string) string {
	return filepath.Join(m.volumeDir(), key+".ext4")
}

func validateVolumeKey(key string) error {
	if !volumeKeyRe.MatchString(key) {
		return fmt.Errorf("invalid volume key %q: must match %s", key, volumeKeyRe)
	}
	return nil
}

// validateVolumePath rejects a guest mount point Oasis's mount layer would
// walk: anything at or under /workspace would push the whole checkout into
// the files table.
func validateVolumePath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("VolumePath %q must be absolute", p)
	}
	clean := filepath.Clean(p)
	if clean == "/" || clean == "/workspace" || strings.HasPrefix(clean, "/workspace/") {
		return fmt.Errorf("VolumePath %q must not be / or under /workspace", p)
	}
	return nil
}

// ensureVolume creates the Volume file if missing: sparse, ext4, journal ON
// (unlike the scratch template — this disk survives a power-cut destroy).
func ensureVolume(path string, sizeMB int64) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return mkfsSparse(path, sizeMB, true)
}

// attachVolume claims key for sessionID. Must be called before any VM starts.
func (m *IXManager) attachVolume(key, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, busy := m.volumes[key]; busy {
		return ErrVolumeBusy
	}
	if m.volumes == nil {
		m.volumes = make(map[string]string)
	}
	m.volumes[key] = sessionID
	return nil
}

func (m *IXManager) releaseVolume(key string) {
	if key == "" {
		return
	}
	m.mu.Lock()
	delete(m.volumes, key)
	m.mu.Unlock()
}

func (m *IXManager) volumeAttached(key string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.volumes[key]
	return ok
}

// CreateWithVolume is Create with a host-persistent Volume attached at
// ManagerConfig.VolumePath. The sandbox always cold-boots: a snapshot-restored
// VM cannot take a drive the golden snapshot did not have. The Volume file is
// created empty on first use and kept on destroy (ADR 0003).
func (m *IXManager) CreateWithVolume(ctx context.Context, opts sandbox.CreateOpts, key string) (sandbox.Sandbox, error) {
	if !m.accepting.Load() {
		return nil, sandbox.ErrShuttingDown
	}
	if err := validateVolumeKey(key); err != nil {
		return nil, err
	}
	resolved := m.resolveOpts(opts)
	if err := m.attachVolume(key, resolved.SessionID); err != nil {
		return nil, err
	}
	path := m.volumeFile(key)
	if err := ensureVolume(path, m.cfg.VolumeSizeMB); err != nil {
		m.releaseVolume(key)
		return nil, fmt.Errorf("ensure volume: %w", err)
	}
	sb, err := m.createCold(ctx, resolved, key)
	if err != nil {
		m.releaseVolume(key)
		return nil, err
	}
	return sb, nil
}

// volumeDrive is the extra drive spec + env for a Volume-bearing boot.
func (m *IXManager) volumeDrive(key string) ([]driveSpec, map[string]string) {
	if key == "" {
		return nil, nil
	}
	return []driveSpec{buildDriveSpec("volume", m.volumeFile(key), false)},
		map[string]string{volumeEnvKey: m.cfg.VolumePath}
}

// syncVolume asks the guest to flush before the VM is killed. Best effort:
// the journal covers the crash case, this only closes the common one.
func (m *IXManager) syncVolume(sb *IXSandbox) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := sb.ShellOneShot(ctx, sandbox.ShellRequest{Command: "sync", Timeout: 2}); err != nil {
		m.logger.Warn("volume: sync before destroy failed", "volume", sb.volume, "error", err)
	}
}

// DeleteVolume removes a Volume file. ErrVolumeBusy if attached; an unknown
// key is not an error.
func (m *IXManager) DeleteVolume(key string) error {
	if err := validateVolumeKey(key); err != nil {
		return err
	}
	if m.volumeAttached(key) {
		return ErrVolumeBusy
	}
	if err := os.Remove(m.volumeFile(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ListVolumes returns every Volume file on the host, oldest first.
func (m *IXManager) ListVolumes() ([]VolumeInfo, error) {
	entries, err := os.ReadDir(m.volumeDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []VolumeInfo
	for _, e := range entries {
		key, ok := strings.CutSuffix(e.Name(), ".ext4")
		if !ok || e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		var alloc int64
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			alloc = st.Blocks * 512
		}
		out = append(out, VolumeInfo{
			Key:       key,
			Size:      fi.Size(),
			Allocated: alloc,
			ModTime:   fi.ModTime(),
			Attached:  m.volumeAttached(key),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.Before(out[j].ModTime) })
	return out, nil
}

// evictOldestVolume deletes the least recently modified unattached Volume.
// Returns false when there is nothing to evict.
func (m *IXManager) evictOldestVolume() bool {
	vols, err := m.ListVolumes()
	if err != nil {
		m.logger.Warn("reaper: list volumes failed", "error", err)
		return false
	}
	for _, v := range vols {
		if v.Attached {
			continue
		}
		if err := m.DeleteVolume(v.Key); err != nil {
			m.logger.Warn("reaper: evict volume failed", "volume", v.Key, "error", err)
			continue
		}
		m.logger.Info("reaper: low disk space, evicted volume", "volume", v.Key, "freedBytes", v.Allocated)
		return true
	}
	return false
}
