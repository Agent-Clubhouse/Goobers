package telemetry

import "go.opentelemetry.io/otel/attribute"

func runAttributeSet(a RunAttributes) []attribute.KeyValue {
	return withOptional([]attribute.KeyValue{
		attribute.String(AttrSpanKind, SpanKindRun),
		attribute.String(AttrGaggle, a.Gaggle),
		attribute.String(AttrWorkflowID, a.WorkflowID),
		attribute.String(AttrRunID, a.RunID),
	}, map[string]string{
		AttrWorkflowVersion: a.WorkflowVersion,
		AttrItemID:          a.ItemID,
		AttrItemProvider:    a.ItemProvider,
		AttrTrigger:         a.Trigger,
	})
}

func taskAttributeSet(a TaskAttributes) []attribute.KeyValue {
	return withOptional([]attribute.KeyValue{
		attribute.String(AttrSpanKind, SpanKindTask),
		attribute.String(AttrGaggle, a.Gaggle),
		attribute.String(AttrWorkflowID, a.WorkflowID),
		attribute.String(AttrRunID, a.RunID),
		attribute.String(AttrTaskID, a.TaskID),
	}, map[string]string{
		AttrWorkflowVersion: a.WorkflowVersion,
		AttrTaskType:        a.TaskType,
		AttrGooberID:        a.GooberID,
		AttrItemID:          a.ItemID,
		AttrItemProvider:    a.ItemProvider,
	})
}

func gateAttributeSet(a GateAttributes) []attribute.KeyValue {
	return withOptional([]attribute.KeyValue{
		attribute.String(AttrSpanKind, SpanKindGate),
		attribute.String(AttrGaggle, a.Gaggle),
		attribute.String(AttrWorkflowID, a.WorkflowID),
		attribute.String(AttrRunID, a.RunID),
		attribute.String(AttrGateID, a.GateID),
	}, map[string]string{
		AttrWorkflowVersion: a.WorkflowVersion,
		AttrGateEvaluator:   a.Evaluator,
		AttrGateDecision:    a.Decision,
		AttrGooberID:        a.GooberID,
		AttrItemID:          a.ItemID,
		AttrItemProvider:    a.ItemProvider,
	})
}

func schedulerAttributeSet(a SchedulerAttributes) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(AttrSpanKind, SpanKindScheduler),
		attribute.String(AttrGaggle, a.Gaggle),
		attribute.String(AttrWorkflowID, a.WorkflowID),
		attribute.String(AttrSchedulerAction, a.Action),
	}
	return withOptional(attrs, map[string]string{
		AttrWorkflowVersion: a.WorkflowVersion,
		AttrRunID:           a.RunID,
		AttrSchedulerReason: a.Reason,
		AttrItemID:          a.ItemID,
		AttrItemProvider:    a.ItemProvider,
	})
}

func withOptional(attrs []attribute.KeyValue, values map[string]string) []attribute.KeyValue {
	for key, value := range values {
		if value != "" {
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	return attrs
}
