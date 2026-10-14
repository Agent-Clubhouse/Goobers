package runner

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
)

// localGateMachine is the shipped implementation workflow's local-ci ->
// local-gate shape: the gate's failure-class check sends a retryable failure
// down its infra branch back to local-ci.
func localGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "local-ci",
		Tasks: []apiv1.Task{{
			Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "run the local validation suite",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "local-gate",
		}},
		Gates: []apiv1.Gate{{
			Name: "local-gate", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "failure-class"},
			Branches: map[string]string{
				gate.OutcomePass:  workflow.TerminalComplete,
				gate.OutcomeInfra: "local-ci",
				gate.OutcomeFail:  workflow.TargetEscalate,
			},
		}},
	}
	m, err := workflow.Compile(
		workflow.Definition{Name: "local-gate-5638", Version: 1, Spec: spec},
		workflow.WithKnownChecks([]string{"failure-class"}),
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile local-gate machine: %v", err)
	}
	return m
}

// suspendedLocalCI is local-ci on a host that slept: the configured results
// come back in order, and once they run out the next attempt runs until the
// run is interrupted.
type suspendedLocalCI struct {
	mu      sync.Mutex
	results []apiv1.ResultEnvelope
	running chan struct{}
}

func (s *suspendedLocalCI) Run(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	s.mu.Lock()
	if len(s.results) > 0 {
		result := s.results[0]
		s.results = s.results[1:]
		s.mu.Unlock()
		return result, nil
	}
	s.mu.Unlock()
	close(s.running)
	<-ctx.Done()
	return apiv1.ResultEnvelope{}, context.Cause(ctx)
}

// expireDuringLocalCI starts runID against localGateMachine, waits until
// local-ci is running with no results left to return, expires the run, and
// returns its journal. Failed and NotifyTerminal record what they saw.
func expireDuringLocalCI(t *testing.T, runID string, results []apiv1.ResultEnvelope) (events []journal.Event, failedCalls int, notified []journal.RunPhase) {
	t.Helper()
	localCI := &suspendedLocalCI{results: results, running: make(chan struct{})}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return localCI, nil
	}, gate.NewAutomatedEvaluator())
	var mu sync.Mutex
	r.cfg.Failed = func(context.Context, FailedOutcome) error {
		mu.Lock()
		defer mu.Unlock()
		failedCalls++
		return nil
	}
	r.cfg.NotifyTerminal = func(_ string, phase journal.RunPhase, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		notified = append(notified, phase)
		return nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := r.Start(context.Background(), StartInput{
			RunID: runID, Machine: localGateMachine(t), Gaggle: "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- err
	}()
	<-localCI.running

	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	result, expired, err := r.ExpireRun(runID, identity.StartedAt.Add(7*time.Hour), identity.StartedAt, 6*time.Hour+30*time.Minute)
	if err != nil || !expired || result.Phase != journal.PhaseAborted {
		t.Fatalf("ExpireRun = %+v, %v, %v; want aborted", result, expired, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Start: %v", err)
	}
	events, err = reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return events, failedCalls, notified
}

// TestGateInfraTimeoutThenExpiryIsAnInfraTerminal is #5638's second
// reproduction, traced. local-ci timed out across a host suspension; the
// failure-class gate judged it infra and routed back to local-ci; the run then
// expired during that infrastructure retry. That terminal never reaches the
// Failed handler — the expiry interrupt is intercepted first and the run ends
// aborted — but the aborted terminal is what the instance's circuit breaker
// charges through NotifyTerminal. The journal it reads must say infra.
func TestGateInfraTimeoutThenExpiryIsAnInfraTerminal(t *testing.T) {
	events, failedCalls, notified := expireDuringLocalCI(t, "run-local-ci-suspend-5638", []apiv1.ResultEnvelope{{
		Status: apiv1.ResultFailure,
		Error:  &apiv1.ErrorInfo{Code: telemetry.ErrCodeTimeout, Message: "local-ci exceeded its wall-clock limit", Retryable: true},
	}})

	var decision journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == RetryDecisionKind {
			decision = event
		}
	}
	if decision.Gate != "local-gate" || decision.Runner[RetryFailureClassKey] != string(journal.AttemptInfra) ||
		decision.Runner["failureCode"] != telemetry.ErrCodeTimeout || decision.Runner["target"] != "local-ci" {
		t.Fatalf("precondition: want local-gate's infra retry of a local-ci timeout, got %+v", decision.Runner)
	}
	if failedCalls != 0 || len(notified) != 1 || notified[0] != journal.PhaseAborted {
		t.Fatalf("Failed calls = %d, NotifyTerminal phases = %v; want the aborted terminal notifier only", failedCalls, notified)
	}
	if class := JournaledTerminalFault(events, ""); class != TerminalFaultItemInfrastructure {
		t.Fatalf("terminal class = %q, want %q: the gate already judged this retry infrastructure", class, TerminalFaultItemInfrastructure)
	}
}

// An expiry while the stage is doing its ordinary first attempt has no
// infrastructure classification behind it and stays a charged terminal.
func TestExpiryDuringOrdinaryWorkIsUnclassified(t *testing.T) {
	events, _, notified := expireDuringLocalCI(t, "run-local-ci-expiry-5638", nil)
	if len(notified) != 1 || notified[0] != journal.PhaseAborted {
		t.Fatalf("NotifyTerminal phases = %v, want aborted", notified)
	}
	if class := JournaledTerminalFault(events, ""); class != "" {
		t.Fatalf("terminal class = %q, want unclassified", class)
	}
}

func TestJournaledTerminalFault(t *testing.T) {
	finished := func(phase journal.RunPhase, classification journal.TerminalClassification, code string) journal.Event {
		return journal.Event{Type: journal.EventRunFinished, Status: string(phase), TerminalCause: &journal.TerminalCause{
			Schema: journal.TerminalCauseSchema, Phase: phase, Classification: classification, Code: code,
		}}
	}
	interrupt := func(code string) journal.Event {
		return journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: code}}
	}
	retry := func(class journal.AttemptClass) journal.Event {
		return journal.Event{Type: journal.EventRunnerAnnotation, Gate: "local-gate", Runner: map[string]any{"kind": RetryDecisionKind, RetryFailureClassKey: string(class)}}
	}
	dispatchFailure := func(class journal.AttemptClass) journal.Event {
		return journal.Event{Type: journal.EventError, Stage: "local-ci", Error: &journal.ErrorDetail{Code: "executor_error"}, Runner: map[string]any{RetryFailureClassKey: string(class)}}
	}
	started := journal.Event{Type: journal.EventStageStarted, Stage: "local-ci"}
	stageDone := journal.Event{Type: journal.EventStageFinished, Stage: "local-ci", Status: string(apiv1.ResultSuccess)}

	for name, tc := range map[string]struct {
		events []journal.Event
		want   TerminalFault
	}{
		"infra repass budget escalation":    {[]journal.Event{finished(journal.PhaseEscalated, journal.TerminalInfrastructureFailure, gate.ReasonInfrastructureBudgetExhausted)}, TerminalFaultItemInfrastructure},
		"expiry during gate infra retry":    {[]journal.Event{retry(journal.AttemptInfra), started, interrupt(RunDurationExceededErrorCode), finished(journal.PhaseAborted, journal.TerminalPolicyExhaustion, RunDurationExceededErrorCode)}, TerminalFaultItemInfrastructure},
		"stall during dispatch infra retry": {[]journal.Event{started, dispatchFailure(journal.AttemptInfra), started, interrupt(RunStalledErrorCode), finished(journal.PhaseEscalated, journal.TerminalEscalation, RunStalledErrorCode)}, TerminalFaultSubstrate},
		// An engine workflow the daemon terminated because no worker polled it.
		"terminated engine workflow":       {[]journal.Event{finished(journal.PhaseAborted, journal.TerminalInfrastructureFailure, "terminated")}, TerminalFaultSubstrate},
		"expiry after the retry succeeded": {[]journal.Event{retry(journal.AttemptInfra), started, stageDone, started, interrupt(RunDurationExceededErrorCode), finished(journal.PhaseAborted, journal.TerminalPolicyExhaustion, RunDurationExceededErrorCode)}, ""},
		"expiry during a policy retry":     {[]journal.Event{started, dispatchFailure(journal.AttemptPolicy), started, interrupt(RunDurationExceededErrorCode), finished(journal.PhaseAborted, journal.TerminalPolicyExhaustion, RunDurationExceededErrorCode)}, ""},
		// An operator's cancel is not a watchdog judgment about the run.
		"cancel during infra retry": {[]journal.Event{retry(journal.AttemptInfra), started, interrupt(RunCanceledErrorCode), finished(journal.PhaseAborted, journal.TerminalOperatorAbort, RunCanceledErrorCode)}, ""},
		"defined abort":             {[]journal.Event{finished(journal.PhaseAborted, journal.TerminalDefinedAbort, "defined_abort")}, ""},
		// A failed cause defaults to infrastructure-failure whatever failed;
		// FailedOutcome.FaultClass owns failed terminals.
		"failed terminal":   {[]journal.Event{finished(journal.PhaseFailed, journal.TerminalInfrastructureFailure, "run_failed")}, ""},
		"no terminal":       {[]journal.Event{retry(journal.AttemptInfra)}, ""},
		"legacy no cause":   {[]journal.Event{{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted)}}, ""},
		"only latest cause": {[]journal.Event{finished(journal.PhaseEscalated, journal.TerminalInfrastructureFailure, gate.ReasonInfrastructureBudgetExhausted), finished(journal.PhaseEscalated, journal.TerminalEscalation, "escalated")}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := JournaledTerminalFault(tc.events, ""); got != tc.want {
				t.Fatalf("class = %q, want %q", got, tc.want)
			}
		})
	}
}

// When local-ci keeps timing out, the gate's infrastructure repass budget runs
// out and the run escalates. The terminal cause already says
// infrastructure-failure; the breaker reads it from the same journal.
func TestGateInfraBudgetEscalationIsAnInfraTerminal(t *testing.T) {
	const runID = "run-local-ci-infra-budget-5638"
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{runID + ":local-ci": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: telemetry.ErrCodeTimeout, Message: "local-ci exceeded its wall-clock limit", Retryable: true},
		}}}, nil
	}, gate.NewAutomatedEvaluator())
	var failedCalls int
	var notified []journal.RunPhase
	r.cfg.Failed = func(context.Context, FailedOutcome) error { failedCalls++; return nil }
	r.cfg.NotifyTerminal = func(_ string, phase journal.RunPhase, _ string) error {
		notified = append(notified, phase)
		return nil
	}
	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: localGateMachine(t), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil || res.Phase != journal.PhaseEscalated {
		t.Fatalf("Start = %+v, %v; want escalated on the exhausted infrastructure budget", res, err)
	}
	if failedCalls != 0 || len(notified) != 1 || notified[0] != journal.PhaseEscalated {
		t.Fatalf("Failed calls = %d, NotifyTerminal phases = %v; want the escalated terminal notifier only", failedCalls, notified)
	}
	if class := JournaledTerminalFault(readRunEvents(t, runsDir, runID), ""); class != TerminalFaultItemInfrastructure {
		t.Fatalf("terminal class = %q, want %q", class, TerminalFaultItemInfrastructure)
	}
}
