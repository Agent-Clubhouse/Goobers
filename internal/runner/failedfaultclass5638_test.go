package runner

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// TestRunnerFailedTerminalCarriesExplicitFaultClass is #5638's runner half: a
// dispatch that exhausts its infrastructure retry budget (the shape of a
// harness whose required MCP control process never became ready — no agent
// turn) reaches Config.Failed with the runner's own infra classification, not
// just a code the failure streak would have to re-derive a class from. A
// session timeout carries no such marker and stays unclassified, so the #1054
// timeout case is still judged by its code.
func TestRunnerFailedTerminalCarriesExplicitFaultClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want telemetry.ErrorClass
	}{
		{
			name: "infrastructure budget exhausted",
			err: invoke.InfrastructureFailure(executor.StageFailure("HARNESS_REQUIRED_MCP_UNAVAILABLE",
				errors.New("harness: copilot-cli: Copilot control process exited before readiness"))),
			want: telemetry.ErrorClassInfra,
		},
		{
			name: "session timeout",
			err:  invoke.Timeout(errors.New("harness: copilot-cli: session timed out after 30m0s")),
			want: "",
		},
		{
			name: "unmarked dispatch error",
			err:  errors.New("executor: record stdout: no space left on device"),
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
				return alwaysErrDeterministic{err: tc.err}, nil
			}, nil)
			var got []FailedOutcome
			r.cfg.Failed = func(_ context.Context, o FailedOutcome) error {
				got = append(got, o)
				return nil
			}
			res, err := r.Start(context.Background(), StartInput{
				RunID:   "run-fault-class",
				Machine: terminalFailMachine(t),
				Gaggle:  "acme-web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			})
			if err == nil || res.Phase != journal.PhaseFailed {
				t.Fatalf("Start() = %+v, %v, want a failed terminal", res, err)
			}
			if len(got) != 1 {
				t.Fatalf("Config.Failed calls = %d, want 1", len(got))
			}
			if got[0].FaultClass != tc.want {
				t.Fatalf("FailedOutcome.FaultClass = %q, want %q (code %q)", got[0].FaultClass, tc.want, got[0].Code)
			}
		})
	}
}
