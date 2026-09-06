package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/goobers/goobers/internal/journal"
)

const (
	// NestedAgentLifecycleEventName identifies lifecycle telemetry events.
	NestedAgentLifecycleEventName = "goobers.agent.lifecycle"
	// NestedAgentMessageEventName identifies peer-message telemetry events.
	NestedAgentMessageEventName = "goobers.agent.message"
)

// RecordAgentProvenance annotates the active task or gate span. Empty values
// remain explicit so every harness invocation has both provenance dimensions.
func RecordAgentProvenance(ctx context.Context, model, harnessVersion string) {
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String(AttrModel, model),
		attribute.String(AttrHarnessVersion, harnessVersion),
	)
}

// RecordNestedAgent projects structured adapter provenance onto a span without
// retaining assignment or peer-message content in telemetry.
func RecordNestedAgent(ctx context.Context, agent journal.AgentProvenance) {
	attrs := []attribute.KeyValue{
		attribute.String(AttrAgentID, agent.ID),
		attribute.String(AttrAgentLifecycle, string(agent.Lifecycle)),
	}
	attrs = appendOptionalString(attrs, AttrAgentParentID, agent.ParentID)
	attrs = appendOptionalString(attrs, AttrAgentPlugin, agent.Plugin)
	attrs = appendOptionalString(attrs, AttrAgentRequestedModel, agent.RequestedModel)
	attrs = appendOptionalString(attrs, AttrAgentResolvedModel, agent.ResolvedModel)
	attrs = appendOptionalString(attrs, AttrAgentRequestedReasoningEffort, agent.RequestedReasoningEffort)
	attrs = appendOptionalString(attrs, AttrAgentResolvedReasoningEffort, agent.ResolvedReasoningEffort)
	attrs = appendOptionalString(attrs, AttrAgentFidelity, agent.Fidelity)
	if agent.Usage.InputTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrGenAIUsageInputTokens, *agent.Usage.InputTokens))
	}
	if agent.Usage.OutputTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrGenAIUsageOutputTokens, *agent.Usage.OutputTokens))
	}
	if agent.Usage.Model != "" {
		attrs = append(attrs, attribute.String(AttrGenAIResponseModel, agent.Usage.Model))
	}
	if agent.Usage.CacheReadTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrUsageCacheReadTokens, *agent.Usage.CacheReadTokens))
	}
	if agent.Usage.CacheWriteTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrUsageCacheWriteTokens, *agent.Usage.CacheWriteTokens))
	}
	if agent.Usage.ReasoningTokens != nil {
		attrs = append(attrs, attribute.Int64(AttrUsageReasoningTokens, *agent.Usage.ReasoningTokens))
	}
	if agent.Usage.NanoAIU != nil {
		attrs = append(attrs,
			attribute.Int64(AttrUsageNanoAIU, *agent.Usage.NanoAIU),
			attribute.Float64(AttrUsageCostUSD, NanoAIUToUSD(*agent.Usage.NanoAIU)),
		)
	} else if agent.Usage.CostUSD != nil {
		attrs = append(attrs, attribute.Float64(AttrUsageCostUSD, *agent.Usage.CostUSD))
	}
	options := []trace.EventOption{trace.WithAttributes(attrs...)}
	if !agent.UpdatedAt.IsZero() {
		options = append(options, trace.WithTimestamp(agent.UpdatedAt))
	}
	trace.SpanFromContext(ctx).AddEvent(NestedAgentLifecycleEventName, options...)
}

// RecordNestedAgentMessage retains only orchestration metadata. The normalized
// contract has no raw message body field.
func RecordNestedAgentMessage(ctx context.Context, message journal.PeerMessageMetadata) {
	attrs := []attribute.KeyValue{
		attribute.String(AttrAgentMessageID, message.ID),
		attribute.String(AttrAgentMessageSenderID, message.SenderID),
		attribute.String(AttrAgentMessageRecipientID, message.RecipientID),
		attribute.String(AttrAgentMessagePurpose, message.Purpose),
	}
	options := []trace.EventOption{trace.WithAttributes(attrs...)}
	if !message.OccurredAt.IsZero() {
		options = append(options, trace.WithTimestamp(message.OccurredAt))
	}
	trace.SpanFromContext(ctx).AddEvent(NestedAgentMessageEventName, options...)
}

func runAttributeSet(a RunAttributes) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(AttrRunID, a.RunID),
		attribute.String(AttrGaggle, a.Gaggle),
		attribute.String(AttrWorkflow, a.WorkflowID),
	}
	attrs = appendOptionalString(attrs, AttrWorkflowVersion, a.WorkflowVersion)
	attrs = appendOptionalString(attrs, AttrWorkflowDigest, a.WorkflowDigest)
	attrs = appendOptionalString(attrs, AttrGooberDigest, a.GooberDigest)
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
		attribute.Int(AttrBranch, a.Branch),
		attribute.Int(AttrAttemptNumber, attempt),
	}
	attrs = appendOptionalString(attrs, AttrWorkflowVersion, a.WorkflowVersion)
	attrs = appendOptionalString(attrs, AttrWorkflowDigest, a.WorkflowDigest)
	attrs = appendOptionalString(attrs, AttrGooberDigest, a.GooberDigest)
	attrs = appendOptionalString(attrs, AttrGoober, a.GooberID)
	attrs = appendOptionalString(attrs, AttrStageType, a.TaskType)
	if a.TaskType == StageTypeAgentic {
		attrs = append(attrs,
			attribute.String(AttrModel, a.Model),
			attribute.String(AttrHarnessVersion, a.HarnessVersion),
		)
	}
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
	attrs = appendOptionalString(attrs, AttrGooberDigest, a.GooberDigest)
	attrs = appendOptionalString(attrs, AttrGoober, a.GooberID)
	if a.Agentic {
		attrs = append(attrs,
			attribute.String(AttrModel, a.Model),
			attribute.String(AttrHarnessVersion, a.HarnessVersion),
		)
	}
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
	attrs = appendOptionalString(attrs, AttrGooberDigest, a.GooberDigest)
	attrs = appendOptionalString(attrs, AttrItemID, a.ItemID)
	return appendOptionalString(attrs, AttrItemURL, a.ItemURL)
}

func appendOptionalString(attrs []attribute.KeyValue, key, value string) []attribute.KeyValue {
	if value == "" {
		return attrs
	}
	return append(attrs, attribute.String(key, value))
}
