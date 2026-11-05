package main

import (
	"context"
	"fmt"
	"maps"
	"math"
	"sync"

	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/usageallowance"
)

type supervisorBudgetExceededError struct{ message string }

func (e *supervisorBudgetExceededError) Error() string { return e.message }

type supervisedObservedUsage struct {
	mu        sync.Mutex
	allowance *usageallowance.Allowance
	cancel    context.CancelCauseFunc
	metrics   map[string]float64
	reported  bool
	exceeded  error
}

func newSupervisedUsageContext(
	parent context.Context,
	allowance *usageallowance.Allowance,
) (context.Context, *supervisedObservedUsage) {
	ctx, cancel := context.WithCancelCause(parent)
	supervisor := &supervisedObservedUsage{
		allowance: usageallowance.Clone(allowance), cancel: cancel,
	}
	if allowance != nil {
		switch {
		case allowance.MaxTokens != nil && *allowance.MaxTokens == 0:
			supervisor.exceeded = &supervisorBudgetExceededError{message: "remaining maxTokens is exhausted"}
		case allowance.MaxCostUSD != nil && *allowance.MaxCostUSD == 0:
			supervisor.exceeded = &supervisorBudgetExceededError{message: "remaining maxCostUSD is exhausted"}
		}
	}
	if supervisor.exceeded != nil {
		cancel(supervisor.exceeded)
	}
	return ctx, supervisor
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
	if u.metrics == nil {
		u.metrics = make(map[string]float64)
	}
	for name, value := range snapshot {
		if previous, ok := u.metrics[name]; !ok || value > previous {
			u.metrics[name] = value
		}
	}
	if u.exceeded == nil {
		u.exceeded = supervisorUsageLimitError(u.allowance, u.metrics)
	}
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

func supervisorUsageLimitError(allowance *usageallowance.Allowance, usage map[string]float64) error {
	if allowance == nil {
		return nil
	}
	if allowance.MaxTokens != nil {
		input, hasInput := usage[telemetry.AttrGenAIUsageInputTokens]
		output, hasOutput := usage[telemetry.AttrGenAIUsageOutputTokens]
		if hasInput && hasOutput && validSupervisorTokenUsage(input) && validSupervisorTokenUsage(output) &&
			input+output > float64(*allowance.MaxTokens) {
			return &supervisorBudgetExceededError{message: fmt.Sprintf(
				"token usage %.0f exceeds remaining maxTokens %d", input+output, *allowance.MaxTokens,
			)}
		}
	}
	if allowance.MaxCostUSD != nil {
		cost, ok := usage[telemetry.AttrUsageCostUSD]
		if ok && validSupervisorUsage(cost) && cost > *allowance.MaxCostUSD {
			return &supervisorBudgetExceededError{message: fmt.Sprintf(
				"cost usage $%g exceeds remaining maxCostUSD $%g", cost, *allowance.MaxCostUSD,
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
