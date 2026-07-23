package runner

import (
	"context"
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
			got := enforceStageBudget(tc.limits, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Metrics: tc.metrics,
			})
			if !tc.wantFailure {
				if got.Status != apiv1.ResultSuccess || got.Error != nil {
					t.Fatalf("result = %+v, want unchanged success", got)
				}
				return
			}
			if got.Status != apiv1.ResultFailure || got.Error == nil {
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
			got := enforceStageBudget(tc.limits, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Metrics: tc.metrics,
			})
			if got.Status != apiv1.ResultFailure || got.Error == nil || got.Error.Code != budgetExceededErrorCode {
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

	got := enforceStageBudget(apiv1.Limits{MaxTokens: 100}, result)
	if got.Status != apiv1.ResultFailure || got.Error == nil || got.Error.Code != budgetExceededErrorCode {
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

func (g *budgetResultGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.called = true
	ref, err := g.rec.RecordArtifact("partial.txt", []byte("partial"))
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: "attempt completed before usage became observable",
		Outputs: map[string]interface{}{"partial": "retained"},
		Artifacts: []apiv1.ArtifactPointer{{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: "text/plain",
		}},
		Metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens:  70,
			telemetry.AttrGenAIUsageOutputTokens: 31,
		},
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
