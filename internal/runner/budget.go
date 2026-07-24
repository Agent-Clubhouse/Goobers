package runner

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

const budgetExceededErrorCode = "budget-exceeded"

type attemptUsageCollector struct {
	mu       sync.Mutex
	metrics  map[string]float64
	reported bool
}

type stageUsageTotals struct {
	metrics     map[string]float64
	costUSD     *big.Rat
	invalidCost bool
}

func (c *attemptUsageCollector) report(metrics map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reported = true
	if c.metrics == nil {
		c.metrics = make(map[string]float64)
	}
	for name, value := range metrics {
		if telemetry.IsCanonicalAgentUsageMetric(name) {
			c.metrics[name] = value
		}
	}
}

func (c *attemptUsageCollector) snapshot() (map[string]float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	metrics := make(map[string]float64, len(c.metrics))
	for name, value := range c.metrics {
		metrics[name] = value
	}
	return metrics, c.reported
}

func newStageUsageTotals() *stageUsageTotals {
	return &stageUsageTotals{metrics: make(map[string]float64)}
}

func accumulateStageUsage(total *stageUsageTotals, attempt map[string]float64) {
	for name, value := range attempt {
		if !telemetry.IsCanonicalAgentUsageMetric(name) {
			continue
		}
		if name != telemetry.AttrUsageCostUSD {
			total.metrics[name] += value
			continue
		}

		valueDecimal, ok := decimalFloat64(value)
		if !ok || total.invalidCost {
			total.metrics[name] += value
			total.invalidCost = true
			continue
		}
		if total.costUSD == nil {
			total.costUSD = new(big.Rat)
		}
		total.costUSD.Add(total.costUSD, valueDecimal)
		total.metrics[name], _ = total.costUSD.Float64()
	}
}

func decimalFloat64(value float64) (*big.Rat, bool) {
	return new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
}

func enforceStageBudget(limits apiv1.Limits, attempt map[string]float64, total *stageUsageTotals, result apiv1.ResultEnvelope) (apiv1.ResultEnvelope, bool) {
	var violations []string
	if limits.MaxTokens > 0 {
		attemptInput, hasInput := attempt[telemetry.AttrGenAIUsageInputTokens]
		attemptOutput, hasOutput := attempt[telemetry.AttrGenAIUsageOutputTokens]
		switch {
		case !hasInput || !hasOutput:
			var missing []string
			if !hasInput {
				missing = append(missing, telemetry.AttrGenAIUsageInputTokens)
			}
			if !hasOutput {
				missing = append(missing, telemetry.AttrGenAIUsageOutputTokens)
			}
			violations = append(violations, fmt.Sprintf(
				"cannot enforce maxTokens %d: missing %s",
				limits.MaxTokens, strings.Join(missing, ", "),
			))
		case !validTokenUsage(attemptInput) || !validTokenUsage(attemptOutput):
			violations = append(violations, fmt.Sprintf(
				"cannot enforce maxTokens %d: invalid token usage input=%g output=%g",
				limits.MaxTokens, attemptInput, attemptOutput,
			))
		default:
			input := total.metrics[telemetry.AttrGenAIUsageInputTokens]
			output := total.metrics[telemetry.AttrGenAIUsageOutputTokens]
			switch {
			case !validTokenUsage(input) || !validTokenUsage(output) || math.IsInf(input+output, 0):
				violations = append(violations, fmt.Sprintf(
					"cannot enforce maxTokens %d: invalid cumulative token usage input=%g output=%g",
					limits.MaxTokens, input, output,
				))
			case input+output > float64(limits.MaxTokens):
				violations = append(violations, fmt.Sprintf(
					"token usage %.0f exceeds maxTokens %d",
					input+output, limits.MaxTokens,
				))
			}
		}
	}
	if limits.MaxCostUSD > 0 {
		attemptCost, ok := attempt[telemetry.AttrUsageCostUSD]
		switch {
		case !ok:
			violations = append(violations, fmt.Sprintf(
				"cannot enforce maxCostUSD %g: missing %s",
				limits.MaxCostUSD, telemetry.AttrUsageCostUSD,
			))
		case math.IsNaN(attemptCost) || math.IsInf(attemptCost, 0) || attemptCost < 0:
			violations = append(violations, fmt.Sprintf(
				"cannot enforce maxCostUSD %g: invalid cost usage %g",
				limits.MaxCostUSD, attemptCost,
			))
		default:
			cost := total.metrics[telemetry.AttrUsageCostUSD]
			limit, validLimit := decimalFloat64(limits.MaxCostUSD)
			switch {
			case total.invalidCost || math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0:
				violations = append(violations, fmt.Sprintf(
					"cannot enforce maxCostUSD %g: invalid cumulative cost usage %g",
					limits.MaxCostUSD, cost,
				))
			case !validLimit:
				violations = append(violations, fmt.Sprintf(
					"cannot enforce invalid maxCostUSD %g",
					limits.MaxCostUSD,
				))
			case total.costUSD.Cmp(limit) > 0:
				violations = append(violations, fmt.Sprintf(
					"cost usage $%g exceeds maxCostUSD $%g",
					cost, limits.MaxCostUSD,
				))
			}
		}
	}
	return budgetFailure(result, violations), len(violations) > 0
}

func interruptedStageBudgetFailure(limits apiv1.Limits) apiv1.ResultEnvelope {
	var violations []string
	if limits.MaxTokens > 0 {
		violations = append(violations, fmt.Sprintf(
			"cannot enforce maxTokens %d: interrupted attempt usage is unavailable",
			limits.MaxTokens,
		))
	}
	if limits.MaxCostUSD > 0 {
		violations = append(violations, fmt.Sprintf(
			"cannot enforce maxCostUSD %g: interrupted attempt usage is unavailable",
			limits.MaxCostUSD,
		))
	}
	return budgetFailure(apiv1.ResultEnvelope{}, violations)
}

func budgetFailure(result apiv1.ResultEnvelope, violations []string) apiv1.ResultEnvelope {
	if len(violations) == 0 {
		return result
	}
	result.Status = apiv1.ResultFailure
	result.Error = &apiv1.ErrorInfo{
		Code:      budgetExceededErrorCode,
		Message:   "stage budget exceeded: " + strings.Join(violations, "; "),
		Retryable: false,
	}
	return result
}

func usageBudgetConfigured(limits apiv1.Limits) bool {
	return limits.MaxTokens > 0 || limits.MaxCostUSD > 0
}

func validTokenUsage(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value == math.Trunc(value)
}
