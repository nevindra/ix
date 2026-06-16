package ix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// ErrNetworkPoolExhausted is returned by a netProvider when no TAP slot is
// free. The manager surfaces it so an embedding service can queue or report it.
var ErrNetworkPoolExhausted = errors.New("ix: network pool exhausted")

// netProvider abstracts per-VM TAP acquisition. The dynamic implementation
// creates/destroys TAPs at runtime (needs CAP_NET_ADMIN); the preconfigured
// implementation hands out pre-provisioned TAPs from a manifest (no privilege).
type netProvider interface {
	// acquire returns the addressing for one VM's TAP, ready to attach to
	// Firecracker. Returns ErrNetworkPoolExhausted when none are available.
	acquire(ctx context.Context) (*vmNet, error)
	// release returns a TAP to the provider. Best-effort; logs its own errors
	// via the supplied logger (may be nil).
	release(vn *vmNet, logger *slog.Logger)
}

// dynamicNet creates and tears down TAPs on demand. This is the root-mode
// provider — identical behavior to the pre-refactor inline code.
type dynamicNet struct {
	alloc *tapAllocator
}

func newDynamicNet() *dynamicNet { return &dynamicNet{alloc: newTapAllocator(0)} }

func (d *dynamicNet) acquire(ctx context.Context) (*vmNet, error) {
	idx, err := d.alloc.alloc()
	if err != nil {
		// The allocator fails only on exhaustion; surface the shared sentinel so
		// callers can errors.Is uniformly across both providers.
		return nil, fmt.Errorf("%w: %v", ErrNetworkPoolExhausted, err)
	}
	vn, err := setupTap(ctx, idx)
	if err != nil {
		d.alloc.release(idx)
		return nil, err
	}
	return &vn, nil
}

func (d *dynamicNet) release(vn *vmNet, logger *slog.Logger) {
	if err := teardownTap(context.Background(), *vn); err != nil && logger != nil {
		logger.Warn("teardown tap", "tap", vn.tapName, "error", err)
	}
	d.alloc.release(vn.idx)
}

// preconfiguredNet hands out TAPs from a fixed pool created by ix-host-setup.
// acquire/release are pure bookkeeping — no `ip`/`nft` exec, no privilege.
type preconfiguredNet struct {
	mu    sync.Mutex
	total int     // pool capacity (immutable)
	free  []vmNet // currently available
	inUse map[int]bool
}

func newPreconfiguredNet(nets []vmNet) *preconfiguredNet {
	free := make([]vmNet, len(nets))
	copy(free, nets)
	return &preconfiguredNet{total: len(nets), free: free, inUse: make(map[int]bool)}
}

func (p *preconfiguredNet) size() int { return p.total }

func (p *preconfiguredNet) freeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.free)
}

func (p *preconfiguredNet) acquire(ctx context.Context) (*vmNet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.free) == 0 {
		return nil, ErrNetworkPoolExhausted
	}
	vn := p.free[len(p.free)-1]
	p.free = p.free[:len(p.free)-1]
	p.inUse[vn.idx] = true
	return &vn, nil
}

func (p *preconfiguredNet) release(vn *vmNet, logger *slog.Logger) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.inUse[vn.idx] {
		return // double-release or never-acquired: ignore, don't duplicate the slot
	}
	delete(p.inUse, vn.idx)
	p.free = append(p.free, *vn)
}
