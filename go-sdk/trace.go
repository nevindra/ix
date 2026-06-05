package ix

import (
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// traceEnabled gates hot-path phase logging. Set IX_TRACE=1 when running
// benchmarks to attribute where creation/request time goes. Read once at
// process start — tracing is a measurement mode, not a runtime toggle.
var traceEnabled = os.Getenv("IX_TRACE") != ""

// tracePhase logs one named phase's elapsed time when IX_TRACE is set.
// Call as: defer tracePhase(logger, "restore", "load", time.Now()) — or with
// an explicit start for sequential phases.
func tracePhase(logger *slog.Logger, op, phase string, start time.Time) {
	if !traceEnabled || logger == nil {
		return
	}
	logger.Info("trace", "op", op, "phase", phase, "us", time.Since(start).Microseconds())
}

// dialCount counts vsock UDS dials across the process. With working HTTP
// keep-alive this stays near one per sandbox; one per REQUEST means the
// connection pool is broken (see TestSSEConnectionReuse).
var dialCount atomic.Int64

// DialCount returns the number of vsock dials so far (test/diagnostics hook).
func DialCount() int64 { return dialCount.Load() }
