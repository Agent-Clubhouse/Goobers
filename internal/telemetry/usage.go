package telemetry

import (
	"context"
	"math"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/goobers/goobers/internal/journal"
)

// GenAIModelUsageEventName identifies one model's usage on an agentic task
// span. The event carries AttrGenAIResponseModel plus whichever usage measures
// the harness reported.
const GenAIModelUsageEventName = "goobers.gen_ai.model_usage"

const (
	// BillingModelAICredits identifies AI-credit billing.
	BillingModelAICredits = "ai_credits"
	// BillingModelPremiumRequests identifies legacy premium-request billing.
	BillingModelPremiumRequests = "premium_requests"
	// CostBasisVendorReported identifies vendor-reported cost data.
	CostBasisVendorReported = "vendor_reported"
	// CostBasisUnknown identifies usage without an authoritative cost basis.
	CostBasisUnknown = "unknown"
	// NanoAIUPerAICredit is the number of nano-AIU in one AI credit.
	NanoAIUPerAICredit int64 = 1_000_000_000
	// NanoAIUPerUSD is the number of nano-AIU represented by one US dollar.
	NanoAIUPerUSD int64 = 100_000_000_000
)

// ModelUsage preserves one model's observed usage. Nil measures are unknown;
// pointers to zero are measured zeroes.
type ModelUsage struct {
	Model                  string
	InputTokens            *int64
	OutputTokens           *int64
	CacheReadTokens        *int64
	CacheWriteTokens       *int64
	ReasoningTokens        *int64
	CopilotPremiumRequests *float64
	NanoAIU                *int64
	CostUSD                *float64
	BillingModel           string
	CostBasis              string
}

// IsCanonicalAgentUsageMetric reports whether name is owned by the harness
// adapter rather than by an agent-authored completion envelope.
func IsCanonicalAgentUsageMetric(name string) bool {
	switch name {
	case AttrGenAIUsageInputTokens,
		AttrGenAIUsageOutputTokens,
		AttrCopilotPremiumRequests,
		AttrUsageNanoAIU,
		AttrUsageBillingModel,
		AttrUsageCacheReadTokens,
		AttrUsageCacheWriteTokens,
		AttrUsageReasoningTokens,
		AttrUsageCostBasis,
		AttrUsageCostUSD:
		return true
	default:
		return false
	}
}

// RecordAgentUsage copies canonical usage metrics onto the active agentic span.
func RecordAgentUsage(ctx context.Context, metrics map[string]float64, modelUsage []ModelUsage) {
	if len(metrics) == 0 && len(modelUsage) == 0 {
		return
	}

	attrs := aggregateTokenUsageAttributes(metrics)
	attrs = append(attrs, aggregateBillingUsageAttributes(metrics, modelUsage)...)
	if len(attrs) > 0 {
		trace.SpanFromContext(ctx).SetAttributes(attrs...)
	}

	measured := make([]ModelUsage, 0, len(modelUsage))
	for _, usage := range modelUsage {
		if usage.Model == "" || !hasModelUsage(usage) {
			continue
		}
		measured = append(measured, usage)
	}
	span := trace.SpanFromContext(ctx)
	if len(measured) == 1 {
		span.SetAttributes(attribute.String(AttrGenAIResponseModel, measured[0].Model))
	}
	for _, usage := range measured {
		span.AddEvent(GenAIModelUsageEventName, trace.WithAttributes(modelUsageAttributes(usage)...))
	}
}

func aggregateTokenUsageAttributes(metrics map[string]float64) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 5)
	if value, ok := metrics[AttrGenAIUsageInputTokens]; ok {
		attrs = append(attrs, attribute.Int64(AttrGenAIUsageInputTokens, int64(value)))
	}
	if value, ok := metrics[AttrGenAIUsageOutputTokens]; ok {
		attrs = append(attrs, attribute.Int64(AttrGenAIUsageOutputTokens, int64(value)))
	}
	if value, ok := metrics[AttrUsageCacheReadTokens]; ok {
		attrs = append(attrs, attribute.Int64(AttrUsageCacheReadTokens, int64(value)))
	}
	if value, ok := metrics[AttrUsageCacheWriteTokens]; ok {
		attrs = append(attrs, attribute.Int64(AttrUsageCacheWriteTokens, int64(value)))
	}
	if value, ok := metrics[AttrUsageReasoningTokens]; ok {
		attrs = append(attrs, attribute.Int64(AttrUsageReasoningTokens, int64(value)))
	}
	return attrs
}

func aggregateBillingUsageAttributes(metrics map[string]float64, modelUsage []ModelUsage) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 5)
	nanoAIU, hasNanoAIU := metrics[AttrUsageNanoAIU]
	exactNanoAIU, hasExactNanoAIU := summedModelNanoAIU(modelUsage)
	if hasExactNanoAIU && (!hasNanoAIU || float64(exactNanoAIU) == nanoAIU) {
		nanoAIU = float64(exactNanoAIU)
		hasNanoAIU = true
	}
	if hasNanoAIU {
		nanoAIUInt := int64(nanoAIU)
		if hasExactNanoAIU && float64(exactNanoAIU) == nanoAIU {
			nanoAIUInt = exactNanoAIU
		}
		costBasis := CostBasisVendorReported
		if hasExactNanoAIU {
			if basis, ok := commonModelCostBasis(modelUsage); ok {
				costBasis = basis
			}
		}
		attrs = append(attrs,
			attribute.Int64(AttrUsageNanoAIU, nanoAIUInt),
			attribute.String(AttrUsageBillingModel, BillingModelAICredits),
			attribute.String(AttrUsageCostBasis, costBasis),
		)
	}
	if value, ok := metrics[AttrCopilotPremiumRequests]; ok && value != 0 {
		attrs = append(attrs, attribute.Float64(AttrCopilotPremiumRequests, value))
		if !hasNanoAIU {
			attrs = append(attrs,
				attribute.String(AttrUsageBillingModel, BillingModelPremiumRequests),
				attribute.String(AttrUsageCostBasis, CostBasisUnknown),
			)
		}
	}
	if hasNanoAIU {
		nanoAIUInt := int64(nanoAIU)
		if hasExactNanoAIU && float64(exactNanoAIU) == nanoAIU {
			nanoAIUInt = exactNanoAIU
		}
		attrs = append(attrs, attribute.Float64(AttrUsageCostUSD, NanoAIUToUSD(nanoAIUInt)))
	} else if value, ok := metrics[AttrUsageCostUSD]; ok {
		attrs = append(attrs, attribute.Float64(AttrUsageCostUSD, value))
	}
	return attrs
}

// MergeNestedAgentUsage fills measures absent from an adapter's aggregate with
// finalized nested usage. Adapter-reported measures remain authoritative.
func MergeNestedAgentUsage(metrics map[string]float64, events []journal.Event) map[string]float64 {
	usage := journal.RollupAgentUsage(events)
	nested := make(map[string]float64)
	if usage.InputTokens != nil {
		nested[AttrGenAIUsageInputTokens] = float64(*usage.InputTokens)
	}
	if usage.OutputTokens != nil {
		nested[AttrGenAIUsageOutputTokens] = float64(*usage.OutputTokens)
	}
	if usage.CacheReadTokens != nil {
		nested[AttrUsageCacheReadTokens] = float64(*usage.CacheReadTokens)
	}
	if usage.CacheWriteTokens != nil {
		nested[AttrUsageCacheWriteTokens] = float64(*usage.CacheWriteTokens)
	}
	if usage.ReasoningTokens != nil {
		nested[AttrUsageReasoningTokens] = float64(*usage.ReasoningTokens)
	}
	if usage.NanoAIU != nil {
		nested[AttrUsageNanoAIU] = float64(*usage.NanoAIU)
		nested[AttrUsageCostUSD] = NanoAIUToUSD(*usage.NanoAIU)
	} else if usage.CostUSD != nil {
		nested[AttrUsageCostUSD] = *usage.CostUSD
	}
	merged := make(map[string]float64, len(metrics)+len(nested))
	for name, value := range metrics {
		merged[name] = value
	}
	for name, value := range nested {
		if _, exists := merged[name]; !exists {
			merged[name] = value
		}
	}
	return merged
}

func hasModelUsage(usage ModelUsage) bool {
	return usage.InputTokens != nil ||
		usage.OutputTokens != nil ||
		usage.CacheReadTokens != nil ||
		usage.CacheWriteTokens != nil ||
		usage.ReasoningTokens != nil ||
		usage.CopilotPremiumRequests != nil ||
		usage.NanoAIU != nil ||
		usage.CostUSD != nil ||
		usage.BillingModel != "" ||
		usage.CostBasis != ""
}

func modelUsageAttributes(usage ModelUsage) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String(AttrGenAIResponseModel, usage.Model)}
	if usage.InputTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrGenAIUsageInputTokens, *usage.InputTokens))
	}
	if usage.OutputTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrGenAIUsageOutputTokens, *usage.OutputTokens))
	}
	if usage.CacheReadTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrUsageCacheReadTokens, *usage.CacheReadTokens))
	}
	if usage.CacheWriteTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrUsageCacheWriteTokens, *usage.CacheWriteTokens))
	}
	if usage.ReasoningTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrUsageReasoningTokens, *usage.ReasoningTokens))
	}
	if usage.CopilotPremiumRequests != nil && *usage.CopilotPremiumRequests != 0 {
		attrs = append(attrs, attribute.Float64(AttrCopilotPremiumRequests, *usage.CopilotPremiumRequests))
	}
	if usage.NanoAIU != nil {
		attrs = append(attrs, attribute.Int64(AttrUsageNanoAIU, *usage.NanoAIU))
	}
	if usage.BillingModel != "" {
		attrs = append(attrs, attribute.String(AttrUsageBillingModel, usage.BillingModel))
	}
	if usage.CostBasis != "" {
		attrs = append(attrs, attribute.String(AttrUsageCostBasis, usage.CostBasis))
	}
	if usage.NanoAIU != nil {
		attrs = append(attrs, attribute.Float64(AttrUsageCostUSD, NanoAIUToUSD(*usage.NanoAIU)))
	} else if usage.CostUSD != nil {
		attrs = append(attrs, attribute.Float64(AttrUsageCostUSD, *usage.CostUSD))
	}
	return attrs
}

// NanoAIUToUSD converts exact nano-AIU into its derived USD display value.
func NanoAIUToUSD(nanoAIU int64) float64 {
	return float64(nanoAIU) / float64(NanoAIUPerUSD)
}

// USDToNanoAIU normalizes vendor-reported USD for mixed-provider aggregation.
func USDToNanoAIU(usd float64) int64 {
	return int64(math.Round(usd * float64(NanoAIUPerUSD)))
}

func summedModelNanoAIU(usages []ModelUsage) (int64, bool) {
	var total int64
	var measured bool
	for _, usage := range usages {
		if usage.NanoAIU != nil {
			total += *usage.NanoAIU
			measured = true
		}
	}
	return total, measured
}

func commonModelCostBasis(usages []ModelUsage) (string, bool) {
	var basis string
	var measured bool
	for _, usage := range usages {
		if usage.NanoAIU == nil {
			continue
		}
		if !measured {
			basis = usage.CostBasis
			measured = true
			continue
		}
		if usage.CostBasis != basis {
			return "", false
		}
	}
	return basis, measured && basis != ""
}
