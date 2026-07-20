package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// RecordAgentUsage copies canonical usage metrics onto the active agentic span.
func RecordAgentUsage(ctx context.Context, metrics map[string]float64) {
	if len(metrics) == 0 {
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
}
