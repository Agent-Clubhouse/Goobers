package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestRenderCostReportDisclosesCoverageAndNativeUnits(t *testing.T) {
	nano := int64(12_400_000_000)
	cost := 0.124
	input := int64(829_000)
	output := int64(18_000)
	cached := int64(550_000)
	report := renderCostReport("PR", rollup.CostAggregate{
		TotalRuns: 6, MeasuredRuns: 4, TotalAttempts: 7,
		InputTokens: &input, OutputTokens: &output, CacheReadTokens: &cached,
		NanoAIU: &nano, CostUSD: &cost,
		BillingModels: []string{telemetry.BillingModelAICredits},
		Models: []rollup.CostModelAggregate{{
			Model: "gpt-5.6-sol", UsageAttempts: 7, InputTokens: &input,
			OutputTokens: &output, CacheReadTokens: &cached, NanoAIU: &nano, CostUSD: &cost,
		}},
	})
	for _, want := range []string{
		"Thanks for using Goobers! This PR cost **12.40 AI credits (~$0.12)**.",
		"Cost known for 4 of 6 runs -- totals are a lower bound.",
		"<details><summary>Cost breakdown -- 6 runs, 847.00K tokens</summary>",
		"| `gpt-5.6-sol` | 7 | 829.00K / 18.00K / 550.00K | 12.40 AIC (~$0.12) |",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

func TestRenderCostReportUnavailableAndNormalized(t *testing.T) {
	input := int64(100)
	output := int64(20)
	unavailable := renderCostReport("issue", rollup.CostAggregate{
		TotalRuns: 1, InputTokens: &input, OutputTokens: &output,
	})
	if !strings.Contains(unavailable, "Cost is unavailable; known usage is 120 tokens.") {
		t.Fatalf("unavailable report = %q", unavailable)
	}

	nano := int64(42_000_000_000)
	cost := 0.42
	normalized := renderCostReport("issue", rollup.CostAggregate{
		TotalRuns: 1, MeasuredRuns: 1, NanoAIU: &nano, CostUSD: &cost,
		Models: []rollup.CostModelAggregate{{
			Model: "claude-sonnet-5", UsageAttempts: 1, NanoAIU: &nano, CostUSD: &cost,
		}},
	})
	for _, want := range []string{"includes normalized estimates", "42.00 AIC (~$0.42)*", "vendor-reported estimates"} {
		if !strings.Contains(normalized, want) {
			t.Fatalf("normalized report missing %q:\n%s", want, normalized)
		}
	}
}
