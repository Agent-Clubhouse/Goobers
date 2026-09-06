package telemetry

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestModelUsageAttributesPreserveCanonicalUsage(t *testing.T) {
	zero, one, two, three := int64(0), int64(1), int64(2), int64(3)
	premium := 0.0
	attrs := modelUsageAttributes(ModelUsage{
		Model: "gpt-5.6", InputTokens: &one, OutputTokens: &two,
		CacheReadTokens: &three, CacheWriteTokens: &zero,
		ReasoningTokens: &one, CopilotPremiumRequests: &premium,
		NanoAIU: &zero, BillingModel: BillingModelAICredits,
		CostBasis: CostBasisVendorReported,
	})
	got := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		got[string(attr.Key)] = attr.Value.AsInterface()
	}
	for key, want := range map[string]int64{
		AttrGenAIUsageInputTokens:  1,
		AttrGenAIUsageOutputTokens: 2,
		AttrUsageCacheReadTokens:   3,
		AttrUsageCacheWriteTokens:  0,
		AttrUsageReasoningTokens:   1,
		AttrUsageNanoAIU:           0,
	} {
		if got[key] != want {
			t.Errorf("%s = %#v, want %d", key, got[key], want)
		}
	}
	if got[AttrUsageBillingModel] != BillingModelAICredits ||
		got[AttrUsageCostBasis] != CostBasisVendorReported ||
		got[AttrUsageCostUSD] != float64(0) {
		t.Fatalf("metadata attributes = %#v", got)
	}
	if _, ok := got[AttrCopilotPremiumRequests]; ok {
		t.Fatalf("zero premium requests must be omitted: %#v", got)
	}
}

func TestMergeNestedAgentUsageIncludesExactCreditAndTokenUsage(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	nano, cacheRead, cacheWrite, reasoning := int64(250_000_000_000), int64(30), int64(4), int64(7)
	events := []journal.Event{{Type: journal.EventAgentLifecycle, Agent: &journal.AgentProvenance{
		Schema: "goobers.dev/journal/agent/v1", ID: "worker", RunID: "run", Stage: "work",
		Attempt: 1, Lifecycle: journal.AgentCompleted, StartedAt: now, UpdatedAt: now,
		Usage: journal.AgentUsage{
			NanoAIU: &nano, CacheReadTokens: &cacheRead,
			CacheWriteTokens: &cacheWrite, ReasoningTokens: &reasoning,
		},
	}}}

	got := MergeNestedAgentUsage(nil, events)
	if got[AttrUsageNanoAIU] != 250_000_000_000 ||
		got[AttrUsageCacheReadTokens] != 30 ||
		got[AttrUsageCacheWriteTokens] != 4 ||
		got[AttrUsageReasoningTokens] != 7 ||
		got[AttrUsageCostUSD] != 2.5 {
		t.Fatalf("merged usage = %#v", got)
	}
}
