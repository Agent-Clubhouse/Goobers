package runsemantics

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
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
			total := NewStageUsageTotals()
			AccumulateStageUsage(total, tc.metrics)
			got, exceeded := EnforceStageBudget(tc.limits, tc.metrics, total, apiv1.ResultEnvelope{
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
			if got.Error.Code != BudgetExceededErrorCode || got.Error.Retryable {
				t.Fatalf("error = %+v, want non-retryable %q", got.Error, BudgetExceededErrorCode)
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
			total := NewStageUsageTotals()
			var got apiv1.ResultEnvelope
			var exceeded bool
			for _, cost := range tc.attempts {
				attempt := map[string]float64{telemetry.AttrUsageCostUSD: cost}
				AccumulateStageUsage(total, attempt)
				got, exceeded = EnforceStageBudget(
					apiv1.Limits{MaxCostUSD: 0.3},
					attempt,
					total,
					apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
				)
				if exceeded {
					break
				}
			}

			if total.Metrics[telemetry.AttrUsageCostUSD] != tc.wantTotal {
				t.Fatalf("cumulative cost = %.17g, want %.17g", total.Metrics[telemetry.AttrUsageCostUSD], tc.wantTotal)
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
			total := NewStageUsageTotals()
			AccumulateStageUsage(total, tc.metrics)
			got, exceeded := EnforceStageBudget(tc.limits, tc.metrics, total, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Metrics: tc.metrics,
			})
			if !exceeded || got.Status != apiv1.ResultFailure || got.Error == nil || got.Error.Code != BudgetExceededErrorCode {
				t.Fatalf("result = %+v, want %q failure", got, BudgetExceededErrorCode)
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

	total := NewStageUsageTotals()
	AccumulateStageUsage(total, result.Metrics)
	got, exceeded := EnforceStageBudget(apiv1.Limits{MaxTokens: 100}, result.Metrics, total, result)
	if !exceeded || got.Status != apiv1.ResultFailure || got.Error == nil || got.Error.Code != BudgetExceededErrorCode {
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

func TestStageUsageBudgetAllowanceTracksCumulativeUsage(t *testing.T) {
	limits := apiv1.Limits{MaxTokens: 100, MaxCostUSD: 0.3}
	var budget StageUsageBudget
	initial := budget.Allowance(limits)
	if initial.MaxTokens == nil || *initial.MaxTokens != 100 ||
		initial.MaxCostUSD == nil || *initial.MaxCostUSD != 0.3 {
		t.Fatalf("initial allowance = %+v", initial)
	}

	result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
	var dispatchErr error
	budget.Apply(limits, map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  40,
		telemetry.AttrGenAIUsageOutputTokens: 10,
		telemetry.AttrUsageCostUSD:           0.1,
	}, true, &result, &dispatchErr)
	if dispatchErr != nil {
		t.Fatalf("first attempt: %v", dispatchErr)
	}
	remaining := budget.Allowance(limits)
	if remaining.MaxTokens == nil || *remaining.MaxTokens != 50 ||
		remaining.MaxCostUSD == nil || *remaining.MaxCostUSD != 0.2 {
		t.Fatalf("remaining allowance = %+v", remaining)
	}

	budget.Apply(limits, map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  30,
		telemetry.AttrGenAIUsageOutputTokens: 20,
		telemetry.AttrUsageCostUSD:           0.2,
	}, true, &result, &dispatchErr)
	if dispatchErr != nil {
		t.Fatalf("second attempt at exact limit: %v", dispatchErr)
	}
	exhausted := budget.Allowance(limits)
	if exhausted.MaxTokens == nil || *exhausted.MaxTokens != 0 ||
		exhausted.MaxCostUSD == nil || *exhausted.MaxCostUSD != 0 {
		t.Fatalf("exhausted allowance = %+v", exhausted)
	}
}

func TestStageUsageBudgetAllowanceKeepsUnlimitedFieldsNil(t *testing.T) {
	allowance := (&StageUsageBudget{}).Allowance(apiv1.Limits{})
	if allowance.MaxTokens != nil || allowance.MaxCostUSD != nil {
		t.Fatalf("unlimited allowance = %+v", allowance)
	}
}
