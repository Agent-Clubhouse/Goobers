package main

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/usageallowance"
)

func TestSupervisorTerminatesOnTokenCap(t *testing.T) {
	ctx, supervisor := newSupervisedUsageContext(
		context.Background(), testUsageAllowance(10, 0),
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
		context.Background(), testUsageAllowance(1, 0.01),
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
		context.Background(), testUsageAllowance(1, 0.01),
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
		context.Background(), testUsageAllowance(0, 0.1),
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

func TestSupervisorUsageSnapshotsAreMonotonic(t *testing.T) {
	_, supervisor := newSupervisedUsageContext(
		context.Background(), testUsageAllowance(100, 0),
	)
	supervisor.report(map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  60,
		telemetry.AttrGenAIUsageOutputTokens: 30,
	})
	supervisor.report(map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  5,
		telemetry.AttrGenAIUsageOutputTokens: 5,
	})
	supervisor.report(map[string]float64{})
	metrics, _ := supervisor.snapshot()
	if metrics[telemetry.AttrGenAIUsageInputTokens] != 60 ||
		metrics[telemetry.AttrGenAIUsageOutputTokens] != 30 {
		t.Fatalf("decreasing/empty reports lowered usage: %v", metrics)
	}

	supervisor.report(map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  80,
		telemetry.AttrGenAIUsageOutputTokens: 30,
	})
	var firstCause *supervisorBudgetExceededError
	if !errors.As(supervisor.budgetCause(), &firstCause) {
		t.Fatalf("first cap crossing cause = %T %v", supervisor.budgetCause(), supervisor.budgetCause())
	}
	supervisor.report(map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  800,
		telemetry.AttrGenAIUsageOutputTokens: 300,
	})
	metrics, _ = supervisor.snapshot()
	if metrics[telemetry.AttrGenAIUsageInputTokens] != 800 ||
		metrics[telemetry.AttrGenAIUsageOutputTokens] != 300 {
		t.Fatalf("post-cap final report did not raise surrendered usage: %v", metrics)
	}
	if got := supervisor.budgetCause(); got == nil || got.Error() != firstCause.Error() {
		t.Fatalf("post-cap report replaced the first crossing cause: %v", got)
	}
}

func TestSupervisorRefusesExhaustedAllowanceBeforeUsage(t *testing.T) {
	zero := int64(0)
	ctx, supervisor := newSupervisedUsageContext(context.Background(), &usageallowance.Allowance{
		MaxTokens: &zero,
	})
	var exceeded *supervisorBudgetExceededError
	if !errors.As(context.Cause(ctx), &exceeded) ||
		!errors.As(supervisor.budgetCause(), &exceeded) {
		t.Fatalf("exhausted allowance cause = %T %v", context.Cause(ctx), context.Cause(ctx))
	}
	outcome := stageOutcome{}
	settleSupervisedUsage(&outcome, supervisor)
	if outcome.Result.Error == nil || outcome.Result.Error.Code != runner.BudgetExceededErrorCode {
		t.Fatalf("exhausted outcome = %+v", outcome)
	}
}

func testUsageAllowance(tokens int64, cost float64) *usageallowance.Allowance {
	out := &usageallowance.Allowance{}
	if tokens > 0 {
		out.MaxTokens = &tokens
	}
	if cost > 0 {
		out.MaxCostUSD = &cost
	}
	return out
}
