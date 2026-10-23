package engine

import (
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
)

func usageMetrics(tokens, cost float64) map[string]float64 {
	return map[string]float64{telemetry.AttrGenAIUsageInputTokens: tokens, telemetry.AttrGenAIUsageOutputTokens: 0, telemetry.AttrUsageCostUSD: cost}
}
func init() {
	for _, tc := range []struct {
		name  string
		limit apiv1.Limits
		usage map[string]float64
	}{
		{"tokens", apiv1.Limits{MaxTokens: 10}, usageMetrics(6, 0)},
		{"cost", apiv1.Limits{MaxCostUSD: 0.3}, usageMetrics(0, 0.2)},
		{"missing", apiv1.Limits{MaxTokens: 10}, map[string]float64{}},
	} {
		spec := agenticRetrySpec(&apiv1.RetryPolicy{MaxAttempts: 3})
		spec.Tasks[0].Workspace = apiv1.WorkspaceScratch
		spec.Tasks[0].Limits = &tc.limit
		registerParityRow(parityCase{
			Row: parityRow("E2875-usage-budget-" + tc.name), Name: "trusted cumulative usage budget", Spec: spec,
			Script:  map[string][]scriptedCall{"implement": {{err: errors.New("retryable adapter failure"), usage: tc.usage}, {result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, usage: tc.usage}}},
			Premise: func(obs parityObservation) error { return budgetJournalFailure(obs.Runner) },
			Check:   func(obs parityObservation) error { return budgetJournalFailure(obs.Engine) },
		})
	}
}
func budgetJournalFailure(side paritySide) error {
	for _, ev := range side.Events {
		if ev.Type == journal.EventStageFinished && ev.Error != nil && ev.Error.Code == runner.BudgetExceededErrorCode {
			return nil
		}
	}
	return fmt.Errorf("%s did not journal budget-exceeded", side.Name)
}
