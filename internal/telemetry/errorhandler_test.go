package telemetry

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type loggedError struct {
	msg  string
	args []any
}

func newTestExportErrorHandler(now *time.Time) (*exportErrorHandler, *[]loggedError) {
	var logged []loggedError
	h := newExportErrorHandler()
	h.now = func() time.Time { return *now }
	h.log = func(msg string, args ...any) {
		logged = append(logged, loggedError{msg: msg, args: args})
	}
	return h, &logged
}

func argValue(t *testing.T, args []any, key string) (any, bool) {
	t.Helper()
	for i := 0; i+1 < len(args); i += 2 {
		name, ok := args[i].(string)
		if !ok {
			t.Fatalf("argument %d is not a key: %#v", i, args[i])
		}
		if name == key {
			return args[i+1], true
		}
	}
	return nil, false
}

func TestExportErrorHandlerLogsFirstOccurrenceThenSuppresses(t *testing.T) {
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	h, logged := newTestExportErrorHandler(&now)

	err := errors.New("failed to upload metrics: connection reset")
	h.Handle(err)
	if len(*logged) != 1 {
		t.Fatalf("first occurrence: got %d log lines, want 1", len(*logged))
	}
	if (*logged)[0].msg != "telemetry export failed" {
		t.Fatalf("first occurrence message = %q", (*logged)[0].msg)
	}

	for i := 0; i < 29; i++ {
		now = now.Add(time.Minute)
		h.Handle(err)
	}
	if len(*logged) != 1 {
		t.Fatalf("within the window: got %d log lines, want 1", len(*logged))
	}
}

func TestExportErrorHandlerReportsCountAfterWindow(t *testing.T) {
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	h, logged := newTestExportErrorHandler(&now)

	err := errors.New("failed to upload metrics: connection reset")
	h.Handle(err)
	for i := 0; i < 30; i++ {
		now = now.Add(time.Minute)
		h.Handle(err)
	}

	if len(*logged) != 2 {
		t.Fatalf("after the window: got %d log lines, want 2", len(*logged))
	}
	summary := (*logged)[1]
	if summary.msg != "telemetry export still failing" {
		t.Fatalf("summary message = %q", summary.msg)
	}
	repeats, ok := argValue(t, summary.args, "repeats")
	if !ok {
		t.Fatalf("summary carries no repeats count: %#v", summary.args)
	}
	if repeats != 30 {
		t.Fatalf("repeats = %v, want 30", repeats)
	}

	// The count restarts after a summary rather than accumulating for the life
	// of the process; two summaries must not double-count the same occurrence.
	for i := 0; i < 30; i++ {
		now = now.Add(time.Minute)
		h.Handle(err)
	}
	if len(*logged) != 3 {
		t.Fatalf("second window: got %d log lines, want 3", len(*logged))
	}
	repeats, ok = argValue(t, (*logged)[2].args, "repeats")
	if !ok {
		t.Fatalf("second summary carries no repeats count")
	}
	if repeats != 30 {
		t.Fatalf("second summary repeats = %v, want 30", repeats)
	}
}

func TestExportErrorHandlerSeparatesDistinctFailures(t *testing.T) {
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	h, logged := newTestExportErrorHandler(&now)

	h.Handle(errors.New("failed to upload metrics: connection reset"))
	h.Handle(errors.New("failed to upload traces: connection reset"))
	h.Handle(errors.New("failed to upload metrics: connection reset"))

	if len(*logged) != 2 {
		t.Fatalf("got %d log lines, want 2 (one per distinct failure)", len(*logged))
	}
}

func TestExportErrorHandlerExplainsUnimplemented(t *testing.T) {
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	h, logged := newTestExportErrorHandler(&now)

	err := status.Error(codes.Unimplemented,
		"unknown service opentelemetry.proto.collector.metrics.v1.MetricsService")
	h.Handle(err)

	if len(*logged) != 1 {
		t.Fatalf("got %d log lines, want 1", len(*logged))
	}
	hint, ok := argValue(t, (*logged)[0].args, "hint")
	if !ok {
		t.Fatalf("Unimplemented carries no hint: %#v", (*logged)[0].args)
	}
	text, _ := hint.(string)
	if text == "" {
		t.Fatalf("hint is empty")
	}
}

func TestExportErrorHandlerHintsOnlyWhereItApplies(t *testing.T) {
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	h, logged := newTestExportErrorHandler(&now)

	h.Handle(status.Error(codes.Unavailable, "collector is down"))

	if len(*logged) != 1 {
		t.Fatalf("got %d log lines, want 1", len(*logged))
	}
	if _, ok := argValue(t, (*logged)[0].args, "hint"); ok {
		t.Fatalf("a transient Unavailable must not carry the configuration hint")
	}
}

func TestExportErrorHandlerIgnoresNil(t *testing.T) {
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	h, logged := newTestExportErrorHandler(&now)

	h.Handle(nil)

	if len(*logged) != 0 {
		t.Fatalf("a nil error logged %d lines", len(*logged))
	}
}

func TestExportErrorHandlerBoundsItsTable(t *testing.T) {
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	h, _ := newTestExportErrorHandler(&now)

	for i := 0; i < exportErrorDistinctLimit*3; i++ {
		h.Handle(errors.New("failure " + time.Duration(i).String()))
	}

	h.mu.Lock()
	size := len(h.seen)
	h.mu.Unlock()
	if size > exportErrorDistinctLimit {
		t.Fatalf("signature table grew to %d, want at most %d", size, exportErrorDistinctLimit)
	}
}
