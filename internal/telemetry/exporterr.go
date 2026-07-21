package telemetry

import (
	"context"
	"errors"
	"strings"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// isCollectorUnreachable reports whether err is a transient telemetry-export
// transport failure — the remote OTLP collector being unreachable, slow, or
// gone — rather than a real defect in a local exporter (a journal/stdout write,
// a nil provider). Telemetry is best-effort: neither a daemon shutdown nor a
// test that merely touches telemetry should fail because a collector is not up
// (#1124, the macOS-gate flake). Such a failure surfaces from the OTLP gRPC
// exporter as connection-refused or gRPC Unavailable, and — because that
// exporter retries the refusal with backoff until the caller's deadline —
// commonly as a context deadline from ForceFlush/Shutdown. A non-transport
// error (e.g. a local exporter failing) is NOT classified transient, so it
// still propagates to the caller.
//
// The OTLP SDK's own async export errors continue to route through the global
// otel error handler, so dropping the returned error here suppresses a spurious
// caller-facing failure without hiding the condition from operators.
func isCollectorUnreachable(err error) bool {
	if err == nil {
		return false
	}
	// An errors.Join tree is transient only if EVERY leaf is transient, so a
	// real local-exporter error joined alongside a collector timeout still
	// surfaces rather than being masked.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		leaves := joined.Unwrap()
		if len(leaves) == 0 {
			return false
		}
		for _, e := range leaves {
			if !isCollectorUnreachable(e) {
				return false
			}
		}
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	}
	// Last resort: the OTLP SDK sometimes flattens the transport cause into an
	// opaque string that carries neither a sentinel nor a gRPC status.
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "context deadline exceeded")
}
