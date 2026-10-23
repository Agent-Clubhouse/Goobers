package escalationnotify

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// writeTerminalJournal records a run that ended with events after its
// identity, in a real journal under runsDir.
func writeTerminalJournal(t *testing.T, runsDir, runID string, events ...journal.Event) {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: "acme-web"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	for _, event := range events {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

func retryDecision(class journal.AttemptClass) journal.Event {
	return journal.Event{Type: journal.EventRunnerAnnotation, Stage: "local-ci", Gate: "local-gate", Runner: map[string]any{
		"kind": runsemantics.RetryDecisionKind, runsemantics.RetryFailureClassKey: string(class), "failureCode": telemetry.ErrCodeTimeout, "target": "local-ci",
	}}
}

// interruptedRetry is a local-ci timeout local-gate classified retryClass and
// routed back to local-ci, followed by the given terminal events.
func interruptedRetry(retryClass journal.AttemptClass, terminal ...journal.Event) []journal.Event {
	return append([]journal.Event{
		{Type: journal.EventStageStarted, Stage: "local-ci", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "local-ci", Attempt: 1, Status: "failure", Error: &journal.ErrorDetail{Code: telemetry.ErrCodeTimeout, Message: "local-ci exceeded its wall-clock limit"}},
		{Type: journal.EventGateStarted, Gate: "local-gate"},
		{Type: journal.EventGateEvaluated, Gate: "local-gate", Verdict: "infra", Target: "local-ci"},
		retryDecision(retryClass),
		{Type: journal.EventStageStarted, Stage: "local-ci", Attempt: 2},
	}, terminal...)
}

func abortedBy(code string, classification journal.TerminalClassification) []journal.Event {
	return []journal.Event{
		{Type: journal.EventError, Error: &journal.ErrorDetail{Code: code, Message: "interrupted"}},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted), TerminalCause: &journal.TerminalCause{
			Schema: journal.TerminalCauseSchema, Phase: journal.PhaseAborted, Classification: classification,
			SelectorKind: "condition", Code: code,
		}},
	}
}

// expiredDuringRetry is #5638's second reproduction as the local runner
// journals it: the run expired during local-gate's retry of local-ci.
func expiredDuringRetry(retryClass journal.AttemptClass) []journal.Event {
	return interruptedRetry(retryClass, abortedBy(runsemantics.RunDurationExceededErrorCode, journal.TerminalPolicyExhaustion)...)
}

func lastComment(poster *fakePoster) string {
	for i := len(poster.updates) - 1; i >= 0; i-- {
		if poster.updates[i].Comment != "" {
			return poster.updates[i].Comment
		}
	}
	return ""
}

// TestAbortedTerminalHonorsGateInfraClassification pins #5638's second
// reproduction at the breaker: an aborted terminal the journal attributes to
// the gate's infrastructure retry is no genuine/work strike. It counts on the
// infrastructure streak, whose comment says so, and refunds a remediation
// charge exactly as an infra failed terminal does.
func TestAbortedTerminalHonorsGateInfraClassification(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	writeTerminalJournal(t, p.RunsDir, "run-infra", expiredDuringRetry(journal.AttemptInfra)...)
	state.items["run-infra"] = []Item{{ItemID: "3271", Repo: webRepo}}

	if err := p.TerminalNotifier(nil)("run-infra", journal.PhaseAborted, "local-ci"); err != nil {
		t.Fatalf("aborted: %v", err)
	}
	if state.streaks["web#3271"] != 0 || state.infraStreaks["web#3271"] != 1 || len(poster.labelUpdates()) != 0 {
		t.Fatalf("work=%v infra=%v labels=%+v; want one infrastructure strike and no park", state.streaks, state.infraStreaks, poster.labelUpdates())
	}
	body := lastComment(poster)
	if !strings.Contains(body, "Classification: **infrastructure failure**") || strings.Contains(body, "genuine/work failure**") {
		t.Fatalf("comment = %q, want the infrastructure classification", body)
	}
	if !slices.Equal(state.voided, []string{"run-infra"}) {
		t.Fatalf("voided = %v, want the run's remediation charge refunded", state.voided)
	}
}

// The same expiry after a work-classified retry is still a work strike, and
// the item gets the genuine/work-failure comment.
func TestAbortedTerminalWithoutInfraClassificationStillCounts(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	writeTerminalJournal(t, p.RunsDir, "run-work", expiredDuringRetry(journal.AttemptPolicy)...)
	state.items["run-work"] = []Item{{ItemID: "3271", Repo: webRepo}}

	if err := p.TerminalNotifier(nil)("run-work", journal.PhaseAborted, "local-ci"); err != nil {
		t.Fatalf("aborted: %v", err)
	}
	if state.streaks["web#3271"] != 1 || state.infraStreaks["web#3271"] != 0 || len(state.voided) != 0 {
		t.Fatalf("work=%v infra=%v voided=%v, want one work strike and no refund", state.streaks, state.infraStreaks, state.voided)
	}
	if !strings.Contains(lastComment(poster), "genuine/work failure") {
		t.Fatalf("updates = %+v, want the failure-streak comment", poster.updates)
	}
}

// An item whose CI hangs every time escalates on an exhausted
// infrastructure repass budget run after run. Those terminals stay off the
// work streak, but the separate infrastructure streak still parks the item at
// its own threshold, with an accurate classification — the loop is bounded.
func TestAlwaysHangingCIParksOnTheInfraStreak(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	state.items["run-hang"] = []Item{{ItemID: "3271", Repo: webRepo}}
	writeTerminalJournal(t, p.RunsDir, "run-hang", journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated), TerminalCause: &journal.TerminalCause{
			Schema: journal.TerminalCauseSchema, Phase: journal.PhaseEscalated, Classification: journal.TerminalInfrastructureFailure,
			SelectorKind: "gate", Selector: "local-gate", Code: gate.ReasonInfrastructureBudgetExhausted,
		},
	})
	notify := p.TerminalNotifier(nil)
	for i := 1; i <= InfraFailureStreakThreshold; i++ {
		if err := notify("run-hang", journal.PhaseEscalated, "park-escalated"); err != nil {
			t.Fatalf("escalated %d: %v", i, err)
		}
		if parked := len(poster.labelUpdates()) > 0; parked != (i == InfraFailureStreakThreshold) {
			t.Fatalf("after %d infrastructure terminals parked=%v, want a park only at %d", i, parked, InfraFailureStreakThreshold)
		}
	}
	if state.streaks["web#3271"] != 0 || state.infraStreaks["web#3271"] != InfraFailureStreakThreshold {
		t.Fatalf("work=%v infra=%v", state.streaks, state.infraStreaks)
	}
	park := poster.labelUpdates()[0]
	if !slices.Equal(park.AddLabels, []string{providers.LabelNeedsHuman}) || !slices.Equal(park.RemoveLabels, []string{providers.LabelReady}) {
		t.Fatalf("park = %+v", park)
	}
	body := lastComment(poster)
	if !strings.Contains(body, "**6 consecutive infrastructure-classified terminal(s)**") ||
		!strings.Contains(body, "Classification: **infrastructure failure**") || !strings.Contains(body, providers.LabelNeedsHuman) {
		t.Fatalf("park comment = %q", body)
	}

	// A completed run resets the infrastructure streak too.
	if err := notify("run-hang", journal.PhaseCompleted, "done"); err != nil {
		t.Fatalf("completed: %v", err)
	}
	if state.infraStreaks["web#3271"] != 0 {
		t.Fatalf("infra streak after completion = %d, want 0", state.infraStreaks["web#3271"])
	}
}

// The Temporal driver journals every cancellation as run_canceled. The stall
// sweep's own record of why it cancelled is what tells a watchdog stop of an
// infrastructure retry from an operator's cancel.
func TestEngineCancelledRunHonorsTheSweepsInterruption(t *testing.T) {
	cancelled := interruptedRetry(journal.AttemptInfra, journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "run_failed", Message: "run canceled on the engine"}},
		journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted), TerminalCause: &journal.TerminalCause{
			Schema: journal.TerminalCauseSchema, Phase: journal.PhaseAborted, Classification: journal.TerminalOperatorAbort,
			SelectorKind: "condition", Code: runsemantics.RunCanceledErrorCode,
		}})
	for name, tc := range map[string]struct {
		interruption string
		work, infra  int
	}{
		"sweep expiry":    {runsemantics.RunDurationExceededErrorCode, 0, 1},
		"sweep stall":     {runsemantics.RunStalledErrorCode, 0, 1},
		"operator cancel": {"", 1, 0},
	} {
		t.Run(name, func(t *testing.T) {
			p, _, state := newTestPolicy(t)
			writeTerminalJournal(t, p.RunsDir, "run-engine", cancelled...)
			state.items["run-engine"] = []Item{{ItemID: "3271", Repo: webRepo}}
			p.EngineInterruption = func(runID string) (string, error) {
				if runID != "run-engine" {
					t.Fatalf("interruption asked for %q", runID)
				}
				return tc.interruption, nil
			}
			if err := p.TerminalNotifier(nil)("run-engine", journal.PhaseAborted, "local-ci"); err != nil {
				t.Fatalf("aborted: %v", err)
			}
			if state.streaks["web#3271"] != tc.work || state.infraStreaks["web#3271"] != tc.infra {
				t.Fatalf("work=%v infra=%v, want %d/%d", state.streaks, state.infraStreaks, tc.work, tc.infra)
			}
		})
	}
}

// An escalation on an exhausted infrastructure repass budget carries its
// classification on the terminal cause itself.
func TestEscalatedInfraBudgetTerminalIsNotAWorkStrike(t *testing.T) {
	p, _, state := newTestPolicy(t)
	writeTerminalJournal(t, p.RunsDir, "run-budget", journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated), TerminalCause: &journal.TerminalCause{
			Schema: journal.TerminalCauseSchema, Phase: journal.PhaseEscalated, Classification: journal.TerminalInfrastructureFailure,
			SelectorKind: "gate", Selector: "local-gate", Code: gate.ReasonInfrastructureBudgetExhausted,
		},
	})
	state.items["run-budget"] = []Item{{ItemID: "3271", Repo: webRepo}}
	if err := p.TerminalNotifier(nil)("run-budget", journal.PhaseEscalated, "park-escalated"); err != nil {
		t.Fatalf("escalated: %v", err)
	}
	if state.streaks["web#3271"] != 0 || state.infraStreaks["web#3271"] != 1 {
		t.Fatalf("work=%v infra=%v, want an infrastructure strike only", state.streaks, state.infraStreaks)
	}
}

// TestHarnessStartupInfraNeverAdvancesEitherStreak is #5638's original
// complaint: a harness that never gave the agent a turn (the required-MCP
// control process exiting before readiness) parked six good issues. That is
// substrate, not about the item, so however many times it repeats — past
// both thresholds — it charges neither streak and parks nothing.
func TestHarnessStartupInfraNeverAdvancesEitherStreak(t *testing.T) {
	for name, outcome := range map[string]runner.FailedOutcome{
		"runner fault class": {Code: telemetry.ErrCodeExecutor, FaultClass: telemetry.ErrorClassInfra},
		"substrate code":     {Code: telemetry.ErrCodeHarnessRequiredMCPUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			p, poster, state := newTestPolicy(t)
			state.items["run-startup"] = []Item{{ItemID: "5629", Repo: webRepo}}
			outcome.RunID, outcome.RepoRef, outcome.Stage = "run-startup", webRepoRef, "curate"
			for i := 0; i < 2*InfraFailureStreakThreshold; i++ {
				if err := p.Failed(context.Background(), outcome); err != nil {
					t.Fatalf("Failed %d: %v", i+1, err)
				}
			}
			if state.streaks["web#5629"] != 0 || state.infraStreaks["web#5629"] != 0 || len(poster.updates) != 0 {
				t.Fatalf("work=%v infra=%v updates=%+v; a harness startup failure must not touch the item", state.streaks, state.infraStreaks, poster.updates)
			}
		})
	}
}

// Infrastructure that interrupted a retry of something other than the item's
// own validation is substrate on the escalated/aborted path too: an expiry
// during a dispatch's infrastructure retry (a clone that kept failing), or an
// engine workflow the daemon terminated because no worker polled it.
func TestSubstrateTerminalsChargeNeitherStreak(t *testing.T) {
	dispatchRetry := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventError, Stage: "implement", Error: &journal.ErrorDetail{Code: "executor_error", Message: "clone failed"},
			Runner: map[string]any{runsemantics.RetryFailureClassKey: string(journal.AttemptInfra)}},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2},
	}
	for name, events := range map[string][]journal.Event{
		"expiry during dispatch retry": append(dispatchRetry, abortedBy(runsemantics.RunDurationExceededErrorCode, journal.TerminalPolicyExhaustion)...),
		"terminated engine workflow": {{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted), TerminalCause: &journal.TerminalCause{
			Schema: journal.TerminalCauseSchema, Phase: journal.PhaseAborted, Classification: journal.TerminalInfrastructureFailure,
			SelectorKind: "condition", Code: "terminated",
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			p, poster, state := newTestPolicy(t)
			writeTerminalJournal(t, p.RunsDir, "run-substrate", events...)
			state.items["run-substrate"] = []Item{{ItemID: "42", Repo: webRepo}}
			for i := 0; i < 2*InfraFailureStreakThreshold; i++ {
				if err := p.TerminalNotifier(nil)("run-substrate", journal.PhaseAborted, "implement"); err != nil {
					t.Fatalf("aborted %d: %v", i+1, err)
				}
			}
			if state.streaks["web#42"] != 0 || state.infraStreaks["web#42"] != 0 || len(poster.updates) != 0 {
				t.Fatalf("work=%v infra=%v updates=%+v, want neither streak touched", state.streaks, state.infraStreaks, poster.updates)
			}
			if len(state.voided) == 0 {
				t.Fatal("a substrate terminal still refunds the run's remediation charge")
			}
		})
	}
}

// A journal that exists but cannot be read leaves the terminal charged as
// work, as before, and reports why it could not be classified.
func TestUnreadableTerminalJournalStillCountsAndReports(t *testing.T) {
	p, _, state := newTestPolicy(t)
	if err := os.MkdirAll(filepath.Join(p.RunsDir, "run-broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.RunsDir, "run-broken", "run.yaml"), []byte("::: not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	state.items["run-broken"] = []Item{{ItemID: "3271", Repo: webRepo}}
	err := p.TerminalNotifier(nil)("run-broken", journal.PhaseAborted, "local-ci")
	if err == nil || !strings.Contains(err.Error(), "read terminal classification") {
		t.Fatalf("err = %v, want the classification read failure reported", err)
	}
	if state.streaks["web#3271"] != 1 {
		t.Fatalf("streaks = %v, want the unclassified terminal charged", state.streaks)
	}
}

// localCI is local-ci on a host that slept: it times out once, and the
// retried attempt runs until the run is interrupted.
type localCI struct {
	mu      sync.Mutex
	calls   int
	running chan struct{}
}

func (l *localCI) Run(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	l.mu.Lock()
	l.calls++
	first := l.calls == 1
	l.mu.Unlock()
	if first {
		return apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{
			Code: telemetry.ErrCodeTimeout, Message: "local-ci exceeded its wall-clock limit", Retryable: true,
		}}, nil
	}
	close(l.running)
	<-ctx.Done()
	return apiv1.ResultEnvelope{}, context.Cause(ctx)
}

// TestRealRunnerJournalReachesTheInfraStreak drives the reproduction through
// the real local runner and hands its aborted terminal to the real breaker,
// so a change to the order the runner journals the gate's decision cannot
// pass one layer and fail the other.
func TestRealRunnerJournalReachesTheInfraStreak(t *testing.T) {
	const runID = "run-local-ci-suspend-5638"
	runsDir := filepath.Join(t.TempDir(), "runs")
	state := newFakeState()
	state.items[runID] = []Item{{ItemID: "3271", Repo: webRepo}}
	poster := &fakePoster{}
	p := &Policy{Poster: poster, RunsDir: runsDir, State: state}
	ci := &localCI{running: make(chan struct{})}
	worktrees, err := worktree.NewManager(filepath.Join(filepath.Dir(runsDir), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{
		Worktrees:        worktrees,
		ScratchDir:       filepath.Join(filepath.Dir(runsDir), "scratch"),
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) { return ci, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		RunsDir:          runsDir,
		Failed:           p.Failed,
		NotifyTerminal:   p.TerminalNotifier(nil),
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	spec := apiv1.WorkflowSpec{
		Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "local-ci",
		Tasks: []apiv1.Task{{
			Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "run the local validation suite",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "local-gate",
		}},
		Gates: []apiv1.Gate{{
			Name: "local-gate", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "failure-class"},
			Branches: map[string]string{gate.OutcomePass: workflow.TerminalComplete, gate.OutcomeInfra: "local-ci", gate.OutcomeFail: workflow.TargetEscalate},
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "local-gate-5638", Version: 1, Spec: spec},
		workflow.WithKnownChecks([]string{"failure-class"}), workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.Start(context.Background(), runner.StartInput{
			RunID: runID, Machine: machine, Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- err
	}()
	select {
	case <-ci.running:
	case err := <-done:
		t.Fatalf("Start returned before local-gate's retry reached local-ci: %v", err)
	}
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
	if state.streaks["web#3271"] != 0 || state.infraStreaks["web#3271"] != 1 {
		t.Fatalf("work=%v infra=%v; the gate already judged this retry infrastructure", state.streaks, state.infraStreaks)
	}
	if body := lastComment(poster); !strings.Contains(body, "Classification: **infrastructure failure**") {
		t.Fatalf("comment = %q, want the infrastructure classification", body)
	}
}
