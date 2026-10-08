package readservice

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/runcontrol"
)

func appendReliabilityEvents(t *testing.T, run *journal.Run, clock *fixtureClock, events ...journal.Event) {
	t.Helper()
	for _, event := range events {
		clock.advance(time.Second)
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
}

func projectReliabilityRuns(t *testing.T, layout instance.Layout, runIDs ...string) *readmodel.Store {
	t.Helper()
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, runID := range runIDs {
		reader, err := journal.OpenRead(filepath.Join(layout.RunsDir(), runID))
		if err != nil {
			t.Fatal(err)
		}
		identity, err := reader.Identity()
		if err != nil {
			t.Fatal(err)
		}
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		projection, err := readmodel.ProjectRunFromJournal(reader, identity, events)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertRun(context.Background(), projection); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func reliabilityByRun(t *testing.T, sources LocalSources) map[string]RunReliability {
	t.Helper()
	service, err := NewLocal(sources, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return fixedTime.Add(time.Hour) }
	page, err := service.ListRuns(context.Background(), RunListOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]RunReliability, len(page.Runs))
	for _, run := range page.Runs {
		if run.Operator.Reliability == nil {
			t.Fatalf("run %s has no reliability projection", run.ID)
		}
		out[run.ID] = *run.Operator.Reliability
	}
	return out
}

func budgetByKind(t *testing.T, reliability RunReliability, kind string) ReliabilityBudget {
	t.Helper()
	for _, budget := range reliability.Budgets {
		if budget.Kind == kind {
			return budget
		}
	}
	t.Fatalf("budget %s missing from %+v", kind, reliability.Budgets)
	return ReliabilityBudget{}
}

func assertBudget(t *testing.T, name string, budget ReliabilityBudget, consumed, remaining *int, evidence string) {
	t.Helper()
	if !reflect.DeepEqual(budget.Consumed, consumed) || !reflect.DeepEqual(budget.Remaining, remaining) || budget.Evidence != evidence {
		t.Fatalf("%s budget %s = consumed %v remaining %v evidence %q, want %v/%v/%q",
			name, budget.Kind, budget.Consumed, budget.Remaining, budget.Evidence, consumed, remaining, evidence)
	}
}

func intPtr(n int) *int { return &n }

// TestRunReliabilityProjectsActiveRetryingAndEscalatedRuns is #5313's
// read-model acceptance: active, retrying, and escalated runs project the same
// reliability state from the journal and the SQLite read model, and evidence
// neither source records is reported unknown rather than zero.
func TestRunReliabilityProjectsActiveRetryingAndEscalatedRuns(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	machine := fixtureMachine(t)
	trigger := journal.Trigger{Kind: journal.TriggerItem, Ref: "5313"}

	active, clock := createFixtureRun(t, layout, machine, "reliability-active", "implementation", "goobers", fixedTime, trigger, true)
	appendReliabilityEvents(t, active, clock, journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}

	retrying, clock := createFixtureRun(t, layout, machine, "reliability-retrying", "implementation", "goobers", fixedTime.Add(time.Minute), trigger, true)
	appendReliabilityEvents(t, retrying, clock,
		journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		journal.Event{
			Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultFailure),
			Error: &journal.ErrorDetail{Code: "workspace_failed", Causes: []journal.ErrorCause{{Code: "workspace_failed", Class: " Infra "}}},
		},
		journal.Event{Type: journal.EventRunnerAnnotation, Stage: "implement", Attempt: 2, Runner: map[string]any{
			"kind": journal.RetryBackoffKind, "driver": "local", "retryClass": string(journal.AttemptInfra),
			"observedAt": fixedTime.Add(time.Minute).Format(time.RFC3339Nano),
			"deadline":   fixedTime.Add(time.Hour).Format(time.RFC3339Nano),
		}},
	)
	if err := retrying.Close(); err != nil {
		t.Fatal(err)
	}

	escalated, clock := createFixtureRun(t, layout, machine, "reliability-escalated", "implementation", "goobers", fixedTime.Add(2*time.Minute), trigger, true)
	appendReliabilityEvents(t, escalated, clock,
		journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		journal.Event{Type: journal.EventError, Stage: "implement", Error: &journal.ErrorDetail{
			Code: "workspace_failed", Message: "recovered earlier", Causes: []journal.ErrorCause{{Code: "workspace_failed", Class: "infra"}},
		}},
		journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "escalate", Target: journal.TargetEscalate},
		journal.Event{
			Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated),
			TerminalCause: &journal.TerminalCause{
				Schema: journal.TerminalCauseSchema, Phase: journal.PhaseEscalated,
				Classification: journal.TerminalEscalation, SelectorKind: "gate", Selector: "review",
				Verdict: "escalate", Target: journal.TargetEscalate, Code: runcontrol.ReasonInfrastructureBudgetExhausted,
				Message: "reviewer requested a human decision", CausalEventSeq: escalated.Seq(),
				Repass: &journal.TerminalBudget{Consumed: 1, Allowed: 3},
			},
		},
	)
	if err := escalated.Close(); err != nil {
		t.Fatal(err)
	}

	runIDs := []string{"reliability-active", "reliability-retrying", "reliability-escalated"}
	fromJournal := reliabilityByRun(t, LocalSources{Layout: layout, Definitions: testDefinitions()})
	store := projectReliabilityRuns(t, layout, runIDs...)
	fromReadModel := reliabilityByRun(t, LocalSources{Layout: layout, Definitions: testDefinitions(), ReadModel: store})
	// The read model stores no terminal-cause column, so only active and
	// retrying runs are expected to be identical across the two list paths.
	for _, runID := range runIDs[:2] {
		if !reflect.DeepEqual(fromJournal[runID], fromReadModel[runID]) {
			t.Fatalf("%s: journal reliability %s != read model %s", runID, fromJournal[runID].StatusLine(), fromReadModel[runID].StatusLine())
		}
	}

	got := fromJournal["reliability-active"]
	if got.State != "active" || got.CurrentStage != "implement" || got.CurrentAttempt != 1 ||
		got.Failure.Classification != "none" || got.Failure.EvidenceRule != "no-error-recorded" ||
		got.LatestVerdict != "unknown" || got.Acceptance.State != "unknown" ||
		got.NextAction != "finish implement" || got.HumanInterventionReason != "" {
		t.Fatalf("active reliability = %+v", got)
	}
	for _, kind := range []string{"local-validation", "provider-remediation", "ci-poll"} {
		assertBudget(t, "active", budgetByKind(t, got, kind), nil, nil, "unknown")
	}

	got = fromJournal["reliability-retrying"]
	if got.State != "retrying" || got.NextAction != "wait for retry backoff on implement" ||
		got.Failure.Classification != "infra" || got.Failure.EvidenceRule != "latestError.causes.class" || got.Failure.Code != "workspace_failed" {
		t.Fatalf("retrying reliability = %+v", got)
	}
	assertBudget(t, "retrying", budgetByKind(t, got, "local-infra"), intPtr(0), nil, "journal")
	assertBudget(t, "retrying", budgetByKind(t, got, "implementation-review"), intPtr(0), nil, "journal")
	if line := got.StatusLine(); !strings.HasPrefix(line, "retrying; failure infra") ||
		!strings.Contains(line, "local-infra 0 used/? left") || !strings.Contains(line, "ci-poll ? used/? left") {
		t.Fatalf("retrying status line = %q", line)
	}

	got = fromJournal["reliability-escalated"]
	if got.State != "escalated" || got.LatestVerdict != "escalate" ||
		got.NextAction != "human intervention required" {
		t.Fatalf("escalated reliability = %+v", got)
	}
	assertBudget(t, "escalated list", budgetByKind(t, got, "implementation-review"), intPtr(0), nil, "journal")
	// Neither list path carries the recorded terminal cause, and the earlier
	// recovered infra error must not be reported as what ended the run.
	for name, got := range map[string]RunReliability{"journal": got, "read model": fromReadModel["reliability-escalated"]} {
		if got.State != "escalated" || got.Failure.Classification != "unknown" ||
			got.Failure.EvidenceRule != "terminal-cause-not-recorded" || got.HumanInterventionReason != "unknown" {
			t.Fatalf("%s escalated reliability must mark missing cause unknown: %+v", name, got)
		}
	}

	service, err := NewLocal(LocalSources{Layout: layout, Definitions: testDefinitions()}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	detail, err := service.GetRun(context.Background(), "reliability-escalated")
	if err != nil {
		t.Fatal(err)
	}
	refined := detail.Operator.Reliability
	if refined == nil || refined.Failure.Classification != string(journal.TerminalEscalation) ||
		refined.Failure.EvidenceRule != "terminalCause.classification" ||
		refined.Failure.Code != runcontrol.ReasonInfrastructureBudgetExhausted ||
		refined.HumanInterventionReason != "reviewer requested a human decision" {
		t.Fatalf("detail reliability = %+v", refined)
	}
	assertBudget(t, "escalated detail", budgetByKind(t, *refined, "local-infra"), intPtr(1), intPtr(2), "terminalCause")
	assertBudget(t, "escalated detail", budgetByKind(t, *refined, "implementation-review"), intPtr(0), nil, "journal")
	assertBudget(t, "escalated detail", budgetByKind(t, *refined, "ci-poll"), nil, nil, "unknown")
}

func TestRunReliabilityCompletedRunIgnoresRecoveredErrorAndShowsRetainedRefs(t *testing.T) {
	summary := RunSummary{Phase: journal.PhaseCompleted, Terminal: true}
	summary.Operator.LatestError = &journal.ErrorDetail{Code: "workspace_failed", Causes: []journal.ErrorCause{{Class: "infra"}}}
	summary.Operator.PullRequest = &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "42"}
	summary.Lineage = &RunLineage{WorkspaceBranch: "goobers/5313", WorkspaceBranchSHA: "abc123"}
	got := projectRunReliability(summary)
	if got.Failure.Classification != "none" || got.Failure.EvidenceRule != "run-completed" {
		t.Fatalf("completed run failure = %+v", got.Failure)
	}
	if line := got.StatusLine(); !strings.Contains(line, "; retained branch goobers/5313@abc123, pr 42;") {
		t.Fatalf("status line missing retained refs: %q", line)
	}
}
