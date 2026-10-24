package schedstart

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
)

type fakeRun struct {
	client.WorkflowRun
	result engine.RunResult
	err    error
}

func (r fakeRun) Get(_ context.Context, out any) error {
	if r.err != nil {
		return r.err
	}
	*out.(*engine.RunResult) = r.result
	return nil
}

type fakeWorkflows struct{ run fakeRun }

func (w fakeWorkflows) GetWorkflow(context.Context, string, string) client.WorkflowRun { return w.run }

func TestTemporalTerminalSeparatesDefinitiveFromTransient(t *testing.T) {
	for _, test := range []struct {
		name      string
		run       fakeRun
		wantPhase journal.RunPhase
		wantErr   bool
	}{
		{"completed result", fakeRun{result: engine.RunResult{Status: "completed", NoWork: true}}, journal.PhaseCompleted, false},
		{"failed result keeps the cause", fakeRun{result: engine.RunResult{Status: "failed", FailureCode: "boom", FailureMessage: "stage broke"}}, journal.PhaseFailed, false},
		{"workflow failed (execution error)", fakeRun{err: fmt.Errorf("closed: %w", &temporal.WorkflowExecutionError{})}, journal.PhaseFailed, false},
		{"workflow canceled (execution error)", fakeRun{err: &temporal.WorkflowExecutionError{}}, journal.PhaseFailed, false},
		{"permanently gone (NotFound)", fakeRun{err: serviceerror.NewNotFound("no such workflow")}, journal.PhaseFailed, false},
		// A transient failure to OBSERVE must never be taken for a terminal.
		{"server unavailable", fakeRun{err: serviceerror.NewUnavailable("temporal down")}, "", true},
		{"plain network error", fakeRun{err: errors.New("connection reset")}, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, err := TemporalTerminal(fakeWorkflows{run: test.run})(t.Context(), "run-1")
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil && out.Phase != test.wantPhase {
				t.Fatalf("phase = %q, want %q", out.Phase, test.wantPhase)
			}
		})
	}
	// Result details survive.
	out, err := TemporalTerminal(fakeWorkflows{run: fakeRun{result: engine.RunResult{Status: "failed", FailureCode: "boom", FailureMessage: "stage broke", NoWork: true}}})(t.Context(), "run-1")
	if err != nil || out.FailureCode != "boom" || out.FailureMessage != "stage broke" || !out.NoWork {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
}

func TestTemporalTerminalReturnsTheContextErrorAtEpochEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := TemporalTerminal(fakeWorkflows{run: fakeRun{err: serviceerror.NewUnavailable("x")}})(ctx, "run-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context error, not a terminal", err)
	}
}
