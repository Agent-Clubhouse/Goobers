package readmodel

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestCreditAssignmentRanksSharedNodesAcrossRuns(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	start := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	seedCreditRun(t, store, "failed-a", start, journal.PhaseCompleted, "reject", "@abort", []NodeRow{
		{RunID: "failed-a", Kind: "gate", Name: "shared-review", Identity: "sha256:reviewer", Attempts: 2, RetryWasteAttempts: 1},
		{RunID: "failed-a", Kind: "stage", Name: "implement", Identity: "sha256:implementer", Attempts: 1},
	})
	seedCreditRun(t, store, "escalated-b", start.Add(time.Hour), journal.PhaseCompleted, "needs-changes", "@escalate", []NodeRow{
		{RunID: "escalated-b", Kind: "gate", Name: "shared-review", Identity: "sha256:reviewer", Attempts: 3, RetryWasteAttempts: 2},
	})
	seedCreditRun(t, store, "completed-c", start.Add(2*time.Hour), journal.PhaseFailed, "pass", journal.TargetComplete, []NodeRow{
		{RunID: "completed-c", Kind: "gate", Name: "shared-review", Identity: "sha256:reviewer", Attempts: 1},
	})

	got, err := store.CreditAssignment(ctx, CreditOptions{
		Gaggle: "core",
		Since:  start.Add(-time.Minute),
		Until:  start.Add(3 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d ranked nodes, want 2: %+v", len(got), got)
	}
	if got[0] != (NodeCredit{
		Gaggle: "core", Workflow: "implementation", Kind: "gate",
		Stage: "shared-review", Identity: "sha256:reviewer",
		RoutedRuns: 3, FailureRuns: 1, EscalationRuns: 1, RetryWasteAttempts: 3,
	}) {
		t.Errorf("top node = %+v", got[0])
	}
	if got[1].Stage != "implement" || got[1].FailureRuns != 1 {
		t.Errorf("second node = %+v, want failed implement node", got[1])
	}
}

func TestCreditAssignmentAppliesWindowAndWorkflowScope(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	start := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	seedCreditRun(t, store, "inside", start, journal.PhaseCompleted, "fail", "@abort", []NodeRow{
		{RunID: "inside", Kind: "stage", Name: "review", Attempts: 1},
	})
	seedCreditRun(t, store, "outside", start.Add(-48*time.Hour), journal.PhaseCompleted, "fail", "@abort", []NodeRow{
		{RunID: "outside", Kind: "stage", Name: "review", Attempts: 1},
	})

	got, err := store.CreditAssignment(ctx, CreditOptions{
		Gaggle: "core", Workflow: "implementation", Since: start.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RoutedRuns != 1 {
		t.Fatalf("scoped credit = %+v, want one routed run", got)
	}
}

func TestProjectRunDistinguishesRetriesFromSupersededTraversals(t *testing.T) {
	identity := testIdentity()
	identity.GooberDigest = "sha256:shared-prompt"
	events := []journal.Event{
		ev(1, time.Second, journal.EventStageStarted, func(e *journal.Event) {
			e.Stage, e.Attempt, e.Branch = "implement", 1, 7
		}),
		ev(2, 2*time.Second, journal.EventStageFinished, func(e *journal.Event) {
			e.Stage, e.Attempt, e.Status, e.Branch = "implement", 1, "failure", 7
		}),
		ev(3, 3*time.Second, journal.EventStageStarted, func(e *journal.Event) {
			e.Stage, e.Attempt, e.AttemptClass, e.Branch = "implement", 2, journal.AttemptPolicy, 7
		}),
		ev(4, 4*time.Second, journal.EventStageFinished, func(e *journal.Event) {
			e.Stage, e.Attempt, e.AttemptClass, e.Status, e.Branch = "implement", 2, journal.AttemptPolicy, "failure", 7
		}),
		ev(5, 5*time.Second, journal.EventStageStarted, func(e *journal.Event) {
			e.Stage, e.Attempt, e.Branch = "implement", 1, 8
		}),
		ev(6, 6*time.Second, journal.EventStageStarted, func(e *journal.Event) {
			e.Stage, e.Attempt, e.Branch = "implement", 1, 7
		}),
	}

	projection := ProjectRun(identity, Projection{}, events)
	if len(projection.Nodes) != 1 {
		t.Fatalf("nodes = %+v, want one stage node", projection.Nodes)
	}
	node := projection.Nodes[0]
	if node.Attempts != 4 || node.RetryWasteAttempts != 2 {
		t.Fatalf("node = %+v, want policy retry retained in traversal and two superseded attempts", node)
	}

	retryOnly := ProjectRun(identity, Projection{}, events[:5])
	if retryOnly.Nodes[0].RetryWasteAttempts != 0 {
		t.Fatalf("retry and separate-branch waste = %d, want 0", retryOnly.Nodes[0].RetryWasteAttempts)
	}
}

func TestProjectRunProjectsGateIdentityAndTerminalOutcome(t *testing.T) {
	identity := testIdentity()
	identity.GooberDigest = "sha256:shared-reviewer"
	projection := ProjectRun(identity, Projection{}, []journal.Event{
		ev(1, time.Second, journal.EventGateStarted, func(e *journal.Event) {
			e.Gate = "review"
		}),
		ev(2, 2*time.Second, journal.EventGateEvaluated, func(e *journal.Event) {
			e.Gate, e.Verdict, e.Target = "review", "reject", "@abort"
		}),
	})

	if len(projection.Nodes) != 1 {
		t.Fatalf("nodes = %+v, want one gate node", projection.Nodes)
	}
	if node := projection.Nodes[0]; node.Kind != "gate" || node.Name != "review" ||
		node.Identity != identity.GooberDigest {
		t.Fatalf("gate node = %+v", node)
	}
	if projection.Run.OutcomeVerdict != "reject" || projection.Run.OutcomeTarget != "@abort" {
		t.Fatalf("outcome = %q/%q", projection.Run.OutcomeVerdict, projection.Run.OutcomeTarget)
	}
}

func seedCreditRun(
	t *testing.T,
	store *Store,
	runID string,
	startedAt time.Time,
	phase journal.RunPhase,
	verdict string,
	target string,
	nodes []NodeRow,
) {
	t.Helper()
	finishedAt := startedAt.Add(time.Minute)
	if err := store.UpsertRun(context.Background(), Projection{
		Run: RunRow{
			RunID: runID, Gaggle: "core", Workflow: "implementation",
			Phase: phase, Terminal: true, StartedAt: startedAt,
			FinishedAt: &finishedAt, LastActivity: finishedAt, LastSeq: 1,
			OutcomeVerdict: verdict, OutcomeTarget: target,
		},
		Nodes: nodes,
	}); err != nil {
		t.Fatalf("seed %s: %v", runID, err)
	}
}
