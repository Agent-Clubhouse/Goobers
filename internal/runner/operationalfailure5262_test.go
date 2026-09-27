package runner

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// remediationGateMachine is #5262's shape: a stage whose Next is a remediation
// gate that classifies the failure and routes it — the generic workflow the
// issue describes. The gate uses the shipped `failure-class` check, which
// already distinguishes a retryable failure (the `infra` branch, an
// infrastructure retry) from any other failure (`fail`, which escalates here).
//
// That check is the existing consumer the reclassification is designed for:
// #5262 asks that the original error be preserved and that the EXISTING gate
// choose repair, infrastructure retry or escalation. Nothing here is new
// machinery — the only thing that was missing was the operational failure ever
// reaching this gate at all.
func remediationGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "remediate",
			},
			{
				Name: "repair", Type: apiv1.TaskDeterministic, Goal: "repair the environment",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: workflow.TerminalComplete,
			},
		},
		Gates: []apiv1.Gate{{
			Name: "remediate", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "failure-class"},
			Branches: map[string]string{
				gate.OutcomePass:  workflow.TerminalComplete,
				gate.OutcomeInfra: "repair",
				gate.OutcomeFail:  workflow.TargetEscalate,
			},
		}},
	}
	m, err := workflow.Compile(
		workflow.Definition{Name: "remediation-gate-5262", Version: 1, Spec: spec},
		workflow.WithKnownChecks([]string{"failure-class"}),
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile remediation-gate machine: %v", err)
	}
	return m
}

// TestOperationalFailureReachesRemediationGate is #5262's runner-side
// acceptance. The harness has reclassified the producer's blocked report into a
// retryable failure carrying the producer's own DEPENDENCY_RESTORE_FAILED code
// (internal/harness.reclassifyOperationalFailureBlock); this proves what the
// runner then does with it — evaluate the declared gate, take its `infra`
// branch into the repair task, and complete.
//
// Before the reclassification the identical situation ended the run at
// PhaseEscalated without evaluating this gate at all, needs-human-parking every
// item the run had claimed for an environment fault none of those items caused.
//
// Stubbing the already-reclassified envelope is the same seam
// TestCapabilityUnsatisfiedEndsRunFailedNotEscalated (#2197) uses: the harness
// unit tests own the conversion, and this owns the transition. Because both the
// local runner and the Temporal engine consume the envelope the harness returns,
// the transition proven here is the one both substrates apply.
func TestOperationalFailureReachesRemediationGate(t *testing.T) {
	machine := remediationGateMachine(t)
	byTask := map[string]stubTaskResult{
		"run-operational-5262:implement": {
			status: apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{
				Code:      "DEPENDENCY_RESTORE_FAILED",
				Message:   "go mod download: dial tcp: i/o timeout",
				Retryable: true,
			},
			outputs: map[string]interface{}{"operationalFailure": true, "reclassifiedFromBlocked": true},
		},
		// The remediation branch's own stage. Its presence in this map is not
		// incidental: reaching it at all is what the test is proving.
		"run-operational-5262:repair": {status: apiv1.ResultSuccess, summary: "restored the module cache"},
	}
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: byTask}, nil
	}, gate.NewAutomatedEvaluator())

	var blockedCalls int
	r.cfg.Blocked = func(context.Context, BlockedOutcome) error {
		blockedCalls++
		return nil
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-operational-5262",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed: the gate should have routed to repair", res.Phase)
	}
	if blockedCalls != 0 {
		t.Fatalf("Config.Blocked calls = %d, want 0: an environment fault must not park the claimed items", blockedCalls)
	}

	// The gate must actually have been evaluated, and have reached the infra
	// branch specifically. A completed phase alone would not prove that — a
	// workflow that skipped the gate could also complete.
	events := readRunEvents(t, runsDir, "run-operational-5262")
	var gateVerdict, gateTarget string
	var repairRan bool
	for _, ev := range events {
		if ev.Type == journal.EventGateEvaluated && ev.Gate == "remediate" {
			gateVerdict, gateTarget = ev.Verdict, ev.Target
		}
		if ev.Type == journal.EventStageFinished && ev.Stage == "repair" {
			repairRan = true
		}
	}
	if gateVerdict != gate.OutcomeInfra {
		t.Errorf("gate verdict = %q, want %q (a retryable failure is an infrastructure class)", gateVerdict, gate.OutcomeInfra)
	}
	if gateTarget != "repair" {
		t.Errorf("gate target = %q, want \"repair\"", gateTarget)
	}
	if !repairRan {
		t.Error("the repair task never ran: the remediation branch was not taken")
	}
}

// TestGenuineBlockStillEscalatesPastRemediationGate is the other half of the
// acceptance, on the identical workflow: a genuine dependency block reaches the
// #544 escalated terminal WITHOUT evaluating the gate, exactly as before. This
// is what makes the change narrow — #5262 explicitly rejects blanket
// blocked-to-next routing as incompatible and unsafe, so the blocked terminal
// must stay terminal for every block the harness did not reclassify.
func TestGenuineBlockStillEscalatesPastRemediationGate(t *testing.T) {
	machine := remediationGateMachine(t)
	byTask := map[string]stubTaskResult{
		"run-genuine-block-5262:implement": {
			status:    apiv1.ResultBlocked,
			summary:   "waiting on #441",
			errorInfo: &apiv1.ErrorInfo{Code: "DEPENDENCY_NOT_MET", Message: "issue 441 must merge first"},
		},
	}
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: byTask}, nil
	}, gate.NewAutomatedEvaluator())

	var blockedCalls int
	r.cfg.Blocked = func(context.Context, BlockedOutcome) error {
		blockedCalls++
		return nil
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-genuine-block-5262",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated: a genuine dependency block is still terminal (#544)", res.Phase)
	}
	if blockedCalls != 1 {
		t.Fatalf("Config.Blocked calls = %d, want 1", blockedCalls)
	}

	for _, ev := range readRunEvents(t, runsDir, "run-genuine-block-5262") {
		if ev.Type == journal.EventGateEvaluated && ev.Gate == "remediate" {
			t.Fatal("the remediation gate was evaluated for a genuine block; blocked must stay terminal")
		}
	}
}
