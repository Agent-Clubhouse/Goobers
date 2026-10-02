package runner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestSelfExecutionDeniedLocalRunnerParksWithoutPlacement(t *testing.T) {
	flaky := &flakyDeterministic{}
	r, runsDir := newPlacementTestRunner(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return flaky, nil }, true)
	r.cfg.SelfExecutionDenied = true
	parked := 0
	r.cfg.Blocked = func(_ context.Context, out BlockedOutcome) error {
		parked++
		if out.Reason == "" {
			t.Fatal("missing actionable park reason")
		}
		return nil
	}
	placements, refusals := 0, 0
	r.cfg.SelfExecutionObserved = func(refused bool) {
		if refused {
			refusals++
		} else {
			placements++
		}
	}
	res, err := r.Start(context.Background(), StartInput{RunID: "self-denied", Machine: retryFixtureMachine(t, 3), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}})
	if err != nil || res.Phase != journal.PhaseEscalated || parked != 1 || placements != 0 || refusals != 1 {
		t.Fatalf("result %+v err %v parked %d placements %d refusals %d", res, err, parked, placements, refusals)
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, "self-denied"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	classified := false
	for _, event := range events {
		if event.Type == journal.EventRunnerPlacement {
			t.Fatal("refused attempt reported self placement")
		}
		if event.Error != nil && event.Error.Code == SelfExecutionDeniedCode {
			classified = true
		}
	}
	if !classified {
		t.Fatal("missing classified refusal")
	}
}

func TestSelfExecutionDeniedLocalReviewerParksBeforeProvisioning(t *testing.T) {
	jr, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "denied-review", Workflow: "workflow", WorkflowVersion: 1, Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := jr.Close(); err != nil {
			t.Error(err)
		}
	}()
	parked := 0
	r := &Runner{cfg: Config{SelfExecutionDenied: true, Blocked: func(_ context.Context, o BlockedOutcome) error { parked++; return nil }}}
	_, err, _ = r.evaluateGate(context.Background(), jr, nil, nil, StartInput{RunID: "denied-review"}, apiv1.Gate{Name: "review", Evaluator: apiv1.EvaluatorAgentic}, "", apiv1.ResultEnvelope{}, nil, nil, "", "", "")
	var refused *SelfExecutionRefusal
	if !errors.As(err, &refused) || parked != 1 {
		t.Fatalf("review refusal: %v parked %d", err, parked)
	}
}
