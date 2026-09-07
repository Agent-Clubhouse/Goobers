package telemetry

import "go.opentelemetry.io/otel/attribute"

// SetEngineFallback adds bounded routing diagnostics to a scheduler span. Stage
// names and raw refusal prose stay in the journal, not telemetry dimensions.
func (s Span) SetEngineFallback(reasonClass string, declared bool, selfPinned, unpinnedGates int) {
	if s.span == nil {
		return
	}
	s.span.SetAttributes(
		attribute.Bool("goobers.engine.fallback.placement_declared", declared),
		attribute.String("goobers.engine.fallback.reason_class", s.scrub(reasonClass)),
		attribute.Int("goobers.engine.fallback.self_pinned_count", selfPinned),
		attribute.Int("goobers.engine.fallback.unpinned_gate_count", unpinnedGates),
	)
}
