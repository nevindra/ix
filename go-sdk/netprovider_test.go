package ix

import (
	"context"
	"errors"
	"testing"
)

func threeNets() []vmNet {
	return []vmNet{deriveVMNet(0), deriveVMNet(1), deriveVMNet(2)}
}

func TestPreconfiguredNetAcquireRelease(t *testing.T) {
	p := newPreconfiguredNet(threeNets())
	if got := p.size(); got != 3 {
		t.Fatalf("size = %d, want 3", got)
	}
	a, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.tapName == b.tapName {
		t.Fatalf("acquired same tap twice: %s", a.tapName)
	}
	p.release(a, nil)
	c, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.tapName != a.tapName {
		t.Fatalf("released tap not reused: got %s want %s", c.tapName, a.tapName)
	}
}

func TestPreconfiguredNetExhaustion(t *testing.T) {
	p := newPreconfiguredNet([]vmNet{deriveVMNet(0)})
	if _, err := p.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := p.acquire(context.Background())
	if !errors.Is(err, ErrNetworkPoolExhausted) {
		t.Fatalf("want ErrNetworkPoolExhausted, got %v", err)
	}
}

func TestPreconfiguredNetDoubleReleaseSafe(t *testing.T) {
	p := newPreconfiguredNet([]vmNet{deriveVMNet(0)})
	a, _ := p.acquire(context.Background())
	p.release(a, nil)
	p.release(a, nil) // must not push a duplicate
	if got := p.freeCount(); got != 1 {
		t.Fatalf("freeCount = %d after double release, want 1", got)
	}
}
