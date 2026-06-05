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
	Memory int64 // bytes; 0 uses default (256 MB)
}

// ManagerConfig configures an IXManager.
type ManagerConfig struct {
	RootfsImage       string        // path to ext4 rootfs image (required)
	KernelPath        string        // path to vmlinux kernel (required)
	FCBinary          string        // path to firecracker binary; empty searches PATH
	MaxConcurrent     int           // 0 = auto-detect from host resources
	DefaultTTL        time.Duration // default: 1 hour
	PerSandbox        ResourceSpec  // per-VM resource limits
	MaxRestarts       int           // default: 3
	Logger            *slog.Logger
	DefaultEgress     *EgressPolicy // optional default egress policy applied to all sandboxes
	PoolSize          int           // number of VMs to keep pre-warmed (default: 0 = disabled)
	PoolMinReady      int           // minimum ready VMs before triggering replenishment (default: 1)
	PoolWorkers       int           // parallel workers for pool fill (default: 3)
	PreWarmKernels    []string      // languages to pre-warm in pool entries (e.g., ["python"])
	SnapshotDir       string        // directory for golden snapshot files (default: /tmp/ix-golden-snapshot)
	UseSnapshot       bool          // enable snapshot/restore (default: false)
	BrowserGatewayURL string        // when set, per-chat VMs use the shared browser gateway (IX_BROWSER_MODE=remote=<url>)

	// Browser-tier (shared browser VM) settings. Active when BrowserMode=="remote".
	BrowserMode       string // "" / "local" (no tier) or "remote" (boot a shared browser-tier VM)
	BrowserVMImage    string // rootfs path for the browser-tier VM (browser-vm tier)
	BrowserVMMemoryMB int64  // browser-tier VM memory; default 4096
	BrowserStateImage string // optional ext4 state disk attached to the browser-tier VM; empty = ephemeral
	GatewayListenAddr string // host addr the gateway binds, reachable from guests via their per-VM TAP route; default "169.254.0.1:9100"
	GatewayToken      string // optional bearer token forwarded to pinchtab and required from daemons
	// BrowserTrustedResolveCIDRs opts specific private/internal CIDRs back in
	// past pinchtab's SSRF navigation guard (security.trustedResolveCIDRs in
	// the guest config). Default empty = guard fully on. Benchmark/test use:
	// the browser benchmarks serve their hermetic page on the gateway's
	// link-local IP, which the guard would otherwise 403.
	BrowserTrustedResolveCIDRs []string

	// Networking (per-VM TAP + host NAT).
	EgressInterface   string // optional: restrict NAT MASQUERADE to this uplink; empty = masquerade on any non-TAP egress (robust default for multi-homed/VPN hosts)
	NetworkCIDR       string // base address space; default "172.16.0.0/16"
	DisableNetworking bool   // skip TAP setup (vsock-only VMs)

	// Per-VM disk isolation. The rootfs is attached read-only and shared;
	// each VM writes to a private sparse scratch disk (overlay upper layer).
	RunDir        string // base dir for per-VM runtime dirs (sockets + scratch disks); default: <dir of RootfsImage>/run. Must NOT be on tmpfs.
	ScratchSizeMB int64  // per-VM scratch disk size in MB (sparse; allocates only what is written); default 10240
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
		c.PerSandbox.Memory = 256 << 20 // 256 MB (base image; was 512 for the heavy image)
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
	if c.NetworkCIDR == "" {
		c.NetworkCIDR = "172.16.0.0/16"
	}
	if c.BrowserMode == "remote" {
		if c.BrowserVMMemoryMB == 0 {
			c.BrowserVMMemoryMB = 4096
		}
		if c.GatewayListenAddr == "" {
			c.GatewayListenAddr = defaultGatewayListenAddr
		}
		if c.GatewayToken == "" {
			// pinchtab requires a non-empty token. Default to the shared internal
			// token the guest's browser-vm-init also defaults to, so the host
			// Gateway and guest pinchtab agree with zero operator config. The
			// Gateway is link-local and pinchtab is guest-local (vsock-only), so
			// this is not externally reachable; override via GatewayToken for prod.
			c.GatewayToken = defaultGatewayToken
		}
	}
	if c.ScratchSizeMB == 0 {
		c.ScratchSizeMB = 10240
	}
	if c.RunDir == "" && c.RootfsImage != "" {
		// Default next to the rootfs image: that path is operator-managed real
		// disk. os.TempDir() would risk tmpfs — sparse scratch growth would
		// silently consume host RAM.
		c.RunDir = filepath.Join(filepath.Dir(c.RootfsImage), "run")
	}
}

// MaxInflightOrDefault returns the per-VM browser in-flight cap (heuristic from
// the browser-tier memory: ~1 Chrome per 250 MB), min 1.
func (c ManagerConfig) MaxInflightOrDefault() int {
	n := int(c.BrowserVMMemoryMB / 250)
	if n < 1 {
		n = 1
	}
	return n
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

	// poolHits counts Create() calls served from the pre-warmed pool.
	// Benchmarks use it to fail loudly instead of silently timing a cold boot.
	poolHits atomic.Int64

	tier *browserTier // shared browser-tier VM + gateway (nil when BrowserMode != "remote")
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

	if err := os.MkdirAll(cfg.RunDir, 0o700); err != nil {
		return nil, fmt.Errorf("create run dir %q: %w", cfg.RunDir, err)
	}
	scratchTemplate := filepath.Join(cfg.RunDir, "scratch-template.ext4")
	if err := ensureScratchTemplate(scratchTemplate, cfg.ScratchSizeMB); err != nil {
		return nil, fmt.Errorf("scratch template: %w", err)
	}

	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = autoDetectMax(cfg.PerSandbox)
	}
	if maxConc < 1 {
		maxConc = 1
	}

	// Custom CIDRs are not yet supported (the /30 derivation assumes the /16 base).
	if cfg.NetworkCIDR != "172.16.0.0/16" {
		return nil, fmt.Errorf("custom NetworkCIDR %q not yet supported; leave empty for the default", cfg.NetworkCIDR)
	}

	mCtx, cancel := context.WithCancel(ctx)

	m := &IXManager{
		cfg: cfg,
		vmm: &firecrackerBackend{
			fcBinary:        cfg.FCBinary,
			kernelPath:      cfg.KernelPath,
			rootfsImage:     cfg.RootfsImage,
			logger:          cfg.Logger,
			tapAlloc:        newTapAllocator(0),
			disableNet:      cfg.DisableNetworking,
			runDir:          cfg.RunDir,
			scratchTemplate: scratchTemplate,
		},
		sandboxes: make(map[string]*IXSandbox),
		semaphore: make(chan struct{}, maxConc),
		ctx:       mCtx,
		cancel:    cancel,
		logger:    cfg.Logger,
		poolStop:  make(chan struct{}),
	}
	m.accepting.Store(true)

	// Clean up orphaned socket dirs from a previous manager instance. MUST run
	// before we start any of our own VMs (browser tier, pool): recover() removes
	// every /tmp/ix-* dir not in m.sandboxes, so running it after startBrowserTier
	// would delete the just-booted tier's vsock socket (it lives in m.tier).
	if err := m.recover(mCtx); err != nil {
		m.logger.Warn("recover failed", "error", err)
	}

	if !cfg.DisableNetworking {
		// Empty EgressInterface masquerades on any non-TAP interface (see
		// nftRuleset) — no uplink detection: pinning NAT to the default-route
		// interface breaks on multi-homed hosts (split-tunnel VPNs reroute
		// destinations through a tun the pinned rule never matches).
		if err := ensureHostNAT(mCtx, cfg.NetworkCIDR, cfg.EgressInterface); err != nil {
			cancel()
			return nil, fmt.Errorf("ensure host NAT: %w", err)
		}
		// Survive a DROP forward policy (e.g. Docker's): an nft accept cannot
		// override an iptables FORWARD DROP, so add an explicit accept there.
		if err := ensureForwardAccept(mCtx, cfg.Logger); err != nil {
			cancel()
			return nil, fmt.Errorf("ensure forward accept: %w", err)
		}
		if cfg.BrowserMode == "remote" {
			gwIP, err := parseGatewayIP(cfg.GatewayListenAddr)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("parse gateway addr: %w", err)
			}
			if err := ensureGatewayAddr(mCtx, gwIP); err != nil {
				cancel()
				return nil, fmt.Errorf("ensure gateway addr: %w", err)
			}
		}
	}

	if cfg.BrowserMode == "remote" {
		tier, gwURL, err := startBrowserTier(mCtx, m.vmm, cfg)
		if err != nil {
			// cancel() is safe here: monitor/reaper/pool goroutines start below,
			// so none are running yet on this error path.
			cancel()
			return nil, fmt.Errorf("start browser tier: %w", err)
		}
		m.tier = tier
		// Inject the gateway URL into m.cfg so buildEnvSlice points per-chat VMs
		// at the tier (the URL isn't known until the tier binds, so it can't be
		// set in applyDefaults).
		m.cfg.BrowserGatewayURL = gwURL
	}

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

	// Pool entries use the default rootfs — only use pool when the image matches.
	var entry *poolEntry
	if resolved.Image == m.cfg.RootfsImage {
		entry = m.grabFromPool()
	}
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

		m.poolHits.Add(1)

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

	vcpus := resolved.Resources.CPU
	if vcpus < 1 {
		vcpus = 1
	}
	memMB := resolved.Resources.Memory >> 20
	if memMB < 128 {
		memMB = 128
	}

	// Build env vars.
	envSlice := m.buildEnvSlice(resolved.Env, resolved.SessionID, resolved.Browser)

	// Launch Firecracker VM.
	handle, err := m.vmm.startVM(ctx, sandboxID, vcpus, memMB, resolved.Image, envSlice, nil)
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

	if m.vmm != nil && m.vmm.snapshot != nil {
		m.vmm.snapshot.StopScratchPool()
	}

	m.tier.stop(m.vmm)

	// One manager owns the host's networking. The ix-nat table and ip_forward
	// are host-wide and idempotent, so they are left in place; the iptables
	// forward-accept rules and the gateway dummy interface (browser-remote only)
	// are removed here. Running multiple managers on one host is not supported
	// (tap names and ixgw0 are global).
	if !m.cfg.DisableNetworking {
		teardownForwardAccept(context.Background())
		if m.cfg.BrowserMode == "remote" {
			if err := teardownGatewayAddr(context.Background()); err != nil {
				m.logger.Warn("teardown gateway addr", "error", err)
			}
		}
	}

	return firstErr
}

// --- Helpers ---

// resolveOpts merges user-provided CreateOpts with manager defaults.
func (m *IXManager) resolveOpts(opts sandbox.CreateOpts) sandbox.CreateOpts {
	if opts.TTL == 0 {
		opts.TTL = m.cfg.DefaultTTL
	}
	if opts.Image == "" {
		opts.Image = m.cfg.RootfsImage
	}
	if opts.Resources.CPU == 0 {
		opts.Resources.CPU = m.cfg.PerSandbox.VCPUs
	}
	if opts.Resources.Memory == 0 {
		opts.Resources.Memory = m.cfg.PerSandbox.Memory
	}
	if opts.Resources.Disk == 0 {
		opts.Resources.Disk = 10 << 30 // 10 GB default
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
// chatID is the per-sandbox chat identifier baked into IX_CHAT_ID (used as the
// gateway routing key for browser ops). Pass a non-empty value for any VM that
// may serve browser requests — cold-boot uses the session id, pool entries use
// their sandbox id (see createPoolEntry). Empty omits IX_CHAT_ID entirely, which
// only suits VMs that will never touch the browser tier.
// browser controls browser-mode injection:
//   - nil  = manager default (browser via shared tier when BrowserGatewayURL is set)
//   - true = explicitly request browser via shared tier
//   - false = light sandbox; browser is disabled (IX_BROWSER_MODE=disabled)
func (m *IXManager) buildEnvSlice(userEnv map[string]string, chatID string, browser *bool) []string {
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

	switch {
	case browser != nil && !*browser:
		// Light sandbox: explicitly disable browser so the base-image daemon
		// does not attempt in-VM pinchtab.
		envSlice = append(envSlice, "IX_BROWSER_MODE=disabled")
	case m.cfg.BrowserGatewayURL != "":
		// Browser via shared tier (nil = manager default, or explicit true).
		envSlice = append(envSlice, "IX_BROWSER_MODE=remote="+m.cfg.BrowserGatewayURL)
		if chatID != "" {
			envSlice = append(envSlice, "IX_CHAT_ID="+chatID)
		}
		if m.cfg.GatewayToken != "" {
			envSlice = append(envSlice, "IX_BROWSER_GATEWAY_TOKEN="+m.cfg.GatewayToken)
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

	// Pool entries have no conversation assigned yet, but the daemon bakes its
	// chat id from IX_CHAT_ID at boot and a claimed VM serves exactly one chat
	// for its lifetime, so seed a stable per-VM chat id (the sandbox id). Without
	// it a pool-served browser sandbox would send an empty X-IX-Chat-Id and the
	// gateway rejects every browser op with 400 ("missing X-IX-Chat-Id header").
	envSlice := m.buildEnvSlice(nil, sandboxID, nil)

	handle, err := m.vmm.startVM(ctx, sandboxID, vcpus, memMB, m.cfg.RootfsImage, envSlice, nil)
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
