package gate_test

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
)

type discardRecorder struct{}

func (discardRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	return journal.Ref{Path: name, Size: int64(len(data))}, nil
}

// unusedResolver is never called: warm-module-cache declares no
// capabilities, so nothing asks the injector to resolve a credential.
type unusedResolver struct{}

func (unusedResolver) Resolve(context.Context, string) (string, error) {
	return "", nil
}

// discardRegistrar is never called for the same reason unusedResolver isn't.
type discardRegistrar struct{}

func (discardRegistrar) Register([]byte) {}

// TestStalledModuleDownloadFailsFastAndAttributably is #4179's regression
// guard for the mechanism warm-module-cache relies on: a stalled dependency
// download must be bounded by ITS OWN short timeout, distinct from the
// agentic implement budget, and must fail with a signal that names the
// timeout specifically rather than a generic "budget exhausted" message —
// and it must do so within an order of magnitude of its OWN declared bound,
// not anywhere near implement's multi-thousand-second budget (the reported
// incident burned 5400s of budget on a download that should have failed in
// under a minute).
//
// It exercises the real ShellExecutor (the same one the warm-module-cache
// stage's `run.command` dispatches through) and the real failure-class
// automated check (the same one warm-module-cache-gate declares), rather
// than asserting against either in isolation — a stalled download must
// survive the full path from "process didn't finish" to "gate outcome" to
// count as fixed.
func TestStalledModuleDownloadFailsFastAndAttributably(t *testing.T) {
	injector, err := credentials.NewInjector(unusedResolver{}, nil, discardRegistrar{})
	if err != nil {
		t.Fatalf("new injector: %v", err)
	}
	exec, err := executor.NewShellExecutor(injector, discardRecorder{})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}

	env := apiv1.InvocationEnvelope{
		TaskID:    "run-1:warm-module-cache",
		Workspace: t.TempDir(),
		// Mirrors warm-module-cache's own declared timeoutSeconds (a short
		// bound distinct from implement's), just compressed so the test
		// itself runs fast — the mechanism under test is "bounded separately
		// from the agentic budget", not the specific number of seconds.
		Inputs: map[string]interface{}{executor.InputTimeout: "500ms"},
	}

	start := time.Now()
	// Stands in for a stalled `go mod download`: a process that never
	// produces output and never exits on its own, exactly like the reported
	// incident's `read_bash` loop waiting on a hung fetch.
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", "while :; do :; done"},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	const implementBudget = 5400 * time.Second
	if elapsed >= implementBudget/10 {
		t.Fatalf("stalled download took %s to fail — must fail on its OWN short bound, nowhere near the %s agentic implement budget it used to silently consume", elapsed, implementBudget)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "timeout" {
		t.Fatalf("error = %+v, want a distinct \"timeout\" code — not a generic failure that would be indistinguishable from budget exhaustion", result.Error)
	}
	if !result.Error.Retryable {
		t.Fatalf("error = %+v, want retryable — a stalled download is a transient/infrastructure condition, not an implementation defect", result.Error)
	}

	inputs, err := gate.AutomatedInputs(result)
	if err != nil {
		t.Fatalf("AutomatedInputs: %v", err)
	}
	check := gate.DefaultChecks()["failure-class"]
	outcome, err := check(inputs, nil)
	if err != nil {
		t.Fatalf("failure-class check: %v", err)
	}
	if outcome != gate.OutcomeInfra {
		t.Fatalf("failure-class outcome = %q, want %q — warm-module-cache-gate's infra branch would not fire, and a stall would misroute as an ordinary implementation failure", outcome, gate.OutcomeInfra)
	}
}
