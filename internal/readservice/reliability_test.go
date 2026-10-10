package readservice

import (
	"context"
	"encoding/json"
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
	"github.com/goobers/goobers/internal/workflow"
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
	pinned := pinnedBudgetMachine(t)
	trigger := journal.Trigger{Kind: journal.TriggerItem, Ref: "5313"}

	active, clock := createPinnedRun(t, layout, pinned, "reliability-active", fixedTime)
	appendReliabilityEvents(t, active, clock,
		journal.Event{Type: journal.EventStageStarted, Stage: "map-acceptance", Attempt: 1},
		journal.Event{Type: journal.EventStageFinished, Stage: "map-acceptance", Attempt: 1, Status: string(apiv1.ResultSuccess),
			Outputs: map[string]any{"acceptanceMapping": "Partial", "acceptanceMappingDigest": "sha256:acc"}},
		journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
	)
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}

	retrying, clock := createPinnedRun(t, layout, pinned, "reliability-retrying", fixedTime.Add(time.Minute))
	appendReliabilityEvents(t, retrying, clock,
		journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultFailure)},
		journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptPolicy},
		journal.Event{
			Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, Status: string(apiv1.ResultFailure),
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

	escalated, clock := createPinnedRun(t, layout, pinned, "reliability-escalated", fixedTime.Add(2*time.Minute))
	appendReliabilityEvents(t, escalated, clock,
		journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		journal.Event{Type: journal.EventError, Stage: "implement", Error: &journal.ErrorDetail{
			Code: "workspace_failed", Message: "recovered earlier", Causes: []journal.ErrorCause{{Code: "workspace_failed", Class: "infra"}},
		}},
		journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		journal.Event{Type: journal.EventStageStarted, Stage: "open-pr", Attempt: 1},
		journal.Event{Type: journal.EventRefTouched, Stage: "open-pr", ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "42"}},
		journal.Event{Type: journal.EventStageFinished, Stage: "open-pr", Attempt: 1, Status: string(apiv1.ResultSuccess),
			Outputs: map[string]any{"opened": "true", "prNumber": "42", "draft": "true"}},
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

	legacy, clock := createFixtureRun(t, layout, machine, "reliability-legacy", "implementation", "goobers", fixedTime.Add(3*time.Minute), trigger, true)
	appendReliabilityEvents(t, legacy, clock,
		journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		journal.Event{Type: journal.EventError, Stage: "implement", Error: &journal.ErrorDetail{
			Code: "workspace_failed", Message: "recovered earlier", Causes: []journal.ErrorCause{{Code: "workspace_failed", Class: "infra"}},
		}},
		journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "escalate", Target: journal.TargetEscalate},
		journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	runIDs := []string{"reliability-active", "reliability-retrying", "reliability-escalated", "reliability-legacy"}
	fromJournal := reliabilityByRun(t, LocalSources{Layout: layout, Definitions: testDefinitions()})
	store := projectReliabilityRuns(t, layout, runIDs...)
	fromReadModel := reliabilityByRun(t, LocalSources{Layout: layout, Definitions: testDefinitions(), ReadModel: store})
	for _, runID := range runIDs {
		if !reflect.DeepEqual(fromJournal[runID], fromReadModel[runID]) {
			t.Fatalf("%s: journal reliability %s != read model %s", runID, fromJournal[runID].StatusLine(), fromReadModel[runID].StatusLine())
		}
	}

	got := fromJournal["reliability-active"]
	if got.State != "active" || got.CurrentStage != "implement" || got.CurrentAttempt != 1 ||
		got.Failure.Classification != "none" || got.Failure.EvidenceRule != "no-error-recorded" ||
		got.LatestVerdict != "unknown" || got.Acceptance != (ReliabilityAcceptance{State: "partial", Digest: "sha256:acc"}) ||
		got.NextAction != "finish implement" || got.HumanInterventionReason != "" {
		t.Fatalf("active reliability = %+v", got)
	}
	// Every budget is bounded by the allowance pinned at run start: the
	// inherited MaxRepasses (5), the ci-gate override (2), its polling bound
	// (6), and implement's retry policy (3 attempts) and infra allowance.
	for kind, remaining := range map[string]int{
		"implementation-review": 5, "stage-policy": 2, "local-infra": 1,
		"local-validation": 5, "provider-remediation": 2, "ci-poll": 6,
	} {
		assertBudget(t, "active", budgetByKind(t, got, kind), intPtr(0), intPtr(remaining), "pinnedDefinition")
	}

	got = fromJournal["reliability-retrying"]
	// No attempt is active during backoff; the backing-off attempt is current.
	if got.State != "retrying" || got.CurrentStage != "implement" || got.CurrentAttempt != 2 ||
		got.NextAction != "wait for retry backoff on implement" ||
		got.Failure.Classification != "infra" || got.Failure.EvidenceRule != "latestError.causes.class" || got.Failure.Code != "workspace_failed" {
		t.Fatalf("retrying reliability = %+v", got)
	}
	assertBudget(t, "retrying", budgetByKind(t, got, "stage-policy"), intPtr(1), intPtr(1), "pinnedDefinition")
	assertBudget(t, "retrying", budgetByKind(t, got, "local-infra"), intPtr(0), intPtr(1), "pinnedDefinition")
	assertBudget(t, "retrying", budgetByKind(t, got, "implementation-review"), intPtr(0), intPtr(5), "pinnedDefinition")
	if line := got.StatusLine(); !strings.HasPrefix(line, "retrying implement attempt 2; failure infra") ||
		!strings.Contains(line, "stage-policy 1 used/1 left") || !strings.Contains(line, "local-infra 0 used/1 left") ||
		!strings.Contains(line, "ci-poll 0 used/6 left") {
		t.Fatalf("retrying status line = %q", line)
	}

	// Both list paths fold the recorded terminal cause from run.finished, so
	// status and the dashboard show the exact reason and pinned budgets.
	got = fromJournal["reliability-escalated"]
	if got.State != "escalated" || got.LatestVerdict != "escalate" ||
		got.NextAction != "human intervention required" ||
		got.Failure != (ReliabilityFailure{Classification: string(journal.TerminalEscalation), EvidenceRule: "terminalCause.classification", Code: runcontrol.ReasonInfrastructureBudgetExhausted}) ||
		got.HumanInterventionReason != "reviewer requested a human decision" ||
		got.Acceptance.State != "unknown" || got.Retained.PullRequestDraft != "draft" {
		t.Fatalf("escalated reliability = %+v", got)
	}
	assertBudget(t, "escalated", budgetByKind(t, got, "local-infra"), intPtr(1), intPtr(2), "terminalCause")
	assertBudget(t, "escalated", budgetByKind(t, got, "implementation-review"), intPtr(0), intPtr(5), "pinnedDefinition")
	assertBudget(t, "escalated", budgetByKind(t, got, "ci-poll"), intPtr(0), intPtr(6), "pinnedDefinition")
	if line := got.StatusLine(); !strings.Contains(line, "local-infra 1 used/2 left") ||
		!strings.Contains(line, "retained branch goobers/5313@abc123, pr 42 (draft)") ||
		!strings.HasSuffix(line, "; needs human: reviewer requested a human decision") {
		t.Fatalf("escalated status line = %q", line)
	}

	// Without a recorded terminal cause the earlier recovered infra error
	// must not be reported as what ended the run.
	got = fromJournal["reliability-legacy"]
	if got.State != "escalated" || got.Failure.Classification != "unknown" ||
		got.Failure.EvidenceRule != "terminal-cause-not-recorded" || got.HumanInterventionReason != "unknown" {
		t.Fatalf("legacy escalated reliability must mark missing cause unknown: %+v", got)
	}
	// Without a pinned definition no allowance is known, so remaining counts
	// stay unknown rather than read from today's configuration.
	assertBudget(t, "legacy", budgetByKind(t, got, "implementation-review"), intPtr(0), nil, "journal")
	assertBudget(t, "legacy", budgetByKind(t, got, "ci-poll"), nil, nil, "unknown")

	service, err := NewLocal(LocalSources{Layout: layout, Definitions: testDefinitions()}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	detail, err := service.GetRun(context.Background(), "reliability-escalated")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Operator.Reliability == nil || !reflect.DeepEqual(*detail.Operator.Reliability, fromJournal["reliability-escalated"]) {
		t.Fatalf("detail reliability = %+v, want list projection %+v", detail.Operator.Reliability, fromJournal["reliability-escalated"])
	}
}

func TestRunReliabilityCompletedRunIgnoresRecoveredErrorAndShowsRetainedRefs(t *testing.T) {
	summary := RunSummary{Phase: journal.PhaseCompleted, Terminal: true}
	summary.Operator.LatestError = &journal.ErrorDetail{Code: "workspace_failed", Causes: []journal.ErrorCause{{Class: "infra"}}}
	summary.Operator.PullRequest = &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "42"}
	summary.workspaceBranch, summary.workspaceBranchSHA = "goobers/5313", "abc123"
	got := projectRunReliability(summary)
	if got.Failure.Classification != "none" || got.Failure.EvidenceRule != "run-completed" {
		t.Fatalf("completed run failure = %+v", got.Failure)
	}
	if line := got.StatusLine(); !strings.Contains(line, "; retained branch goobers/5313@abc123, pr 42 (unknown);") {
		t.Fatalf("status line missing retained refs: %q", line)
	}
}

func pinnedBudgetMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "implementation",
		Version: 4,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers",
			Start:  "implement",
			Tasks: []apiv1.Task{
				{Name: "implement", Type: apiv1.TaskAgentic, Goal: "implement the issue", Next: "review", Retry: &apiv1.RetryPolicy{MaxAttempts: 3}},
				{Name: "local-ci", Type: apiv1.TaskAgentic, Goal: "run local validation", Next: "local-gate"},
				{Name: "open-pr", Type: apiv1.TaskAgentic, Goal: "open the pull request", Next: "ci-gate"},
				{Name: "remediate-ci", Type: apiv1.TaskAgentic, Goal: "repair CI", Next: "ci-gate"},
				{Name: "ci-poll", Type: apiv1.TaskAgentic, Goal: "poll CI", Next: "ci-gate"},
			},
			Gates: []apiv1.Gate{
				{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
					Branches: map[string]string{"pass": "local-gate", "needs-changes": "implement", "fail": "implement"}},
				{Name: "local-gate", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "failure-class"},
					Branches: map[string]string{"pass": "open-pr", "fail": "implement", "infra": "local-ci"}},
				{Name: "ci-gate", Evaluator: apiv1.EvaluatorAutomated, MaxRepasses: 2,
					Automated: &apiv1.AutomatedGate{Check: "ci-status", MaxTimeoutPolls: 6},
					Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": "remediate-ci", "timeout": "ci-poll"}},
			},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

// createPinnedRun records an ordinary (non-continuation) run the way the
// runner does: trusted pinned definition, inherited run controls, and the
// workspace branch it executes.
func createPinnedRun(t *testing.T, layout instance.Layout, machine *workflow.Machine, runID string, startedAt time.Time) (*journal.Run, *fixtureClock) {
	t.Helper()
	graph, err := json.Marshal(machine.Graph())
	if err != nil {
		t.Fatal(err)
	}
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fixtureClock{now: startedAt}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version, WorkflowDigest: machine.Digest(),
		Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerItem, Ref: "5313"}, StartedAt: startedAt,
		RunControls:     &apiv1.RunControls{MaxRepasses: 5},
		WorkspaceBranch: "goobers/5313", WorkspaceBranchSHA: "abc123",
	}, map[string][]byte{
		journal.PinnedWorkflowGraphInputName:      graph,
		journal.PinnedWorkflowDefinitionInputName: definition,
	}, journal.WithClock(func() time.Time { return clock.now }), journal.WithInputIntegrity(map[string]apiv1.Integrity{
		journal.PinnedWorkflowGraphInputName:      apiv1.IntegrityTrusted,
		journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return run, clock
}

func gateCharge(gate, verdict, target string, runner map[string]any) journal.Event {
	return journal.Event{Type: journal.EventGateEvaluated, Gate: gate, Verdict: verdict, Target: target, Runner: runner}
}

// TestRunReliabilityProjectsPinnedGateBudgetsAndOrdinaryWorkspaceBranch
// covers #5313's local-validation, provider-remediation, and CI-poll budgets:
// both list paths count the gates' journaled charges against the allowances
// pinned at run start, and an ordinary run retains its workspace branch.
func TestRunReliabilityProjectsPinnedGateBudgetsAndOrdinaryWorkspaceBranch(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	machine := pinnedBudgetMachine(t)

	polling, clock := createPinnedRun(t, layout, machine, "reliability-polling", fixedTime)
	stage := func(name string) []journal.Event {
		return []journal.Event{
			{Type: journal.EventStageStarted, Stage: name, Attempt: 1},
			{Type: journal.EventStageFinished, Stage: name, Attempt: 1, Status: string(apiv1.ResultSuccess)},
		}
	}
	// The runner charges a budget only when the branch re-enters a completed
	// stage, so first visits to remediate-ci and ci-poll charge nothing.
	var events []journal.Event
	events = append(events, stage("implement")...)
	events = append(events, gateCharge("local-gate", "fail", "implement", map[string]any{"repassAttempt": 1, "repassTarget": "implement"}))
	events = append(events, stage("implement")...)
	events = append(events, gateCharge("local-gate", "pass", "open-pr", map[string]any{"repassAttempt": 0}))
	events = append(events, stage("open-pr")...)
	events = append(events, gateCharge("ci-gate", "fail", "remediate-ci", map[string]any{"repassAttempt": 0}))
	events = append(events, stage("remediate-ci")...)
	events = append(events, gateCharge("ci-gate", "fail", "remediate-ci", map[string]any{"repassAttempt": 1, "repassTarget": "remediate-ci"}))
	events = append(events, stage("remediate-ci")...)
	events = append(events, gateCharge("ci-gate", "timeout", "ci-poll", map[string]any{"repassAttempt": 0}))
	events = append(events, stage("ci-poll")...)
	events = append(events, gateCharge("ci-gate", "timeout", "ci-poll", map[string]any{"repassAttempt": 0, "pollAttempt": 1, "pollTarget": "ci-poll"}))
	appendReliabilityEvents(t, polling, clock, events...)
	if err := polling.Close(); err != nil {
		t.Fatal(err)
	}

	exhausted, clock := createPinnedRun(t, layout, machine, "reliability-ci-exhausted", fixedTime.Add(time.Minute))
	appendReliabilityEvents(t, exhausted, clock,
		gateCharge("ci-gate", "fail", journal.TargetEscalate, map[string]any{"repassAttempt": 3, "repassTarget": "remediate-ci", "escalated": true}),
		journal.Event{
			Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated),
			TerminalCause: &journal.TerminalCause{
				Schema: journal.TerminalCauseSchema, Phase: journal.PhaseEscalated,
				Classification: journal.TerminalPolicyExhaustion, SelectorKind: "gate", Selector: "ci-gate",
				Verdict: "fail", Target: journal.TargetEscalate, Code: runcontrol.ReasonRepassBudgetExhausted,
				Message: "CI kept failing", CausalEventSeq: exhausted.Seq(),
				Repass: &journal.TerminalBudget{Consumed: 2, Allowed: 2},
			},
		},
	)
	if err := exhausted.Close(); err != nil {
		t.Fatal(err)
	}

	runIDs := []string{"reliability-polling", "reliability-ci-exhausted"}
	fromJournal := reliabilityByRun(t, LocalSources{Layout: layout, Definitions: testDefinitions()})
	store := projectReliabilityRuns(t, layout, runIDs...)
	fromReadModel := reliabilityByRun(t, LocalSources{Layout: layout, Definitions: testDefinitions(), ReadModel: store})
	for _, runID := range runIDs {
		if !reflect.DeepEqual(fromJournal[runID], fromReadModel[runID]) {
			t.Fatalf("%s: journal reliability %s != read model %s", runID, fromJournal[runID].StatusLine(), fromReadModel[runID].StatusLine())
		}
	}

	got := fromJournal["reliability-polling"]
	assertBudget(t, "polling", budgetByKind(t, got, "local-validation"), intPtr(1), intPtr(4), "pinnedDefinition")
	assertBudget(t, "polling", budgetByKind(t, got, "provider-remediation"), intPtr(1), intPtr(1), "pinnedDefinition")
	assertBudget(t, "polling", budgetByKind(t, got, "ci-poll"), intPtr(1), intPtr(5), "pinnedDefinition")
	if got.Retained.Branch != "goobers/5313" || got.Retained.BranchSHA != "abc123" || got.Retained.RecoveryRunID != "" {
		t.Fatalf("ordinary run retained refs = %+v", got.Retained)
	}

	got = fromJournal["reliability-ci-exhausted"]
	assertBudget(t, "exhausted", budgetByKind(t, got, "provider-remediation"), intPtr(2), intPtr(0), "terminalCause")
	assertBudget(t, "exhausted", budgetByKind(t, got, "implementation-review"), intPtr(0), intPtr(5), "pinnedDefinition")
	assertBudget(t, "exhausted", budgetByKind(t, got, "ci-poll"), intPtr(0), intPtr(6), "pinnedDefinition")
	if got.Retained.Branch != "goobers/5313" || !strings.Contains(got.StatusLine(), "provider-remediation 2 used/0 left") {
		t.Fatalf("exhausted reliability = %s", got.StatusLine())
	}
}
