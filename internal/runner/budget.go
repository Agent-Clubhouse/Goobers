package runner

import (
	"fmt"
	"math"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

const budgetExceededErrorCode = "budget-exceeded"

func enforceStageBudget(limits apiv1.Limits, result apiv1.ResultEnvelope) apiv1.ResultEnvelope {
	var violations []string
	if limits.MaxTokens > 0 {
		input, hasInput := result.Metrics[telemetry.AttrGenAIUsageInputTokens]
		output, hasOutput := result.Metrics[telemetry.AttrGenAIUsageOutputTokens]
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
		case !validTokenUsage(input) || !validTokenUsage(output) || math.IsInf(input+output, 0):
			violations = append(violations, fmt.Sprintf(
				"cannot enforce maxTokens %d: invalid token usage input=%g output=%g",
				limits.MaxTokens, input, output,
			))
		case input+output > float64(limits.MaxTokens):
			violations = append(violations, fmt.Sprintf(
				"token usage %.0f exceeds maxTokens %d",
				input+output, limits.MaxTokens,
			))
		}
	}
	if limits.MaxCostUSD > 0 {
		cost, ok := result.Metrics[telemetry.AttrUsageCostUSD]
		switch {
		case !ok:
			violations = append(violations, fmt.Sprintf(
				"cannot enforce maxCostUSD %g: missing %s",
				limits.MaxCostUSD, telemetry.AttrUsageCostUSD,
			))
		case math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0:
			violations = append(violations, fmt.Sprintf(
				"cannot enforce maxCostUSD %g: invalid cost usage %g",
				limits.MaxCostUSD, cost,
			))
		case cost > limits.MaxCostUSD:
			violations = append(violations, fmt.Sprintf(
				"cost usage $%g exceeds maxCostUSD $%g",
				cost, limits.MaxCostUSD,
			))
		}
	}
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

func validTokenUsage(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value == math.Trunc(value)
}
