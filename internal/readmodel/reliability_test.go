package readmodel

import (
	"maps"
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func foldReliability(events ...journal.Event) ReliabilityFacts {
	var facts ReliabilityFacts
	for _, event := range events {
		event.Schema = journal.EventSchema
		facts = facts.After(event)
	}
	return facts
}

func TestReliabilityFactsFoldTerminalCauseDraftAndAcceptance(t *testing.T) {
	cause := &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: journal.PhaseEscalated, Code: "review-escalated",
		Repass: &journal.TerminalBudget{Consumed: 2, Allowed: 3}}
	finished := journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated), TerminalCause: cause}

	facts := foldReliability(
		journal.Event{Type: journal.EventArtifactRecorded, Name: AcceptanceMappingArtifact, Ref: &journal.Ref{Digest: "sha256:map"}},
		// JSON round-trips a numeric child-publication prNumber as float64.
		journal.Event{Type: journal.EventStageFinished, Outputs: map[string]any{"prNumber": float64(7), "draft": true}},
		finished,
	)
	if facts.AcceptanceState != AcceptanceStateRecorded || facts.AcceptanceDigest != "sha256:map" {
		t.Fatalf("artifact-only acceptance = %q/%q", facts.AcceptanceState, facts.AcceptanceDigest)
	}
	if facts.PullRequestDraft == nil || *facts.PullRequestDraft != (PullRequestDraft{ID: "7", Draft: true}) {
		t.Fatalf("draft = %+v", facts.PullRequestDraft)
	}
	if facts.TerminalCause == nil || facts.TerminalCause.Code != "review-escalated" || facts.TerminalCause.Repass == cause.Repass {
		t.Fatalf("terminal cause must be a deep copy: %+v", facts.TerminalCause)
	}

	resumed := foldReliability(finished, journal.Event{Type: journal.EventRunResumed, Target: "implement"})
	if resumed.TerminalCause != nil {
		t.Fatalf("resume must clear the previous generation's cause: %+v", resumed.TerminalCause)
	}
	legacy := foldReliability(finished, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)})
	if legacy.TerminalCause != nil {
		t.Fatalf("a later run.finished without a record must not keep an older cause: %+v", legacy.TerminalCause)
	}

	partial := foldReliability(journal.Event{Type: journal.EventStageFinished, Outputs: map[string]any{"prNumber": "9", "draft": "maybe"}})
	if partial.PullRequestDraft != nil {
		t.Fatalf("unparseable draft output must stay unknown: %+v", partial.PullRequestDraft)
	}
}

func TestReliabilityFactsFoldGateBudgetCounters(t *testing.T) {
	charge := func(gate, verdict, target string, runner map[string]any) journal.Event {
		return journal.Event{Type: journal.EventGateEvaluated, Gate: gate, Verdict: verdict, Target: target, Runner: runner}
	}
	before := foldReliability(
		charge("local-gate", "fail", "implement", map[string]any{"repassAttempt": float64(1), "repassTarget": "implement"}),
		charge("review", "needs-changes", "implement", map[string]any{"repassAttempt": float64(2), "repassTarget": "implement"}),
		// Infra repasses, interrupted evaluations, and human overrides charge
		// no policy budget.
		charge("local-gate", "infra", "local-ci", map[string]any{"repassAttempt": float64(1), "repassTarget": "local-ci"}),
		charge("ci-gate", "fail", "remediate-ci", map[string]any{"repassAttempt": float64(1), "repassTarget": "remediate-ci", "interrupted": true}),
		journal.Event{Type: journal.EventGateEvaluated, Gate: "ci-gate", Verdict: "fail", Target: "remediate-ci", Actor: "operator",
			Runner: map[string]any{"repassAttempt": float64(3), "repassTarget": "remediate-ci"}},
		charge("ci-gate", "timeout", "ci-poll", map[string]any{"repassAttempt": float64(0), "pollAttempt": float64(1), "pollTarget": "ci-poll"}),
		charge("ci-gate", "timeout", "ci-poll", map[string]any{"repassAttempt": float64(0), "pollAttempt": float64(2), "pollTarget": "ci-poll"}),
	)
	if want := map[string]int{"implement": 2}; !maps.Equal(before.PolicyRepasses, want) {
		t.Fatalf("policy repasses = %v, want %v", before.PolicyRepasses, want)
	}
	if want := map[string]int{"ci-gate": 2}; !maps.Equal(before.TimeoutPolls, want) {
		t.Fatalf("timeout polls = %v, want %v", before.TimeoutPolls, want)
	}
	// A non-timeout outcome resets the consecutive polling window without
	// mutating the earlier snapshot.
	after := before.After(journal.Event{Schema: journal.EventSchema, Type: journal.EventGateEvaluated, Gate: "ci-gate", Verdict: "fail",
		Target: "remediate-ci", Runner: map[string]any{"repassAttempt": float64(1), "repassTarget": "remediate-ci"}})
	if len(after.TimeoutPolls) != 0 || after.PolicyRepasses["remediate-ci"] != 1 || before.TimeoutPolls["ci-gate"] != 2 || before.PolicyRepasses["remediate-ci"] != 0 {
		t.Fatalf("after fail: polls %v repasses %v; before polls %v repasses %v", after.TimeoutPolls, after.PolicyRepasses, before.TimeoutPolls, before.PolicyRepasses)
	}
}

func TestReliabilityFactsFoldStageRetriesPerPass(t *testing.T) {
	start := func(class journal.AttemptClass) journal.Event {
		return journal.Event{Type: journal.EventStageStarted, Stage: "implement", AttemptClass: class}
	}
	retried := foldReliability(start(""), start(journal.AttemptPolicy), start(journal.AttemptInfra), start(journal.AttemptPolicy))
	if want := (StageRetries{Policy: 2, Infra: 1}); retried.LastStage != "implement" || retried.StageRetries["implement"] != want {
		t.Fatalf("retries = %q %+v, want %+v", retried.LastStage, retried.StageRetries, want)
	}
	// A fresh pass of the stage resets its per-pass retry counters without
	// mutating the earlier snapshot.
	fresh := retried.After(journal.Event{Schema: journal.EventSchema, Type: journal.EventStageStarted, Stage: "implement"})
	if len(fresh.StageRetries) != 0 || retried.StageRetries["implement"].Policy != 2 {
		t.Fatalf("fresh pass = %+v, earlier = %+v", fresh.StageRetries, retried.StageRetries)
	}
	moved := retried.After(journal.Event{Schema: journal.EventSchema, Type: journal.EventStageStarted, Stage: "open-pr", AttemptClass: journal.AttemptHuman})
	if moved.LastStage != "open-pr" || moved.StageRetries["implement"].Policy != 2 || len(moved.StageRetries) != 1 {
		t.Fatalf("other stage = %q %+v", moved.LastStage, moved.StageRetries)
	}
}

func TestReliabilityAllowancesClassifyPinnedGates(t *testing.T) {
	spec := apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "implement", Retry: &apiv1.RetryPolicy{MaxAttempts: 3}},
		{Name: "open-pr"},
	}, Gates: []apiv1.Gate{
		// Approval, escalation, infra, and timeout routes are not review repairs.
		{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Branches: map[string]string{
			"needs-changes": "implement", "approve": "open-pr", "escalate": "@escalate", "infra": "implement"}},
		{Name: "local-gate", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "failure-class"},
			Branches: map[string]string{"pass": "open-pr", "fail": "implement", "infra": "local-ci"}},
		{Name: "ci-gate", Evaluator: apiv1.EvaluatorAutomated, MaxRepasses: 2,
			Automated: &apiv1.AutomatedGate{Check: "ci-status", MaxTimeoutPolls: 30},
			Branches:  map[string]string{"pass": "@complete", "fail": "remediate-ci", "timeout": "ci-poll"}},
	}}
	want := []ReliabilityAllowance{
		{Kind: BudgetStagePolicy, Target: "implement", Allowed: 2},
		{Kind: BudgetLocalInfra, Target: "implement", Allowed: 1},
		{Kind: BudgetStagePolicy, Target: "open-pr", Allowed: 0},
		{Kind: BudgetLocalInfra, Target: "open-pr", Allowed: 1},
		{Kind: BudgetImplementationReview, Gate: "review", Target: "implement", Allowed: 5},
		{Kind: BudgetLocalValidation, Gate: "local-gate", Target: "implement", Allowed: 5},
		{Kind: BudgetProviderRemediation, Gate: "ci-gate", Target: "remediate-ci", Allowed: 2},
		{Kind: BudgetCIPoll, Gate: "ci-gate", Target: "ci-poll", Allowed: 30},
	}
	if got := reliabilityAllowances(spec, 5); !slices.Equal(got, want) {
		t.Fatalf("allowances = %+v, want %+v", got, want)
	}
}
