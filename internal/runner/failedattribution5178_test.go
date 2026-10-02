package runner

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// TestResumedRunFailedHandlerCarriesRunAttribution is #5178's runner-side
// regression: a run that reaches its terminal failure after a daemon restart
// (Resume, not Start) must hand the Failed handler a context carrying the
// run's durable attribution. Before the fix only Start attached it, so a
// resumed run's failure comment and circuit breaker were refused by the
// daemon-write attribution guard.
func TestResumedRunFailedHandlerCarriesRunAttribution(t *testing.T) {
	machine := terminalFailMachine(t)
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return alwaysErrDeterministic{err: errors.New("stage exploded")}, nil
	}, nil)
	const runID = "run-resume-attr-5178"
	simulateCrashMidAttempt(t, runsDir, machine, runID, "implement", 1, journal.Trigger{Kind: journal.TriggerManual}, true)

	var calls int
	var got providers.Attribution
	var attributed bool
	r.cfg.Failed = func(ctx context.Context, _ FailedOutcome) error {
		calls++
		got, attributed = providers.AttributionFromContext(ctx)
		return nil
	}

	res, _ := r.Resume(context.Background(), ResumeInput{
		RunID:   runID,
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if calls != 1 {
		t.Fatalf("Config.Failed calls = %d, want 1", calls)
	}
	if !attributed {
		t.Fatal("Failed handler context carries no run attribution; failure handling would be refused by the daemon-write guard")
	}
	if got.Run != runID || got.Gaggle != "acme-web" || got.Workflow != machine.Def.Name || got.Goober != "runner" {
		t.Fatalf("attribution = %+v, want run %q gaggle acme-web workflow %q goober runner", got, runID, machine.Def.Name)
	}
}
