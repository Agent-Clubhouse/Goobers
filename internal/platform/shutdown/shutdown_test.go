package shutdown

import (
	"testing"
	"time"
)

// TestNotifier_RequestStopCancels exercises the service-stop hook as a fake
// trigger: RequestStop must cancel the shared context and record its reason,
// the same downstream the Windows SCM handler drives. It runs on every platform
// (no OS signals involved), so it also compile-and-run-verifies the windows path.
func TestNotifier_RequestStopCancels(t *testing.T) {
	n := NewExternal()
	defer n.Stop()

	if n.Context().Err() != nil {
		t.Fatal("context should be live before any trigger")
	}
	if n.Reason() != "" {
		t.Fatalf("Reason() = %q before trigger, want empty", n.Reason())
	}

	n.RequestStop(ReasonService)

	select {
	case <-n.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("context not cancelled after RequestStop")
	}
	if got := n.Reason(); got != ReasonService {
		t.Fatalf("Reason() = %q after RequestStop, want %q", got, ReasonService)
	}
}

// TestNotifier_RequestStopFirstWins asserts the reason is latched to the first
// trigger: a later RequestStop must not overwrite it, so a service stop already
// in progress is not relabelled.
func TestNotifier_RequestStopFirstWins(t *testing.T) {
	n := NewExternal()
	defer n.Stop()

	n.RequestStop(ReasonService)
	n.RequestStop(ReasonSignal)

	if got := n.Reason(); got != ReasonService {
		t.Fatalf("Reason() = %q, want first-trigger %q to win", got, ReasonService)
	}
}

// TestNotifier_StopCancels locks that Stop cancels the context and is safe to
// call more than once (callers defer it).
func TestNotifier_StopCancels(t *testing.T) {
	n := NewExternal()
	n.Stop()
	n.Stop() // idempotent

	select {
	case <-n.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("context not cancelled after Stop")
	}
}

// TestNotify_SignalCancels drives the real OS-signal path end to end on unix:
// Notify registers SIGINT/SIGTERM, and raising SIGTERM must cancel the context
// with ReasonSignal. On platforms where the process cannot raise the signal it
// skips. This complements the internal/signals SetupSignalContext test.
func TestNotify_SignalCancels(t *testing.T) {
	n := Notify()
	defer n.Stop()

	if !raiseTerm(t) {
		return
	}
	select {
	case <-n.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context not cancelled after OS shutdown signal")
	}
	if got := n.Reason(); got != ReasonSignal {
		t.Fatalf("Reason() = %q after signal, want %q", got, ReasonSignal)
	}
}
