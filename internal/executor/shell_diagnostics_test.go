package executor

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestShellExecutor_DiagnosticsWatchdogRecordsSamples verifies the
// --diagnostics per-stage watchdog: a stage that runs past the (test-shrunk)
// sample-after threshold gets its capture recorded as a run artifact. The
// capture itself is stubbed via the diagnosticsCapture seam so the test does
// not depend on `sample`/`lsof` being present or on real process state.
func TestShellExecutor_DiagnosticsWatchdogRecordsSamples(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	exec.Diagnostics = true

	prevAfter, prevInterval, prevMax := diagnosticsSampleAfter, diagnosticsSampleInterval, diagnosticsMaxSamples
	diagnosticsSampleAfter, diagnosticsSampleInterval, diagnosticsMaxSamples = 40*time.Millisecond, 30*time.Millisecond, 3
	t.Cleanup(func() {
		diagnosticsSampleAfter, diagnosticsSampleInterval, diagnosticsMaxSamples = prevAfter, prevInterval, prevMax
	})

	var calls int32
	prevCap := diagnosticsCapture
	diagnosticsCapture = func(pid int) []byte {
		atomic.AddInt32(&calls, 1)
		return []byte(fmt.Sprintf("STUBSAMPLE pid=%d\n", pid))
	}
	t.Cleanup(func() { diagnosticsCapture = prevCap })

	env := baseEnvelope(t)
	// Runs long enough for the watchdog to fire at least once after the 40ms
	// threshold (but well under any timeout).
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", "sleep 0.4"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("diagnostics watchdog never captured — it must sample a stage running past the threshold")
	}
	got := string(rec.recorded["task-1/diagnostics/stage-samples.txt"])
	if !strings.Contains(got, "STUBSAMPLE") {
		t.Fatalf("diagnostics artifact = %q, want the watchdog's captured samples recorded", got)
	}
	if !strings.Contains(got, "diagnostics sample #1") {
		t.Fatalf("diagnostics artifact = %q, want it labeled per sample", got)
	}
}

// TestShellExecutor_NoDiagnosticsArtifactWhenOff confirms the watchdog is fully
// off (no artifact, no capture) when Diagnostics is false — the zero-cost path.
func TestShellExecutor_NoDiagnosticsArtifactWhenOff(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	// Diagnostics defaults to false.

	prevAfter := diagnosticsSampleAfter
	diagnosticsSampleAfter = 10 * time.Millisecond
	t.Cleanup(func() { diagnosticsSampleAfter = prevAfter })

	var calls int32
	prevCap := diagnosticsCapture
	diagnosticsCapture = func(int) []byte { atomic.AddInt32(&calls, 1); return []byte("x") }
	t.Cleanup(func() { diagnosticsCapture = prevCap })

	if _, err := exec.Run(context.Background(), baseEnvelope(t), apiv1.DeterministicRun{
		Command: []string{"sh", "-c", "sleep 0.1"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("capture called %d times with Diagnostics off; want 0", calls)
	}
	if _, ok := rec.recorded["task-1/diagnostics/stage-samples.txt"]; ok {
		t.Fatal("recorded a diagnostics artifact with Diagnostics off")
	}
}
