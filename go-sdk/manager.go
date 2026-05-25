package ix

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nevindra/oasis/sandbox"
)

// ResourceSpec defines per-sandbox resource limits for the VM.
type ResourceSpec struct {
	VCPUs  int   // number of virtual CPUs; 0 uses default (1)
	Memory int64 // bytes; 0 uses default (512 MB)
}

// ManagerConfig configures an IXManager.
type ManagerConfig struct {
	RootfsImage    string        // path to ext4 rootfs image (required)
	KernelPath     string        // path to vmlinux kernel (required)
	FCBinary       string        // path to firecracker binary; empty searches PATH
	MaxConcurrent  int           // 0 = auto-detect from host resources
	DefaultTTL     time.Duration // default: 1 hour
	PerSandbox     ResourceSpec  // per-VM resource limits
	MaxRestarts    int           // default: 3
	Logger         *slog.Logger
	DefaultEgress  *EgressPolicy // optional default egress policy applied to all sandboxes
	PoolSize       int           // number of VMs to keep pre-warmed (default: 0 = disabled)
	PoolMinReady   int           // minimum ready VMs before triggering replenishment (default: 1)
	PoolWorkers    int           // parallel workers for pool fill (default: 3)
	PreWarmKernels []string      // languages to pre-warm in pool entries (e.g., ["python"])
	SnapshotDir    string        // directory for golden snapshot files (default: /tmp/ix-golden-snapshot)
	UseSnapshot    bool          // enable snapshot/restore (default: false)
}

// applyDefaults fills zero-valued fields with sensible defaults.
func (c *ManagerConfig) applyDefaults() {
	if c.DefaultTTL == 0 {
		c.DefaultTTL = time.Hour
	}
	if c.PerSandbox.VCPUs == 0 {
		c.PerSandbox.VCPUs = 1
	}
	if c.PerSandbox.Memory == 0 {
		c.PerSandbox.Memory = 512 << 20 // 512 MB
	}
	if c.MaxRestarts == 0 {
		c.MaxRestarts = 3
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.FCBinary == "" {
		if path, err := exec.LookPath("firecracker"); err == nil {
			c.FCBinary = path
		}
	}
	if c.PoolSize > 0 && c.PoolMinReady == 0 {
		c.PoolMinReady = 1
	}
	if c.PoolSize > 0 && c.PoolWorkers == 0 {
		c.PoolWorkers = 3
	}
	if c.UseSnapshot && c.SnapshotDir == "" {
		c.SnapshotDir = filepath.Join(os.TempDir(), "ix-golden-snapshot")
	}
}

// poolEntry represents a pre-warmed VM ready to be claimed.
type poolEntry struct {
	vmm          *VMMHandle
	createdAt    time.Time
	kernelsReady []string // languages that were successfully pre-warmed
}

// IXManager manages sandbox VM lifecycle using Firecracker MicroVMs.
type IXManager struct {
	cfg       ManagerConfig
	vmm       *firecrackerBackend
	sandboxes map[string]*IXSandbox // keyed by sessionID
	mu        sync.RWMutex
	semaphore chan struct{} // concurrency limiter
	accepting atomic.Bool
	ctx       context.Context
	cancel    context.CancelFunc
	logger    *slog.Logger

	pool     []*poolEntry  // pre-warmed VMs ready to be claimed
	poolMu   sync.Mutex    // guards pool slice
	poolStop chan struct{} // signals pool replenisher to stop
}

// NewManager validates config, sets up concurrency, and starts background
// goroutines (monitor + reaper + pool). Call Shutdown or Close to release resources.
func NewManager(ctx context.Context, cfg ManagerConfig) (*IXManager, error) {
	cfg.applyDefaults()

	if cfg.RootfsImage == "" {
		return nil, fmt.Errorf("RootfsImage is required")
	}
	if _, err := os.Stat(cfg.RootfsImage); err != nil {
		return nil, fmt.Errorf("rootfs image %q: %w", cfg.RootfsImage, err)
	}
	if cfg.KernelPath == "" {
		return nil, fmt.Errorf("KernelPath is required")
	}
	if _, err := os.Stat(cfg.KernelPath); err != nil {
		return nil, fmt.Errorf("kernel path %q: %w", cfg.KernelPath, err)
	}
	if cfg.FCBinary == "" {
		return nil, fmt.Errorf("firecracker not found in PATH (set FCBinary)")
	}

	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = autoDetectMax(cfg.PerSandbox)
	}
	if maxConc < 1 {
		maxConc = 1
	}

	mCtx, cancel := context.WithCancel(ctx)

	m := &IXManager{
		cfg: cfg,
		vmm: &firecrackerBackend{
			fcBinary:    cfg.FCBinary,
			kernelPath:  cfg.KernelPath,
			rootfsImage: cfg.RootfsImage,
			logger:      cfg.Logger,
		},
		sandboxes: make(map[string]*IXSandbox),
		semaphore: make(chan struct{}, maxConc),
		ctx:       mCtx,
		cancel:    cancel,
		logger:    cfg.Logger,
		poolStop:  make(chan struct{}),
	}
	m.accepting.Store(true)

	if cfg.UseSnapshot {
		vcpus := cfg.PerSandbox.VCPUs
		if vcpus < 1 {
			vcpus = 1
		}
		memMB := cfg.PerSandbox.Memory >> 20
		if memMB < 128 {
			memMB = 128
		}
		m.vmm.snapshot = NewSnapshotManager(
			cfg.SnapshotDir,
			cfg.RootfsImage,
			m.vmm,
			vcpus,
			memMB,
			cfg.Logger,
		)
		go func() {
			if err := m.vmm.snapshot.CreateGolden(m.ctx); err != nil {
				m.logger.Error("golden snapshot failed, falling back to cold boot", "error", err)
			}
		}()
	}

	// Recover / clean up orphaned socket dirs from a previous run.
	if err := m.recover(mCtx); err != nil {
		m.logger.Warn("recover failed", "error", err)
	}

	go m.monitor(mCtx)
	go m.reaper(mCtx)

	if cfg.PoolSize > 0 {
		go m.poolReplenisher(mCtx)
	}

	return m, nil
}

// Create provisions a new sandbox VM.
func (m *IXManager) Create(ctx context.Context, opts sandbox.CreateOpts) (sandbox.Sandbox, error) {
	if !m.accepting.Load() {
		return nil, sandbox.ErrShuttingDown
	}

	resolved := m.resolveOpts(opts)

	// Try to grab a pre-warmed VM from the pool.
	entry := m.grabFromPool()
	if entry != nil {
		// Fast path: VM already running and ready.
		now := time.Now()
		ttl := resolved.TTL

		poolClient := &http.Client{
			Transport: vsockTransport(entry.vmm.VsockPath),
			Timeout:   5 * time.Minute,
		}
		sb := &IXSandbox{
			id:           resolved.SessionID,
			vmm:          entry.vmm,
			baseURL:      "http://localhost",
			client:       newClient("http://localhost", poolClient),
			createdAt:    now,
			expiresAt:    now.Add(ttl),
			shellSession: "default",
		}

		m.mu.Lock()
		m.sandboxes[resolved.SessionID] = sb
		m.mu.Unlock()

		m.logger.Info("sandbox created from pool",
			"session", resolved.SessionID,
			"pid", entry.vmm.Process.Pid,
			"cid", entry.vmm.CID,
			"ttl", ttl,
			"kernelsReady", entry.kernelsReady,
		)

		// Trigger async replenishment.
		go m.replenishPool()

		return sb, nil
	}

	// Slow path: create on demand.

	// Acquire concurrency slot.
	if err := acquireSlot(ctx, m.semaphore, func() bool { return m.evictIdle(ctx) }, 30*time.Second); err != nil {
		return nil, err
	}

	sandboxID := uuid.NewString()[:12]
	now := time.Now()
	ttl := resolved.TTL

	vcpus := m.cfg.PerSandbox.VCPUs
	if vcpus < 1 {
		vcpus = 1
	}
	memMB := m.cfg.PerSandbox.Memory >> 20
	if memMB < 128 {
		memMB = 128
	}

	// Build env vars.
	envSlice := m.buildEnvSlice(resolved.Env)

	// Launch Firecracker VM.
	handle, err := m.vmm.startVM(ctx, sandboxID, vcpus, memMB, envSlice)
	if err != nil {
		m.releaseSlot()
		return nil, fmt.Errorf("start VM: %w", err)
	}

	if m.vmm.snapshot == nil || !m.vmm.snapshot.Ready() {
		if err := m.vmm.waitReady(ctx, handle); err != nil {
			m.vmm.cleanup(handle)
			m.releaseSlot()
			return nil, fmt.Errorf("wait ready: %w", err)
		}
	}

	transport := vsockTransport(handle.VsockPath)
	httpClient := &http.Client{Transport: transport, Timeout: 2 * time.Minute}
	sb := &IXSandbox{
		id:           resolved.SessionID,
		vmm:          handle,
		baseURL:      "http://localhost",
		client:       newClient("http://localhost", httpClient),
		createdAt:    now,
		expiresAt:    now.Add(ttl),
		shellSession: "default",
	}

	m.mu.Lock()
	m.sandboxes[resolved.SessionID] = sb
	m.mu.Unlock()

	m.logger.Info("sandbox created",
		"session", resolved.SessionID,
		"pid", handle.Process.Pid,
		"cid", handle.CID,
		"ttl", ttl,
	)

	return sb, nil
}

// Get retrieves an existing sandbox by session ID.
func (m *IXManager) Get(sessionID string) (sandbox.Sandbox, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil, sandbox.ErrNotFound
	}
	return sb, nil
}

// Shutdown stops accepting new sandboxes and waits for in-flight work to drain.
func (m *IXManager) Shutdown(ctx context.Context) error {
	m.accepting.Store(false)

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.mu.RLock()
			active := len(m.sandboxes)
			m.mu.RUnlock()
			if active == 0 {
				m.cancel()
				return nil
			}
		}
	}
}

// Close force-destroys all managed sandboxes and pool entries.
func (m *IXManager) Close() error {
	m.accepting.Store(false)

	// Signal pool replenisher to stop.
	select {
	case <-m.poolStop:
	default:
		close(m.poolStop)
	}

	m.cancel()

	m.mu.Lock()
	sessions := make([]string, 0, len(m.sandboxes))
	for sid := range m.sandboxes {
		sessions = append(sessions, sid)
	}
	m.mu.Unlock()

	var firstErr error
	for _, sid := range sessions {
		if err := m.destroy(context.Background(), sid); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Destroy pooled VMs.
	m.poolMu.Lock()
	poolEntries := m.pool
	m.pool = nil
	m.poolMu.Unlock()

	for _, entry := range poolEntries {
		m.vmm.cleanup(entry.vmm)
		m.releaseSlot()
	}

	return firstErr
}

// --- Helpers ---

// resolveOpts merges user-provided CreateOpts with manager defaults.
func (m *IXManager) resolveOpts(opts sandbox.CreateOpts) sandbox.CreateOpts {
	if opts.TTL == 0 {
		opts.TTL = m.cfg.DefaultTTL
	}
	if opts.Resources.CPU == 0 {
		opts.Resources.CPU = m.cfg.PerSandbox.VCPUs
	}
	if opts.Resources.Memory == 0 {
		opts.Resources.Memory = m.cfg.PerSandbox.Memory
	}
	return opts
}

// Destroy stops and removes a sandbox by session ID.
func (m *IXManager) Destroy(ctx context.Context, sessionID string) error {
	return m.destroy(ctx, sessionID)
}

// destroy removes a sandbox from the map, kills its VM process,
// removes the socket dir, and releases the concurrency slot.
func (m *IXManager) destroy(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[sessionID]
	if ok {
		delete(m.sandboxes, sessionID)
	}
	m.mu.Unlock()
	if !ok {
		return sandbox.ErrNotFound
	}

	sb.Close()
	m.vmm.cleanup(sb.vmm)
	m.releaseSlot()
	return nil
}

// buildEnvSlice constructs the env var slice to pass to ix-vmm.
func (m *IXManager) buildEnvSlice(userEnv map[string]string) []string {
	var envSlice []string
	for k, v := range userEnv {
		envSlice = append(envSlice, k+"="+v)
	}

	if m.cfg.DefaultEgress != nil && m.cfg.DefaultEgress.Enabled {
		envSlice = append(envSlice, "IX_EGRESS_ENABLED=true")
		envSlice = append(envSlice, "IX_EGRESS_MODE="+m.cfg.DefaultEgress.Mode)
		if len(m.cfg.DefaultEgress.Rules) > 0 {
			envSlice = append(envSlice, "IX_EGRESS_RULES="+strings.Join(m.cfg.DefaultEgress.Rules, ","))
		}
	}
	return envSlice
}

// releaseSlot returns a concurrency slot to the semaphore.
func (m *IXManager) releaseSlot() {
	select {
	case <-m.semaphore:
	default:
	}
}

// evictIdle destroys the oldest expired sandbox to free a slot.
// Returns true if a sandbox was evicted.
func (m *IXManager) evictIdle(ctx context.Context) bool {
	m.mu.RLock()
	var oldest *IXSandbox
	var oldestSID string
	now := time.Now()
	for sid, sb := range m.sandboxes {
		if now.After(sb.expiresAt) {
			if oldest == nil || sb.createdAt.Before(oldest.createdAt) {
				oldest = sb
				oldestSID = sid
			}
		}
	}
	m.mu.RUnlock()

	if oldest == nil {
		return false
	}

	if err := m.destroy(ctx, oldestSID); err != nil {
		m.logger.Warn("evict idle failed", "session", oldestSID, "error", err)
		return false
	}
	m.logger.Info("evicted idle sandbox", "session", oldestSID)
	return true
}

// acquireSlot attempts to acquire a concurrency slot. It tries the fast path
// first, then attempts eviction, then queues with a timeout.
func acquireSlot(ctx context.Context, sem chan struct{}, tryEvict func() bool, timeout time.Duration) error {
	// Fast path: slot available.
	select {
	case sem <- struct{}{}:
		return nil
	default:
	}

	// Try eviction.
	if tryEvict() {
		select {
		case sem <- struct{}{}:
			return nil
		default:
		}
	}

	// Queue with timeout.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		return nil
	case <-timer.C:
		return sandbox.ErrCapacityFull
	case <-ctx.Done():
		return ctx.Err()
	}
}

// autoDetectMax calculates maximum concurrent sandboxes from host resources.
func autoDetectMax(perSandbox ResourceSpec) int {
	cpus := runtime.NumCPU()
	vcpus := perSandbox.VCPUs
	if vcpus < 1 {
		vcpus = 1
	}
	cpuMax := cpus / vcpus

	memBytes := perSandbox.Memory
	if memBytes < 1 {
		memBytes = 512 << 20
	}
	memMax := int(hostMemoryBytes() / memBytes)

	result := min(cpuMax, memMax)
	if result < 1 {
		result = 1
	}
	return result
}

// --- Pool ---

// poolReplenisher runs in the background and keeps the pool filled to PoolSize.
func (m *IXManager) poolReplenisher(ctx context.Context) {
	// Initial fill.
	m.fillPool(ctx)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.poolStop:
			return
		case <-ticker.C:
			m.poolMu.Lock()
			currentSize := len(m.pool)
			m.poolMu.Unlock()

			if currentSize < m.cfg.PoolMinReady {
				m.fillPool(ctx)
			}
		}
	}
}

// fillPool creates pool entries in parallel until the pool reaches PoolSize.
func (m *IXManager) fillPool(ctx context.Context) {
	m.poolMu.Lock()
	needed := m.cfg.PoolSize - len(m.pool)
	m.poolMu.Unlock()

	if needed <= 0 {
		return
	}

	workers := min(needed, m.cfg.PoolWorkers)
	if workers < 1 {
		workers = 1
	}

	type result struct {
		entry *poolEntry
		err   error
	}
	ch := make(chan result, workers)

	for i := 0; i < workers; i++ {
		go func() {
			entry, err := m.createPoolEntry(ctx)
			ch <- result{entry, err}
		}()
	}

	for i := 0; i < workers; i++ {
		select {
		case <-ctx.Done():
			return
		case <-m.poolStop:
			return
		case r := <-ch:
			if r.err != nil {
				m.logger.Warn("pool fill failed", "error", r.err)
				continue
			}
			m.poolMu.Lock()
			m.pool = append(m.pool, r.entry)
			poolSize := len(m.pool)
			m.poolMu.Unlock()
			m.logger.Info("pool entry created", "poolSize", poolSize, "target", m.cfg.PoolSize)
		}
	}
}

// createPoolEntry creates a single pre-warmed VM for the pool.
func (m *IXManager) createPoolEntry(ctx context.Context) (*poolEntry, error) {
	// Acquire concurrency slot — pool entries count toward MaxConcurrent.
	if err := acquireSlot(ctx, m.semaphore, func() bool { return false }, 30*time.Second); err != nil {
		return nil, fmt.Errorf("pool acquire slot: %w", err)
	}

	sandboxID := uuid.NewString()[:12]

	vcpus := m.cfg.PerSandbox.VCPUs
	if vcpus < 1 {
		vcpus = 1
	}
	memMB := m.cfg.PerSandbox.Memory >> 20
	if memMB < 128 {
		memMB = 128
	}

	envSlice := m.buildEnvSlice(nil)

	handle, err := m.vmm.startVM(ctx, sandboxID, vcpus, memMB, envSlice)
	if err != nil {
		m.releaseSlot()
		return nil, fmt.Errorf("pool start VM: %w", err)
	}

	if m.vmm.snapshot == nil || !m.vmm.snapshot.Ready() {
		if err := m.vmm.waitReady(ctx, handle); err != nil {
			m.vmm.cleanup(handle)
			m.releaseSlot()
			return nil, fmt.Errorf("pool wait ready: %w", err)
		}
	}

	// Pre-warm kernels if configured. Best-effort: a warmup failure does not
	// discard the pool entry; it will just be slower on first execution.
	var kernelsReady []string
	if len(m.cfg.PreWarmKernels) > 0 {
		warmupTransport := vsockTransport(handle.VsockPath)
		warmupHTTP := &http.Client{Transport: warmupTransport, Timeout: 2 * time.Minute}
		warmupClient := newClient("http://localhost", warmupHTTP)

		for _, lang := range m.cfg.PreWarmKernels {
			if err := m.warmKernel(ctx, warmupClient, lang); err != nil {
				m.logger.Warn("pool kernel warmup failed", "language", lang, "error", err)
				continue
			}
			kernelsReady = append(kernelsReady, lang)
		}
	}

	return &poolEntry{
		vmm:          handle,
		createdAt:    time.Now(),
		kernelsReady: kernelsReady,
	}, nil
}

// warmKernel sends a minimal code-execution request to the daemon so the
// language kernel is fully booted before the pool entry is claimed. The SSE
// stream is drained until a "complete" or "error" event is received.
func (m *IXManager) warmKernel(ctx context.Context, client *ixClient, language string) error {
	reader, err := client.postSSE(ctx, "/v1/code/execute", map[string]any{
		"language": language,
		"code":     "", // empty code — just boots the kernel
		"timeout":  60,
	})
	if err != nil {
		return err
	}
	defer reader.Close()

	for reader.Next() {
		ev := reader.Event()
		if ev == "complete" || ev == "error" {
			break
		}
	}
	return reader.Err()
}

// grabFromPool removes and returns the oldest pool entry. Returns nil if the pool is empty.
func (m *IXManager) grabFromPool() *poolEntry {
	m.poolMu.Lock()
	defer m.poolMu.Unlock()

	if len(m.pool) == 0 {
		return nil
	}
	entry := m.pool[0]
	m.pool = m.pool[1:]
	return entry
}

// replenishPool creates a single pool entry if the pool is below target size.
// Called asynchronously after a pool entry is claimed.
func (m *IXManager) replenishPool() {
	select {
	case <-m.poolStop:
		return
	default:
	}

	m.poolMu.Lock()
	currentSize := len(m.pool)
	m.poolMu.Unlock()

	if currentSize >= m.cfg.PoolSize {
		return
	}

	entry, err := m.createPoolEntry(m.ctx)
	if err != nil {
		m.logger.Warn("pool replenish failed", "error", err)
		return
	}

	m.poolMu.Lock()
	m.pool = append(m.pool, entry)
	poolSize := len(m.pool)
	m.poolMu.Unlock()

	m.logger.Info("pool replenished", "poolSize", poolSize, "target", m.cfg.PoolSize)
}

// Compile-time check that IXManager implements sandbox.Manager.
var _ sandbox.Manager = (*IXManager)(nil)
