package runner

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
)

func TestEnforceStageBudgetBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		limits      apiv1.Limits
		metrics     map[string]float64
		wantFailure bool
		wantMessage string
	}{
		{
			name:    "unconfigured budget",
			metrics: map[string]float64{},
		},
		{
			name:   "observed zero usage",
			limits: apiv1.Limits{MaxTokens: 100, MaxCostUSD: 1},
			metrics: map[string]float64{
				telemetry.AttrGenAIUsageInputTokens:  0,
				telemetry.AttrGenAIUsageOutputTokens: 0,
				telemetry.AttrUsageCostUSD:           0,
			},
		},
		{
			name:   "tokens below",
			limits: apiv1.Limits{MaxTokens: 100},
			metrics: map[string]float64{
				telemetry.AttrGenAIUsageInputTokens:  40,
				telemetry.AttrGenAIUsageOutputTokens: 59,
			},
		},
		{
			name:   "tokens equal",
			limits: apiv1.Limits{MaxTokens: 100},
			metrics: map[string]float64{
				telemetry.AttrGenAIUsageInputTokens:  40,
				telemetry.AttrGenAIUsageOutputTokens: 60,
			},
		},
		{
			name:   "tokens above",
			limits: apiv1.Limits{MaxTokens: 100},
			metrics: map[string]float64{
				telemetry.AttrGenAIUsageInputTokens:  40,
				telemetry.AttrGenAIUsageOutputTokens: 61,
			},
			wantFailure: true,
			wantMessage: "token usage 101 exceeds maxTokens 100",
		},
		{
			name:   "cost below",
			limits: apiv1.Limits{MaxCostUSD: 1},
			metrics: map[string]float64{
				telemetry.AttrUsageCostUSD: 0.99,
			},
		},
		{
			name:   "cost equal",
			limits: apiv1.Limits{MaxCostUSD: 1},
			metrics: map[string]float64{
				telemetry.AttrUsageCostUSD: 1,
			},
		},
		{
			name:   "cost above",
			limits: apiv1.Limits{MaxCostUSD: 1},
			metrics: map[string]float64{
				telemetry.AttrUsageCostUSD: 1.01,
			},
			wantFailure: true,
			wantMessage: "cost usage $1.01 exceeds maxCostUSD $1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			total := newStageUsageTotals()
			accumulateStageUsage(total, tc.metrics)
			got, exceeded := enforceStageBudget(tc.limits, tc.metrics, total, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Metrics: tc.metrics,
			})
			if !tc.wantFailure {
				if exceeded || got.Status != apiv1.ResultSuccess || got.Error != nil {
					t.Fatalf("result = %+v, want unchanged success", got)
				}
				return
			}
			if !exceeded || got.Status != apiv1.ResultFailure || got.Error == nil {
				t.Fatalf("result = %+v, want budget failure", got)
			}
			if got.Error.Code != budgetExceededErrorCode || got.Error.Retryable {
				t.Fatalf("error = %+v, want non-retryable %q", got.Error, budgetExceededErrorCode)
			}
			if !strings.Contains(got.Error.Message, tc.wantMessage) {
				t.Fatalf("error message = %q, want %q", got.Error.Message, tc.wantMessage)
			}
		})
	}
}

func TestEnforceStageBudgetCumulativeCostBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		attempts    []float64
		wantFailure bool
		wantTotal   float64
	}{
		{name: "below", attempts: []float64{0.1, 0.19}, wantTotal: 0.29},
		{name: "equal", attempts: []float64{0.1, 0.2}, wantTotal: 0.3},
		{name: "above", attempts: []float64{0.1, 0.21}, wantTotal: 0.31, wantFailure: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			total := newStageUsageTotals()
			var got apiv1.ResultEnvelope
			var exceeded bool
			for _, cost := range tc.attempts {
				attempt := map[string]float64{telemetry.AttrUsageCostUSD: cost}
				accumulateStageUsage(total, attempt)
				got, exceeded = enforceStageBudget(
					apiv1.Limits{MaxCostUSD: 0.3},
					attempt,
					total,
					apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
				)
				if exceeded {
					break
				}
			}

			if total.metrics[telemetry.AttrUsageCostUSD] != tc.wantTotal {
				t.Fatalf("cumulative cost = %.17g, want %.17g", total.metrics[telemetry.AttrUsageCostUSD], tc.wantTotal)
			}
			if exceeded != tc.wantFailure {
				t.Fatalf("exceeded = %v, want %v; result = %+v", exceeded, tc.wantFailure, got)
			}
		})
	}
}

func TestEnforceStageBudgetFailsClosedWithoutRequiredUsage(t *testing.T) {
	tests := []struct {
		name        string
		limits      apiv1.Limits
		metrics     map[string]float64
		wantMissing string
	}{
		{
			name:        "token usage absent",
			limits:      apiv1.Limits{MaxTokens: 100},
			wantMissing: telemetry.AttrGenAIUsageInputTokens,
		},
		{
			name:   "token output absent",
			limits: apiv1.Limits{MaxTokens: 100},
			metrics: map[string]float64{
				telemetry.AttrGenAIUsageInputTokens: 40,
			},
			wantMissing: telemetry.AttrGenAIUsageOutputTokens,
		},
		{
			name:        "cost usage absent",
			limits:      apiv1.Limits{MaxCostUSD: 1},
			wantMissing: telemetry.AttrUsageCostUSD,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			total := newStageUsageTotals()
			accumulateStageUsage(total, tc.metrics)
			got, exceeded := enforceStageBudget(tc.limits, tc.metrics, total, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Metrics: tc.metrics,
			})
			if !exceeded || got.Status != apiv1.ResultFailure || got.Error == nil || got.Error.Code != budgetExceededErrorCode {
				t.Fatalf("result = %+v, want %q failure", got, budgetExceededErrorCode)
			}
			if !strings.Contains(got.Error.Message, "missing "+tc.wantMissing) {
				t.Fatalf("error message = %q, want missing metric %q", got.Error.Message, tc.wantMissing)
			}
		})
	}
}

func TestEnforceStageBudgetPreservesAttemptDiagnostics(t *testing.T) {
	transcript := &apiv1.ArtifactPointer{Path: "spans/attempt.jsonl", Digest: "sha256:transcript", Size: 12}
	result := apiv1.ResultEnvelope{
		Status:     apiv1.ResultSuccess,
		Summary:    "partial work completed",
		Outputs:    map[string]interface{}{"partial": "retained"},
		Artifacts:  []apiv1.ArtifactPointer{{Path: "artifacts/partial.txt", Digest: "sha256:artifact", Size: 7}},
		Transcript: transcript,
		Metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens:  70,
			telemetry.AttrGenAIUsageOutputTokens: 31,
			telemetry.AttrUsageCostUSD:           0.25,
		},
	}

	total := newStageUsageTotals()
	accumulateStageUsage(total, result.Metrics)
	got, exceeded := enforceStageBudget(apiv1.Limits{MaxTokens: 100}, result.Metrics, total, result)
	if !exceeded || got.Status != apiv1.ResultFailure || got.Error == nil || got.Error.Code != budgetExceededErrorCode {
		t.Fatalf("result = %+v, want budget failure", got)
	}
	if got.Summary != result.Summary ||
		!reflect.DeepEqual(got.Outputs, result.Outputs) ||
		!reflect.DeepEqual(got.Artifacts, result.Artifacts) ||
		!reflect.DeepEqual(got.Transcript, result.Transcript) ||
		!reflect.DeepEqual(got.Metrics, result.Metrics) {
		t.Fatalf("diagnostics changed:\ngot  %+v\nwant %+v", got, result)
	}
}

type budgetResultGoober struct {
	rec    ArtifactRecorder
	called bool
}

func (g *budgetResultGoober) Invoke(ctx context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.called = true
	ref, err := g.rec.RecordArtifact("partial.txt", []byte("partial"))
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	metrics := map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  70,
		telemetry.AttrGenAIUsageOutputTokens: 31,
	}
	invoke.ReportAgentUsage(ctx, metrics)
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: "attempt completed before usage became observable",
		Outputs: map[string]interface{}{"partial": "retained"},
		Artifacts: []apiv1.ArtifactPointer{{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: "text/plain",
		}},
		Metrics: metrics,
	}, nil
}

func (*budgetResultGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestRunnerAppliesPostAttemptBudgetFailureToJournalAndBranch(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "produce partial output",
			Limits: &apiv1.Limits{MaxTokens: 100},
			Next:   "budget-gate",
		}},
		Gates: []apiv1.Gate{{
			Name:      "budget-gate",
			Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "status-equals"},
			Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": workflow.TargetAbort},
		}},
	}

	machine, err := workflow.Compile(workflow.Definition{Name: "budget-fixture", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	goober := &budgetResultGoober{}
	r, err := New(Config{
		NewAgentic: func(_ string, rec ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
			goober.rec = rec
			return goober, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := r.Start(context.Background(), StartInput{
		RunID:   "run-budget-overage",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !goober.called {
		t.Fatal("agentic attempt was not invoked before budget enforcement")
	}
	if result.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted through the gate failure branch", result.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-budget-overage"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var stageFinished, gateEvaluated *journal.Event
	for i := range events {
		event := &events[i]
		if event.Type == journal.EventStageFinished && event.Stage == "implement" {
			stageFinished = event
		}
		if event.Type == journal.EventGateEvaluated && event.Gate == "budget-gate" {
			gateEvaluated = event
		}
	}

	if stageFinished == nil {
		t.Fatal("missing implement stage.finished event")
	}
	if stageFinished.Status != string(apiv1.ResultFailure) ||
		stageFinished.Error == nil ||
		stageFinished.Error.Code != budgetExceededErrorCode {
		t.Fatalf("stage.finished = %+v, want budget failure", *stageFinished)
	}
	if stageFinished.Outputs["partial"] != "retained" || len(stageFinished.Artifacts) != 1 {
		t.Fatalf("stage.finished diagnostics = outputs:%v artifacts:%v, want retained partial output", stageFinished.Outputs, stageFinished.Artifacts)
	}
	if gateEvaluated == nil ||
		gateEvaluated.Verdict != gate.OutcomeFail ||
		gateEvaluated.Target != workflow.TargetAbort {
		t.Fatalf("gate.evaluated = %+v, want fail branch to @abort", gateEvaluated)
	}
}

type retryBudgetGoober struct {
	usages []map[string]float64
	calls  int
}

func (g *retryBudgetGoober) Invoke(ctx context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	usage := g.usages[g.calls]
	g.calls++
	invoke.ReportAgentUsage(ctx, usage)
	return apiv1.ResultEnvelope{
		Summary: "partial diagnostics from failed attempt",
		Outputs: map[string]interface{}{"attempt": float64(g.calls)},
		Metrics: usage,
	}, errors.New("agent attempt failed")
}

func (*retryBudgetGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestRunnerAccumulatesBudgetAcrossErroredAttempts(t *testing.T) {
	tests := []struct {
		name        string
		limits      apiv1.Limits
		usages      []map[string]float64
		wantMessage string
	}{
		{
			name:   "tokens",
			limits: apiv1.Limits{MaxTokens: 100},
			usages: []map[string]float64{
				{telemetry.AttrGenAIUsageInputTokens: 40, telemetry.AttrGenAIUsageOutputTokens: 20},
				{telemetry.AttrGenAIUsageInputTokens: 40, telemetry.AttrGenAIUsageOutputTokens: 20},
			},
			wantMessage: "token usage 120 exceeds maxTokens 100",
		},
		{
			name:   "cost",
			limits: apiv1.Limits{MaxCostUSD: 1},
			usages: []map[string]float64{
				{telemetry.AttrUsageCostUSD: 0.6},
				{telemetry.AttrUsageCostUSD: 0.6},
			},
			wantMessage: "cost usage $1.2 exceeds maxCostUSD $1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			machine := budgetFixtureMachine(t, tc.limits, 3)
			runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
			goober := &retryBudgetGoober{usages: tc.usages}
			r, err := New(Config{
				NewAgentic: func(_ string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
					return goober, nil
				},
				Automated: gate.NewAutomatedEvaluator(),
				Worktrees: wtMgr,
				RunsDir:   runsDir,
				RepoCloneURL: func(apiv1.RepoRef) (string, error) {
					return fixtureRepo, nil
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			runID := "run-cumulative-" + tc.name
			result, err := r.Start(context.Background(), StartInput{
				RunID:   runID,
				Machine: machine,
				Gaggle:  "acme-web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if result.Phase != journal.PhaseAborted {
				t.Fatalf("phase = %q, want aborted through budget failure branch", result.Phase)
			}
			if goober.calls != 2 {
				t.Fatalf("agent attempts = %d, want 2 before cumulative overage stopped retries", goober.calls)
			}

			events := readRunEvents(t, runsDir, runID)
			var finished *journal.Event
			for i := range events {
				if events[i].Type == journal.EventStageFinished && events[i].Stage == "implement" {
					finished = &events[i]
				}
			}
			if finished == nil || finished.Attempt != 2 || finished.Error == nil ||
				finished.Error.Code != budgetExceededErrorCode ||
				!strings.Contains(finished.Error.Message, tc.wantMessage) {
				t.Fatalf("stage.finished = %+v, want cumulative budget failure on attempt 2", finished)
			}
			if finished.Outputs["attempt"] != float64(2) {
				t.Fatalf("stage.finished outputs = %v, want second-attempt diagnostics", finished.Outputs)
			}
		})
	}
}

type successfulBudgetGoober struct {
	calls int
}

func (g *successfulBudgetGoober) Invoke(ctx context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.calls++
	usage := map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  10,
		telemetry.AttrGenAIUsageOutputTokens: 10,
	}
	invoke.ReportAgentUsage(ctx, usage)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Metrics: usage}, nil
}

func (*successfulBudgetGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestRunnerRetriesPreHarnessFailureWithoutMissingUsageFailure(t *testing.T) {
	machine := budgetFixtureMachine(t, apiv1.Limits{MaxTokens: 100}, 1)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	goober := &successfulBudgetGoober{}
	factoryCalls := 0
	r, err := New(Config{
		NewAgentic: func(_ string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
			factoryCalls++
			if factoryCalls == 1 {
				return nil, invoke.InfrastructureFailure(errors.New("temporary executor construction failure"))
			}
			return goober, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := r.Start(context.Background(), StartInput{
		RunID:   "run-pre-harness-retry",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted || factoryCalls != 2 || goober.calls != 1 {
		t.Fatalf("result = %+v factoryCalls=%d gooberCalls=%d, want one infrastructure retry then success", result, factoryCalls, goober.calls)
	}
}

type forgedBudgetGoober struct {
	calls int
}

func (g *forgedBudgetGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.calls++
	return apiv1.ResultEnvelope{
		Status: apiv1.ResultSuccess,
		Metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens:  0,
			telemetry.AttrGenAIUsageOutputTokens: 0,
		},
	}, nil
}

func (*forgedBudgetGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestRunnerRejectsForgedUsageWhenHarnessUsageIsMissing(t *testing.T) {
	machine := budgetFixtureMachine(t, apiv1.Limits{MaxTokens: 100}, 1)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	goober := &forgedBudgetGoober{}
	r, err := New(Config{
		NewAgentic: func(_ string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := r.Start(context.Background(), StartInput{
		RunID:   "run-forged-usage",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseAborted || goober.calls != 1 {
		t.Fatalf("result = %+v calls=%d, want one attempt routed to abort", result, goober.calls)
	}
	events := readRunEvents(t, runsDir, "run-forged-usage")
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == "implement" {
			if event.Error == nil || event.Error.Code != budgetExceededErrorCode ||
				!strings.Contains(event.Error.Message, "missing "+telemetry.AttrGenAIUsageInputTokens) {
				t.Fatalf("stage.finished = %+v, want missing authoritative usage failure", event)
			}
			return
		}
	}
	t.Fatal("missing implement stage.finished event")
}

func TestRunnerFailsClosedOnInterruptedBudgetedAgenticStage(t *testing.T) {
	machine := budgetFixtureMachine(t, apiv1.Limits{MaxTokens: 100}, 2)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	simulateCrashMidAttempt(t, runsDir, machine, "run-budget-resume", "implement", 1, journal.Trigger{Kind: journal.TriggerManual}, true)
	goober := &forgedBudgetGoober{}
	r, err := New(Config{
		NewAgentic: func(_ string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-budget-resume",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted through the configured failure branch", result.Phase)
	}
	if goober.calls != 0 {
		t.Fatalf("agent calls = %d, want no resumed attempt with unknowable prior usage", goober.calls)
	}

	events := readRunEvents(t, runsDir, "run-budget-resume")
	var finished, evaluated *journal.Event
	for i := range events {
		switch {
		case events[i].Type == journal.EventStageFinished && events[i].Stage == "implement":
			finished = &events[i]
		case events[i].Type == journal.EventGateEvaluated && events[i].Gate == "budget-gate":
			evaluated = &events[i]
		}
	}
	if finished == nil || finished.Error == nil || finished.Error.Code != budgetExceededErrorCode ||
		!strings.Contains(finished.Error.Message, "interrupted attempt usage is unavailable") {
		t.Fatalf("stage.finished = %+v, want interrupted budget failure", finished)
	}
	if isInterruptedAttemptMarker(*finished) {
		t.Fatalf("stage.finished = %+v, budget result must remain replayable on another resume", finished)
	}
	if evaluated == nil || evaluated.Verdict != gate.OutcomeFail || evaluated.Target != workflow.TargetAbort {
		t.Fatalf("gate.evaluated = %+v, want fail branch to @abort", evaluated)
	}
}

func budgetFixtureMachine(t *testing.T, limits apiv1.Limits, maxAttempts int32) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "produce partial output",
			Limits: &limits,
			Retry:  &apiv1.RetryPolicy{MaxAttempts: maxAttempts},
			Next:   "budget-gate",
		}},
		Gates: []apiv1.Gate{{
			Name:      "budget-gate",
			Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "status-equals"},
			Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": workflow.TargetAbort},
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "budget-fixture", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return machine
}

func readRunEvents(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return events
}
