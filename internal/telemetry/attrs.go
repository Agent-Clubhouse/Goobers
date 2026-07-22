package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// RecordAgentProvenance annotates the active task or gate span. Empty values
// remain explicit so every harness invocation has both provenance dimensions.
func RecordAgentProvenance(ctx context.Context, model, harnessVersion string) {
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String(AttrModel, model),
		attribute.String(AttrHarnessVersion, harnessVersion),
	)
}

func runAttributeSet(a RunAttributes) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(AttrRunID, a.RunID),
		attribute.String(AttrGaggle, a.Gaggle),
		attribute.String(AttrWorkflow, a.WorkflowID),
	}
	attrs = appendOptionalString(attrs, AttrWorkflowVersion, a.WorkflowVersion)
	attrs = appendOptionalString(attrs, AttrWorkflowDigest, a.WorkflowDigest)
	attrs = appendOptionalString(attrs, AttrItemID, a.ItemID)
	return appendOptionalString(attrs, AttrItemURL, a.ItemURL)
}

func taskAttributeSet(a TaskAttributes) []attribute.KeyValue {
	attempt := a.Attempt
	if attempt == 0 {
		attempt = 1
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrRunID, a.RunID),
		attribute.String(AttrGaggle, a.Gaggle),
		attribute.String(AttrWorkflow, a.WorkflowID),
		attribute.String(AttrStage, a.TaskID),
		attribute.Int(AttrAttemptNumber, attempt),
	}
	attrs = appendOptionalString(attrs, AttrWorkflowVersion, a.WorkflowVersion)
	attrs = appendOptionalString(attrs, AttrWorkflowDigest, a.WorkflowDigest)
	attrs = appendOptionalString(attrs, AttrGoober, a.GooberID)
	attrs = appendOptionalString(attrs, AttrStageType, a.TaskType)
	attrs = appendOptionalString(attrs, AttrAttemptKind, a.AttemptKind)
	attrs = appendOptionalString(attrs, AttrItemID, a.ItemID)
	return appendOptionalString(attrs, AttrItemURL, a.ItemURL)
}

func gateAttributeSet(a GateAttributes) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(AttrRunID, a.RunID),
		attribute.String(AttrGaggle, a.Gaggle),
		attribute.String(AttrWorkflow, a.WorkflowID),
		attribute.String(AttrStage, a.GateID),
		attribute.String(AttrStageType, StageTypeGate),
		attribute.Int(AttrGateRepassNumber, a.RepassNumber),
	}
	attrs = appendOptionalString(attrs, AttrWorkflowVersion, a.WorkflowVersion)
	attrs = appendOptionalString(attrs, AttrWorkflowDigest, a.WorkflowDigest)
	attrs = appendOptionalString(attrs, AttrGoober, a.GooberID)
	attrs = appendOptionalString(attrs, AttrGateDecision, a.Decision)
	attrs = appendOptionalString(attrs, AttrItemID, a.ItemID)
	return appendOptionalString(attrs, AttrItemURL, a.ItemURL)
}

func schedulerAttributeSet(a SchedulerAttributes) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(AttrGaggle, a.Gaggle),
		attribute.String(AttrWorkflow, a.WorkflowID),
		attribute.String(AttrStage, a.Action),
		attribute.String(AttrStageType, StageTypeScheduler),
	}
	attrs = appendOptionalString(attrs, AttrRunID, a.RunID)
	attrs = appendOptionalString(attrs, AttrWorkflowVersion, a.WorkflowVersion)
	attrs = appendOptionalString(attrs, AttrWorkflowDigest, a.WorkflowDigest)
	attrs = appendOptionalString(attrs, AttrItemID, a.ItemID)
	return appendOptionalString(attrs, AttrItemURL, a.ItemURL)
}

func appendOptionalString(attrs []attribute.KeyValue, key, value string) []attribute.KeyValue {
	if value == "" {
		return attrs
	}
	return append(attrs, attribute.String(key, value))
}
