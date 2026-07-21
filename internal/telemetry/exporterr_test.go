package telemetry

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIsCollectorUnreachable(t *testing.T) {
	realLocal := errors.New("write journal span: disk full")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context deadline (the shape ForceFlush returns after retrying a refusal)", context.DeadlineExceeded, true},
		{"context canceled", context.Canceled, true},
		{"wrapped deadline", fmt.Errorf("export: %w", context.DeadlineExceeded), true},
		{"econnrefused", syscall.ECONNREFUSED, true},
		{"grpc unavailable", status.Error(codes.Unavailable, "collector unavailable"), true},
		{"grpc deadline", status.Error(codes.DeadlineExceeded, "slow collector"), true},
		{"connection refused string", errors.New("rpc error: dial tcp 127.0.0.1:50092: connect: connection refused"), true},
		{"real local exporter error", realLocal, false},
		{"join all transient", errors.Join(context.DeadlineExceeded, status.Error(codes.Unavailable, "x")), true},
		{"join with a real error still surfaces", errors.Join(context.DeadlineExceeded, realLocal), false},
		{"empty join", errors.Join(), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCollectorUnreachable(tc.err); got != tc.want {
				t.Fatalf("isCollectorUnreachable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestFlushAndShutdownTolerateUnreachableCollector is #1124's regression: a
// client configured to export to a collector that is not running must not fail
// Flush or Shutdown — a test's correctness cannot depend on an OTLP collector
// being up — while its LOCAL exporter still captures the span (best-effort
// remote, reliable local).
func TestFlushAndShutdownTolerateUnreachableCollector(t *testing.T) {
	local := NewMemoryExporter()
	client, err := New(context.Background(), Config{
		ServiceName:  "unreachable-collector-test",
		SpanExporter: local,
		Exporter:     ExporterOTLP,
		OTLPEndpoint: "127.0.0.1:59999", // nothing is listening here
		OTLPInsecure: true,
		Batch:        true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "wf", RunID: "0af7651916cd43dd8448eb211c80319c",
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("Flush() with an unreachable collector = %v, want nil", err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() with an unreachable collector = %v, want nil", err)
	}

	// The remote export was best-effort and dropped, but the local exporter
	// still received the span — proving telemetry stays functional locally when
	// the collector is down.
	if got := len(local.Spans()); got != 1 {
		t.Fatalf("local exporter captured %d spans, want 1", got)
	}
}
