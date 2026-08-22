package telemetry

import (
	"context"

	"github.com/goobers/goobers/internal/journal"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// GenAIModelUsageEventName identifies one model's usage on an agentic task
// span. The event carries AttrGenAIResponseModel plus whichever usage measures
// the harness reported.
const GenAIModelUsageEventName = "goobers.gen_ai.model_usage"

// ModelUsage preserves one model's observed usage. Nil measures are unknown;
// pointers to zero are measured zeroes.
type ModelUsage struct {
	Model                  string
	InputTokens            *int64
	OutputTokens           *int64
	CopilotPremiumRequests *float64
	CostUSD                *float64
}

// IsCanonicalAgentUsageMetric reports whether name is owned by the harness
// adapter rather than by an agent-authored completion envelope.
func IsCanonicalAgentUsageMetric(name string) bool {
	switch name {
	case AttrGenAIUsageInputTokens,
		AttrGenAIUsageOutputTokens,
		AttrCopilotPremiumRequests,
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

	attrs := make([]attribute.KeyValue, 0, 4)
	if value, ok := metrics[AttrGenAIUsageInputTokens]; ok {
		attrs = append(attrs, attribute.Int64(AttrGenAIUsageInputTokens, int64(value)))
	}
	if value, ok := metrics[AttrGenAIUsageOutputTokens]; ok {
		attrs = append(attrs, attribute.Int64(AttrGenAIUsageOutputTokens, int64(value)))
	}
	if value, ok := metrics[AttrCopilotPremiumRequests]; ok {
		attrs = append(attrs, attribute.Float64(AttrCopilotPremiumRequests, value))
	}
	if value, ok := metrics[AttrUsageCostUSD]; ok {
		attrs = append(attrs, attribute.Float64(AttrUsageCostUSD, value))
	}
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

// RecordNestedAgentUsage fills measures that are absent from the adapter's
// aggregate with finalized child usage. Adapter-reported canonical measures
// remain authoritative, preventing coordinator usage from being counted twice.
func RecordNestedAgentUsage(ctx context.Context, metrics map[string]float64, events []journal.Event) {
	usage := journal.RollupAgentUsage(events)
	nested := make(map[string]float64)
	if usage.InputTokens != nil {
		nested[AttrGenAIUsageInputTokens] = float64(*usage.InputTokens)
	}
	if usage.OutputTokens != nil {
		nested[AttrGenAIUsageOutputTokens] = float64(*usage.OutputTokens)
	}
	if usage.CostUSD != nil {
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
	if len(nested) > 0 {
		RecordAgentUsage(ctx, merged, nil)
	}
}

func hasModelUsage(usage ModelUsage) bool {
	return usage.InputTokens != nil ||
		usage.OutputTokens != nil ||
		usage.CopilotPremiumRequests != nil ||
		usage.CostUSD != nil
}

func modelUsageAttributes(usage ModelUsage) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String(AttrGenAIResponseModel, usage.Model)}
	if usage.InputTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrGenAIUsageInputTokens, *usage.InputTokens))
	}
	if usage.OutputTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrGenAIUsageOutputTokens, *usage.OutputTokens))
	}
	if usage.CopilotPremiumRequests != nil {
		attrs = append(attrs, attribute.Float64(AttrCopilotPremiumRequests, *usage.CopilotPremiumRequests))
	}
	if usage.CostUSD != nil {
		attrs = append(attrs, attribute.Float64(AttrUsageCostUSD, *usage.CostUSD))
	}
	return attrs
}
