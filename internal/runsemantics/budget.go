package runsemantics

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/usageallowance"
)

// BudgetExceededErrorCode identifies a stage usage limit refusal.
const BudgetExceededErrorCode = "budget-exceeded"

// StageUsageTotals retains exact cumulative usage across stage attempts.
type StageUsageTotals struct {
	Metrics     map[string]float64 `json:"-"`
	CostUSD     *big.Rat           `json:"-"`
	InvalidCost bool               `json:"-"`
}

// NewStageUsageTotals creates empty cumulative usage.
func NewStageUsageTotals() *StageUsageTotals {
	return &StageUsageTotals{Metrics: make(map[string]float64)}
}

// AccumulateStageUsage adds an attempt without losing decimal cost precision.
func AccumulateStageUsage(total *StageUsageTotals, attempt map[string]float64) {
	for name, value := range attempt {
		if !telemetry.IsCanonicalAgentUsageMetric(name) {
			continue
		}
		if name != telemetry.AttrUsageCostUSD {
			total.Metrics[name] += value
			continue
		}

		valueDecimal, ok := decimalFloat64(value)
		if !ok || total.InvalidCost {
			total.Metrics[name] += value
			total.InvalidCost = true
			continue
		}
		if total.CostUSD == nil {
			total.CostUSD = new(big.Rat)
		}
		total.CostUSD.Add(total.CostUSD, valueDecimal)
		total.Metrics[name], _ = total.CostUSD.Float64()
	}
}

func decimalFloat64(value float64) (*big.Rat, bool) {
	return new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
}

// EnforceStageBudget applies token and cost limits to an attempt result.
func EnforceStageBudget(limits apiv1.Limits, attempt map[string]float64, total *StageUsageTotals, result apiv1.ResultEnvelope) (apiv1.ResultEnvelope, bool) {
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
			input := total.Metrics[telemetry.AttrGenAIUsageInputTokens]
			output := total.Metrics[telemetry.AttrGenAIUsageOutputTokens]
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
			cost := total.Metrics[telemetry.AttrUsageCostUSD]
			limit, validLimit := decimalFloat64(limits.MaxCostUSD)
			switch {
			case total.InvalidCost || math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0:
				violations = append(violations, fmt.Sprintf(
					"cannot enforce maxCostUSD %g: invalid cumulative cost usage %g",
					limits.MaxCostUSD, cost,
				))
			case !validLimit:
				violations = append(violations, fmt.Sprintf(
					"cannot enforce invalid maxCostUSD %g",
					limits.MaxCostUSD,
				))
			case total.CostUSD.Cmp(limit) > 0:
				violations = append(violations, fmt.Sprintf(
					"cost usage $%g exceeds maxCostUSD $%g",
					cost, limits.MaxCostUSD,
				))
			}
		}
	}
	return BudgetFailure(result, violations), len(violations) > 0
}

// BudgetFailure preserves the result while recording usage limit violations.
func BudgetFailure(result apiv1.ResultEnvelope, violations []string) apiv1.ResultEnvelope {
	if len(violations) == 0 {
		return result
	}
	result.Status = apiv1.ResultFailure
	result.Error = &apiv1.ErrorInfo{
		Code:      BudgetExceededErrorCode,
		Message:   "stage budget exceeded: " + strings.Join(violations, "; "),
		Retryable: false,
	}
	return result
}

func validTokenUsage(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value == math.Trunc(value)
}

// StageUsageBudget applies the local runner's canonical cumulative arithmetic
// to trusted adapter usage. Its zero value is ready for one task dispatch loop.
// It contains only deterministic state and may also be used by Temporal replay.
type StageUsageBudget struct {
	Totals *StageUsageTotals `json:"-"`
}

// Allowance returns the deterministic remaining budget for the next remote
// attempt. Nil fields are unlimited; explicit zero means exhausted.
func (b *StageUsageBudget) Allowance(limits apiv1.Limits) usageallowance.Allowance {
	var allowance usageallowance.Allowance
	if limits.MaxTokens > 0 {
		remaining := limits.MaxTokens
		if b.Totals != nil {
			input := b.Totals.Metrics[telemetry.AttrGenAIUsageInputTokens]
			output := b.Totals.Metrics[telemetry.AttrGenAIUsageOutputTokens]
			if !validTokenUsage(input) || !validTokenUsage(output) || math.IsInf(input+output, 0) {
				remaining = 0
			} else {
				used := input + output
				if used >= float64(remaining) {
					remaining = 0
				} else {
					remaining -= int64(used)
				}
			}
		}
		allowance.MaxTokens = &remaining
	}
	if limits.MaxCostUSD > 0 {
		remaining := limits.MaxCostUSD
		limit, valid := decimalFloat64(limits.MaxCostUSD)
		if !valid {
			remaining = 0
		} else if b.Totals != nil {
			switch {
			case b.Totals.InvalidCost:
				remaining = 0
			case b.Totals.CostUSD != nil:
				left := new(big.Rat).Sub(limit, b.Totals.CostUSD)
				if left.Sign() <= 0 {
					remaining = 0
				} else {
					remaining, _ = left.Float64()
				}
			}
		}
		allowance.MaxCostUSD = &remaining
	}
	return allowance
}

// Apply charges this attempt, then checks configured limits. An error before
// the adapter reported usage is not evidence that the adapter consumed budget.
func (b *StageUsageBudget) Apply(limits apiv1.Limits, metrics map[string]float64, reported bool, result *apiv1.ResultEnvelope, dispatchErr *error) {
	if b.Totals == nil {
		b.Totals = NewStageUsageTotals()
	}
	AccumulateStageUsage(b.Totals, metrics)
	if *dispatchErr == nil || reported {
		var exceeded bool
		*result, exceeded = EnforceStageBudget(limits, metrics, b.Totals, *result)
		if exceeded {
			*dispatchErr = nil
		}
	}
}
