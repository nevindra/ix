package ix

import (
	"testing"
	"time"
)

// idle() must be false while a request is in-flight and false before the idle
// deadline passes; true only once the VM is quiet past its idle window.
func TestSandboxIdleLifecycle(t *testing.T) {
	s := &IXSandbox{idleTTL: 50 * time.Millisecond}
	s.touch()

	if s.idle(time.Now()) {
		t.Fatal("freshly touched sandbox must not be idle")
	}

	done := s.activity()
	// Even well past the deadline, an in-flight request keeps it alive.
	if s.idle(time.Now().Add(time.Hour)) {
		t.Fatal("in-flight sandbox must never be reported idle")
	}
	done()

	// After the request ends, touch() pushed the deadline out again.
	if s.idle(time.Now()) {
		t.Fatal("sandbox just after activity must not be idle")
	}
	if !s.idle(time.Now().Add(time.Hour)) {
		t.Fatal("quiet sandbox past idle window must be idle")
	}
}
