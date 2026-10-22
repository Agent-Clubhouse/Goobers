package main

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestSupervisorTerminatesOnTokenCap(t *testing.T) {
	ctx, supervisor := newSupervisedUsageContext(
		context.Background(), apiv1.Limits{MaxTokens: 10},
	)
	harnessStopped := make(chan error, 1)
	go func() {
		<-ctx.Done()
		harnessStopped <- context.Cause(ctx)
	}()

	supervisor.report(map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  8,
		telemetry.AttrGenAIUsageOutputTokens: 3,
	})
	select {
	case cause := <-harnessStopped:
		var exceeded *supervisorBudgetExceededError
		if !errors.As(cause, &exceeded) {
			t.Fatalf("cancellation cause = %T %v, want supervisorBudgetExceededError", cause, cause)
		}
	case <-time.After(time.Second):
		t.Fatal("harness context was not canceled after crossing maxTokens")
	}
	supervisor.report(map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  0,
		telemetry.AttrGenAIUsageOutputTokens: 0,
	})
	metrics, reported := supervisor.snapshot()
	if !reported || metrics[telemetry.AttrGenAIUsageInputTokens] != 8 ||
		metrics[telemetry.AttrGenAIUsageOutputTokens] != 3 {
		t.Fatalf("supervisor snapshot = %v, reported %v", metrics, reported)
	}
}

func TestSupervisorUsageComesOnlyFromCallback(t *testing.T) {
	ctx, supervisor := newSupervisedUsageContext(
		context.Background(), apiv1.Limits{MaxTokens: 1, MaxCostUSD: 0.01},
	)
	modelAuthored := apiv1.ResultEnvelope{Metrics: map[string]float64{
		telemetry.AttrGenAIUsageInputTokens: 999,
		telemetry.AttrUsageCostUSD:          999,
	}}
	if metrics, reported := supervisor.snapshot(); reported || len(metrics) != 0 {
		t.Fatalf("supervisor trusted metrics without a callback: %v, reported %v", metrics, reported)
	}
	if context.Cause(ctx) != nil {
		t.Fatalf("model-authored metrics canceled supervisor context: %v", context.Cause(ctx))
	}
	outcome := stageOutcome{Result: modelAuthored}
	settleSupervisedUsage(&outcome, supervisor)
	if outcome.ObservedUsageReported || len(outcome.ObservedUsage) != 0 ||
		outcome.Result.Metrics[telemetry.AttrUsageCostUSD] != 999 {
		t.Fatalf("model metrics crossed into trusted usage: %+v", outcome)
	}

	belowCtx, below := newSupervisedUsageContext(
		context.Background(), apiv1.Limits{MaxTokens: 1, MaxCostUSD: 0.01},
	)
	below.report(map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  0,
		telemetry.AttrGenAIUsageOutputTokens: 0,
		telemetry.AttrUsageCostUSD:           0,
		"model_authored":                     999,
	})
	metrics, reported := below.snapshot()
	if !reported || len(metrics) != 3 {
		t.Fatalf("callback snapshot = %v, reported %v", metrics, reported)
	}
	if context.Cause(belowCtx) != nil {
		t.Fatalf("below-cap callback canceled supervisor context: %v", context.Cause(belowCtx))
	}
}

func TestSupervisorBudgetCancellationIsNonRetryable(t *testing.T) {
	_, supervisor := newSupervisedUsageContext(
		context.Background(), apiv1.Limits{MaxCostUSD: 0.1},
	)
	supervisor.report(map[string]float64{telemetry.AttrUsageCostUSD: 0.2})
	outcome := stageOutcome{
		Result: apiv1.ResultEnvelope{
			Status:  apiv1.ResultSuccess,
			Metrics: map[string]float64{telemetry.AttrUsageCostUSD: 0},
		},
		Verdict: &apiv1.Verdict{Decision: apiv1.VerdictPass},
	}
	settleSupervisedUsage(&outcome, supervisor)
	if outcome.Verdict != nil || outcome.Result.Status != apiv1.ResultFailure ||
		outcome.Result.Error == nil || outcome.Result.Error.Code != runner.BudgetExceededErrorCode ||
		outcome.Result.Error.Retryable {
		t.Fatalf("settled outcome = %+v", outcome)
	}
	if outcome.ObservedUsage[telemetry.AttrUsageCostUSD] != 0.2 {
		t.Fatalf("trusted observed usage = %v", outcome.ObservedUsage)
	}
}
