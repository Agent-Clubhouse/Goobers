package main

import (
	"testing"

	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/usageallowance"
)

func TestApplyAgenticKitAttemptContractCopiesUsageAllowance(t *testing.T) {
	tokens, cost := int64(0), 0.25
	attempt := dispatcher.Attempt{
		Review: true, ReviewRequiresDiff: true,
		UsageAllowance: &usageallowance.Allowance{
			MaxTokens: &tokens, MaxCostUSD: &cost,
		},
	}
	kit := &agentickit.Kit{}
	applyAgenticKitAttemptContract(kit, attempt)
	tokens, cost = 99, 99
	if !kit.ReviewRequiresDiff || kit.UsageAllowance == nil ||
		kit.UsageAllowance.MaxTokens == nil || *kit.UsageAllowance.MaxTokens != 0 ||
		kit.UsageAllowance.MaxCostUSD == nil || *kit.UsageAllowance.MaxCostUSD != 0.25 {
		t.Fatalf("kit attempt contract = %+v", kit)
	}
}
