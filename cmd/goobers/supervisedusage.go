package main

import (
	"context"
	"fmt"
	"maps"
	"math"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
)

type supervisorBudgetExceededError struct{ message string }

func (e *supervisorBudgetExceededError) Error() string { return e.message }

type supervisedObservedUsage struct {
	mu       sync.Mutex
	limits   apiv1.Limits
	cancel   context.CancelCauseFunc
	metrics  map[string]float64
	reported bool
	exceeded error
}

func newSupervisedUsageContext(
	parent context.Context,
	limits apiv1.Limits,
) (context.Context, *supervisedObservedUsage) {
	ctx, cancel := context.WithCancelCause(parent)
	return ctx, &supervisedObservedUsage{limits: limits, cancel: cancel}
}

func (u *supervisedObservedUsage) report(metrics map[string]float64) {
	snapshot := make(map[string]float64)
	for name, value := range metrics {
		if telemetry.IsCanonicalAgentUsageMetric(name) {
			snapshot[name] = value
		}
	}
	u.mu.Lock()
	u.reported = true
	if u.exceeded != nil {
		exceeded := u.exceeded
		u.mu.Unlock()
		u.cancel(exceeded)
		return
	}
	u.metrics = snapshot
	u.exceeded = supervisorUsageLimitError(u.limits, snapshot)
	exceeded := u.exceeded
	u.mu.Unlock()
	if exceeded != nil {
		u.cancel(exceeded)
	}
}

func (u *supervisedObservedUsage) snapshot() (map[string]float64, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return maps.Clone(u.metrics), u.reported
}

func (u *supervisedObservedUsage) budgetCause() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.exceeded
}

func supervisorUsageLimitError(limits apiv1.Limits, usage map[string]float64) error {
	if limits.MaxTokens > 0 {
		input, hasInput := usage[telemetry.AttrGenAIUsageInputTokens]
		output, hasOutput := usage[telemetry.AttrGenAIUsageOutputTokens]
		if hasInput && hasOutput && validSupervisorTokenUsage(input) && validSupervisorTokenUsage(output) &&
			input+output > float64(limits.MaxTokens) {
			return &supervisorBudgetExceededError{message: fmt.Sprintf(
				"token usage %.0f exceeds maxTokens %d", input+output, limits.MaxTokens,
			)}
		}
	}
	if limits.MaxCostUSD > 0 {
		cost, ok := usage[telemetry.AttrUsageCostUSD]
		if ok && validSupervisorUsage(cost) && cost > limits.MaxCostUSD {
			return &supervisorBudgetExceededError{message: fmt.Sprintf(
				"cost usage $%g exceeds maxCostUSD $%g", cost, limits.MaxCostUSD,
			)}
		}
	}
	return nil
}

func validSupervisorUsage(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validSupervisorTokenUsage(value float64) bool {
	return validSupervisorUsage(value) && value == math.Trunc(value)
}

func settleSupervisedUsage(out *stageOutcome, usage *supervisedObservedUsage) {
	defer usage.cancel(nil)
	out.ObservedUsage, out.ObservedUsageReported = usage.snapshot()
	if cause := usage.budgetCause(); cause != nil {
		out.Verdict = nil
		out.Result = failureEnvelope(runner.BudgetExceededErrorCode, "stage budget exceeded: "+cause.Error())
		out.Result.Error.Retryable = false
	}
}
