package ix

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"

	"github.com/nevindra/oasis/sandbox"
)

// ManagerConfig configures an IXManager.
type ManagerConfig struct {
	Image         string        // default: "ghcr.io/nevindra/ix:full"
	Runtime       string        // "kata", "runsc", or "" for default Docker
	MaxConcurrent int           // 0 = auto-detect from host resources
	DefaultTTL    time.Duration // default: 1 hour
	PerSandbox    sandbox.ResourceSpec
	MaxRestarts   int // default: 3
	Logger        *slog.Logger
	DefaultEgress *EgressPolicy // optional default egress policy applied to all sandboxes
	PoolSize      int           // Number of containers to keep pre-warmed (default: 0 = disabled)
	PoolMinReady  int           // Minimum ready containers before triggering replenishment (default: 1)
}

// applyDefaults fills zero-valued fields with sensible defaults.
func (c *ManagerConfig) applyDefaults() {
	if c.Image == "" {
		c.Image = "ghcr.io/nevindra/ix:full"
	}
	if c.DefaultTTL == 0 {
		c.DefaultTTL = time.Hour
	}
	if c.PerSandbox.CPU == 0 {
		c.PerSandbox.CPU = 1
	}
	if c.PerSandbox.Memory == 0 {
		c.PerSandbox.Memory = 2 << 30 // 2 GB
	}
	if c.PerSandbox.Disk == 0 {
		c.PerSandbox.Disk = 10 << 30 // 10 GB
	}
	if c.MaxRestarts == 0 {
		c.MaxRestarts = 3
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.PoolSize > 0 && c.PoolMinReady == 0 {
		c.PoolMinReady = 1
	}
}

// poolEntry represents a pre-warmed container ready to be claimed.
type poolEntry struct {
	containerID string
	networkID   string
	baseURL     string
	socketDir   string // host-side temp dir for Unix socket
	image       string // image used to create this entry, for staleness checks
	createdAt   time.Time
}

// IXManager manages sandbox container lifecycle using Docker.
type IXManager struct {
	docker    client.APIClient
	cfg       ManagerConfig
	sandboxes map[string]*IXSandbox // keyed by sessionID
	mu        sync.RWMutex
	semaphore chan struct{} // concurrency limiter
	accepting atomic.Bool
	ctx       context.Context
	cancel    context.CancelFunc
	logger    *slog.Logger
	pool      []*poolEntry  // pre-warmed containers ready to be claimed
	poolMu    sync.Mutex    // guards pool slice
	poolStop  chan struct{} // signals pool replenisher to stop
}

// NewManager connects to Docker, auto-detects limits, and starts background
// goroutines (monitor + reaper stubs). Call Shutdown or Close to release resources.
func NewManager(ctx context.Context, cfg ManagerConfig) (*IXManager, error) {
	cfg.applyDefaults()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	// Verify Docker connectivity.
	if _, err := cli.Ping(ctx); err != nil {
		cli.Close()
		return nil, fmt.Errorf("docker ping: %w", err)
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
		docker:    cli,
		cfg:       cfg,
		sandboxes: make(map[string]*IXSandbox),
		semaphore: make(chan struct{}, maxConc),
		ctx:       mCtx,
		cancel:    cancel,
		logger:    cfg.Logger,
		poolStop:  make(chan struct{}),
	}
	m.accepting.Store(true)

	// Recover existing containers from a previous run.
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

// Create provisions a new sandbox container.
func (m *IXManager) Create(ctx context.Context, opts sandbox.CreateOpts) (sandbox.Sandbox, error) {
	if !m.accepting.Load() {
		return nil, sandbox.ErrShuttingDown
	}

	resolved := m.resolveOpts(opts)

	// Detect browser image and adjust container security for Chrome.
	isBrowserImage := strings.Contains(resolved.Image, "browser")
	if isBrowserImage && resolved.Resources.Memory < 3<<30 {
		resolved.Resources.Memory = 3 << 30 // 3 GB minimum for Chrome
	}

	// Try to grab a pre-warmed container from the pool.
	// Only use pool for non-browser images that match the configured image.
	if !isBrowserImage {
		entry := m.grabFromPool(resolved.Image)
		if entry != nil {
			// Fast path: container already running and ready.
			now := time.Now()
			ttl := resolved.TTL

			socketPath := filepath.Join(entry.socketDir, "daemon.sock")
			poolClient := &http.Client{
				Transport: unixSocketTransport(socketPath),
				Timeout:   5 * time.Minute,
			}
			sb := &IXSandbox{
				id:          resolved.SessionID,
				containerID: entry.containerID,
				baseURL:     "http://localhost",
				client:      newClient("http://localhost", poolClient),
				networkID:   entry.networkID,
				socketDir:   entry.socketDir,
				createdAt:   now,
				expiresAt:   now.Add(ttl),
			}

			m.mu.Lock()
			m.sandboxes[resolved.SessionID] = sb
			m.mu.Unlock()

			m.logger.Info("sandbox created from pool",
				"session", resolved.SessionID,
				"container", entry.containerID[:12],
				"socketDir", entry.socketDir,
				"ttl", ttl,
			)

			// Trigger async replenishment.
			go m.replenishPool()

			return sb, nil
		}
	}

	// Slow path: create on demand.

	// Acquire concurrency slot.
	if err := acquireSlot(ctx, m.semaphore, func() bool { return m.evictIdle(ctx) }, 30*time.Second); err != nil {
		return nil, err
	}

	sandboxID := uuid.NewString()[:12]
	networkName := "sandbox-" + sandboxID

	// Create per-sandbox network.
	netResp, err := m.docker.NetworkCreate(ctx, networkName, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{
			"oasis.sandbox": "true",
			"oasis.session": resolved.SessionID,
		},
	})
	if err != nil {
		m.releaseSlot()
		return nil, fmt.Errorf("create network: %w", err)
	}

	// Build container config.
	ttl := resolved.TTL
	now := time.Now()

	// Unix socket: create host-side temp dir, mount into container at /run/ix.
	socketDir := filepath.Join(os.TempDir(), "ix-"+sandboxID)
	if err := os.MkdirAll(socketDir, 0o777); err != nil {
		_ = m.docker.NetworkRemove(ctx, netResp.ID)
		m.releaseSlot()
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	_ = os.Chmod(socketDir, 0o777)
	socketPath := filepath.Join(socketDir, "daemon.sock")

	var pidsLimit int64 = 256
	hostCfg := &container.HostConfig{
		Runtime: m.cfg.Runtime,
		Resources: container.Resources{
			Memory:    resolved.Resources.Memory,
			CPUQuota:  int64(resolved.Resources.CPU) * 100000,
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

	// Chrome requires larger shared memory and relaxed seccomp for its sandbox.
	if isBrowserImage {
		hostCfg.ShmSize = 2 << 30 // 2 GB shared memory
		hostCfg.SecurityOpt = []string{"no-new-privileges:true", "seccomp=unconfined"}
	}

	// Build env vars.
	var envSlice []string
	for k, v := range resolved.Env {
		envSlice = append(envSlice, k+"="+v)
	}
	envSlice = append(envSlice, "IX_SOCKET=/run/ix/daemon.sock")

	// Inject egress env vars from the manager's default egress policy.
	if m.cfg.DefaultEgress != nil && m.cfg.DefaultEgress.Enabled {
		envSlice = append(envSlice, "IX_EGRESS_ENABLED=true")
		envSlice = append(envSlice, "IX_EGRESS_MODE="+m.cfg.DefaultEgress.Mode)
		if len(m.cfg.DefaultEgress.Rules) > 0 {
			envSlice = append(envSlice, "IX_EGRESS_RULES="+strings.Join(m.cfg.DefaultEgress.Rules, ","))
		}
	}

	containerCfg := &container.Config{
		Image: resolved.Image,
		Env:   envSlice,
		Labels: map[string]string{
			"oasis.sandbox": "true",
			"oasis.session": resolved.SessionID,
			"oasis.created": now.Format(time.RFC3339),
			"oasis.expires": now.Add(ttl).Format(time.RFC3339),
		},
	}

	resp, err := m.docker.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "sandbox-"+sandboxID)
	if err != nil {
		_ = m.docker.NetworkRemove(ctx, netResp.ID)
		_ = os.RemoveAll(socketDir)
		m.releaseSlot()
		return nil, fmt.Errorf("create container: %w", err)
	}

	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		_ = m.docker.NetworkRemove(ctx, netResp.ID)
		_ = os.RemoveAll(socketDir)
		m.releaseSlot()
		return nil, fmt.Errorf("start container: %w", err)
	}

	socketTransport := unixSocketTransport(socketPath)
	if err := m.waitReady(ctx, "http://localhost", socketTransport); err != nil {
		_ = m.destroyContainer(ctx, resp.ID, netResp.ID)
		_ = os.RemoveAll(socketDir)
		m.releaseSlot()
		return nil, fmt.Errorf("wait ready: %w", err)
	}

	httpClient := &http.Client{Transport: socketTransport, Timeout: 2 * time.Minute}
	sb := &IXSandbox{
		id:          resolved.SessionID,
		containerID: resp.ID,
		baseURL:     "http://localhost",
		client:      newClient("http://localhost", httpClient),
		networkID:   netResp.ID,
		socketDir:   socketDir,
		createdAt:   now,
		expiresAt:   now.Add(ttl),
	}

	m.mu.Lock()
	m.sandboxes[resolved.SessionID] = sb
	m.mu.Unlock()

	m.logger.Info("sandbox created",
		"session", resolved.SessionID,
		"container", resp.ID[:12],
		"socketDir", socketDir,
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

// Shutdown stops accepting new sandboxes, waits for in-flight work to drain,
// and keeps containers alive for recovery on next boot.
func (m *IXManager) Shutdown(ctx context.Context) error {
	m.accepting.Store(false)

	// Wait for context or drain (all slots released).
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

// Close force-destroys all managed sandboxes, pool entries, and their networks.
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

	// Destroy pooled containers.
	m.poolMu.Lock()
	poolEntries := m.pool
	m.pool = nil
	m.poolMu.Unlock()

	for _, entry := range poolEntries {
		if err := m.destroyContainer(context.Background(), entry.containerID, entry.networkID); err != nil && firstErr == nil {
			firstErr = err
		}
		if entry.socketDir != "" {
			_ = os.RemoveAll(entry.socketDir)
		}
		m.releaseSlot()
	}

	if err := m.docker.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// --- Helpers ---

// resolveOpts merges user-provided CreateOpts with manager defaults.
func (m *IXManager) resolveOpts(opts sandbox.CreateOpts) sandbox.CreateOpts {
	if opts.Image == "" {
		opts.Image = m.cfg.Image
	}
	if opts.TTL == 0 {
		opts.TTL = m.cfg.DefaultTTL
	}
	if opts.Resources.CPU == 0 {
		opts.Resources.CPU = m.cfg.PerSandbox.CPU
	}
	if opts.Resources.Memory == 0 {
		opts.Resources.Memory = m.cfg.PerSandbox.Memory
	}
	if opts.Resources.Disk == 0 {
		opts.Resources.Disk = m.cfg.PerSandbox.Disk
	}
	return opts
}

// Destroy stops and removes a sandbox by session ID.
func (m *IXManager) Destroy(ctx context.Context, sessionID string) error {
	return m.destroy(ctx, sessionID)
}

// destroy removes a sandbox from the map, destroys its container and network,
// and releases the concurrency slot.
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

	err := m.destroyContainer(ctx, sb.containerID, sb.networkID)
	if sb.socketDir != "" {
		_ = os.RemoveAll(sb.socketDir)
	}
	m.releaseSlot()
	return err
}

// destroyContainer stops and removes a container and its network.
func (m *IXManager) destroyContainer(ctx context.Context, containerID, networkID string) error {
	timeout := 10
	_ = m.docker.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	if err := m.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		m.logger.Warn("container remove failed", "container", containerID, "error", err)
	}
	if networkID != "" {
		if err := m.docker.NetworkRemove(ctx, networkID); err != nil {
			m.logger.Warn("network remove failed", "network", networkID, "error", err)
			return fmt.Errorf("network remove: %w", err)
		}
	}
	return nil
}

// releaseSlot returns a concurrency slot to the semaphore.
func (m *IXManager) releaseSlot() {
	select {
	case <-m.semaphore:
	default:
	}
}

// evictIdle destroys the oldest idle sandbox to free a slot. Returns true if
// a sandbox was evicted.
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

// connInfo holds connection details for a sandbox — either Unix socket or TCP.
type connInfo struct {
	baseURL    string
	httpClient *http.Client
	socketDir  string // non-empty for socket-mode containers
}

// unixSocketTransport returns an http.RoundTripper that dials the given Unix socket.
func unixSocketTransport(socketPath string) http.RoundTripper {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
}

// resolveConnection inspects a running container to determine its connection
// mode. If it has a /run/ix bind mount, a Unix socket client is returned;
// otherwise it falls back to the TCP port binding.
func (m *IXManager) resolveConnection(ctx context.Context, containerID string) (connInfo, error) {
	info, err := m.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return connInfo{}, fmt.Errorf("inspect container: %w", err)
	}

	// Socket mode: look for the /run/ix bind mount.
	for _, mount := range info.Mounts {
		if mount.Destination == "/run/ix" {
			socketPath := filepath.Join(mount.Source, "daemon.sock")
			return connInfo{
				baseURL:    "http://localhost",
				httpClient: &http.Client{Transport: unixSocketTransport(socketPath), Timeout: 2 * time.Minute},
				socketDir:  mount.Source,
			}, nil
		}
	}

	// TCP fallback for containers created before the socket change.
	if info.NetworkSettings == nil {
		return connInfo{}, fmt.Errorf("no network settings for container %s", containerID)
	}
	port := nat.Port("8080/tcp")
	bindings, ok := info.NetworkSettings.Ports[port]
	if !ok || len(bindings) == 0 {
		return connInfo{}, fmt.Errorf("no port binding for %s on container %s", port, containerID)
	}
	hostPort := bindings[0].HostPort
	if hostPort == "" {
		return connInfo{}, fmt.Errorf("empty host port for %s on container %s", port, containerID)
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%s", hostPort)
	return connInfo{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

// waitReady polls the ix daemon health endpoint until it responds with HTTP 200
// or the context/timeout expires. Pass a non-nil transport for Unix socket mode.
func (m *IXManager) waitReady(ctx context.Context, baseURL string, transport http.RoundTripper) error {
	deadline := time.After(60 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	httpClient := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	endpoint := baseURL + "/health"

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("sandbox not ready after 60s at %s", baseURL)
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				continue
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
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
func autoDetectMax(perSandbox sandbox.ResourceSpec) int {
	cpus := runtime.NumCPU()
	cpuMax := cpus / max(perSandbox.CPU, 1)

	memMax := hostMemoryBytes() / max(perSandbox.Memory, 1)

	result := min(cpuMax, int(memMax))
	if result < 1 {
		result = 1
	}
	return result
}

// --- Pool ---

// poolReplenisher runs in the background and keeps the pool filled to PoolSize.
// It performs an initial fill, then checks every second whether more entries
// are needed.
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

// fillPool creates pool entries until the pool reaches PoolSize.
func (m *IXManager) fillPool(ctx context.Context) {
	for {
		m.poolMu.Lock()
		currentSize := len(m.pool)
		m.poolMu.Unlock()

		if currentSize >= m.cfg.PoolSize {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-m.poolStop:
			return
		default:
		}

		entry, err := m.createPoolEntry(ctx)
		if err != nil {
			m.logger.Warn("pool fill failed", "error", err)
			return // back off on error; ticker will retry
		}

		m.poolMu.Lock()
		m.pool = append(m.pool, entry)
		poolSize := len(m.pool)
		m.poolMu.Unlock()

		m.logger.Info("pool entry created", "poolSize", poolSize, "target", m.cfg.PoolSize)
	}
}

// createPoolEntry creates a single pre-warmed container for the pool.
// It uses the same image and resource config as normal creates. The container
// does not get a session ID or TTL — those are assigned when claimed.
func (m *IXManager) createPoolEntry(ctx context.Context) (*poolEntry, error) {
	// Acquire concurrency slot — pool entries count toward MaxConcurrent.
	if err := acquireSlot(ctx, m.semaphore, func() bool { return false }, 30*time.Second); err != nil {
		return nil, fmt.Errorf("pool acquire slot: %w", err)
	}

	sandboxID := uuid.NewString()[:12]
	networkName := "sandbox-" + sandboxID

	netResp, err := m.docker.NetworkCreate(ctx, networkName, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{
			"oasis.sandbox": "true",
			"oasis.pool":    "true",
		},
	})
	if err != nil {
		m.releaseSlot()
		return nil, fmt.Errorf("pool create network: %w", err)
	}

	socketDir := filepath.Join(os.TempDir(), "ix-"+sandboxID)
	if err := os.MkdirAll(socketDir, 0o777); err != nil {
		_ = m.docker.NetworkRemove(ctx, netResp.ID)
		m.releaseSlot()
		return nil, fmt.Errorf("pool create socket dir: %w", err)
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
			"oasis.pool":    "true",
			"oasis.created": time.Now().Format(time.RFC3339),
		},
	}

	// Inject egress env vars for pool containers too.
	if m.cfg.DefaultEgress != nil && m.cfg.DefaultEgress.Enabled {
		containerCfg.Env = append(containerCfg.Env, "IX_EGRESS_ENABLED=true")
		containerCfg.Env = append(containerCfg.Env, "IX_EGRESS_MODE="+m.cfg.DefaultEgress.Mode)
		if len(m.cfg.DefaultEgress.Rules) > 0 {
			containerCfg.Env = append(containerCfg.Env, "IX_EGRESS_RULES="+strings.Join(m.cfg.DefaultEgress.Rules, ","))
		}
	}

	resp, err := m.docker.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "sandbox-"+sandboxID)
	if err != nil {
		_ = m.docker.NetworkRemove(ctx, netResp.ID)
		_ = os.RemoveAll(socketDir)
		m.releaseSlot()
		return nil, fmt.Errorf("pool create container: %w", err)
	}

	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		_ = m.docker.NetworkRemove(ctx, netResp.ID)
		_ = os.RemoveAll(socketDir)
		m.releaseSlot()
		return nil, fmt.Errorf("pool start container: %w", err)
	}

	socketTransport := unixSocketTransport(socketPath)
	if err := m.waitReady(ctx, "http://localhost", socketTransport); err != nil {
		_ = m.destroyContainer(ctx, resp.ID, netResp.ID)
		_ = os.RemoveAll(socketDir)
		m.releaseSlot()
		return nil, fmt.Errorf("pool wait ready: %w", err)
	}

	return &poolEntry{
		containerID: resp.ID,
		networkID:   netResp.ID,
		baseURL:     "http://localhost",
		socketDir:   socketDir,
		image:       m.cfg.Image,
		createdAt:   time.Now(),
	}, nil
}

// grabFromPool removes and returns a pool entry whose image matches the
// requested image. Returns nil if the pool is empty or no match is found.
func (m *IXManager) grabFromPool(requestedImage string) *poolEntry {
	m.poolMu.Lock()
	defer m.poolMu.Unlock()

	for i, entry := range m.pool {
		if entry.image == requestedImage {
			// Remove entry from pool (order doesn't matter for correctness,
			// but we preserve FIFO by shifting).
			m.pool = append(m.pool[:i], m.pool[i+1:]...)
			return entry
		}
	}
	return nil
}

// replenishPool creates a single pool entry if the pool is below target size.
// Called asynchronously after a pool entry is claimed.
func (m *IXManager) replenishPool() {
	// Check if pool is still needed and not shutting down.
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

// EnsureImage pulls the configured image if it is not already present locally.
func (m *IXManager) EnsureImage(ctx context.Context) error {
	_, _, err := m.docker.ImageInspectWithRaw(ctx, m.cfg.Image)
	if err == nil {
		return nil // already present
	}
	m.logger.Info("pulling image", "image", m.cfg.Image)
	rc, err := m.docker.ImagePull(ctx, m.cfg.Image, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image pull %s: %w", m.cfg.Image, err)
	}
	defer rc.Close()
	// Drain the pull output to completion.
	buf := make([]byte, 32*1024)
	for {
		if _, err := rc.Read(buf); err != nil {
			break
		}
	}
	return nil
}
