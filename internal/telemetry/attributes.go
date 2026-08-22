package telemetry

import (
	"time"

	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// Attribute is a key in the canonical Goobers span attribute registry.
type Attribute string

// The canonical span attribute registry. Add new Goobers attributes here first.
const (
	AttrRunID                         = "goobers.run.id"
	AttrGaggle                        = "goobers.gaggle"
	AttrWorkflow                      = "goobers.workflow"
	AttrWorkflowVersion               = "goobers.workflow.version"
	AttrWorkflowDigest                = "goobers.workflow.digest"
	AttrGooberDigest                  = "goobers.goober.digest"
	AttrGoober                        = "goobers.goober"
	AttrModel                         = "goobers.model"
	AttrHarnessVersion                = "goobers.harness.version"
	AttrStage                         = "goobers.stage"
	AttrBranch                        = "goobers.branch"
	AttrStageType                     = "goobers.stage.type"
	AttrAttemptNumber                 = "goobers.attempt.n"
	AttrAttemptKind                   = "goobers.attempt.kind"
	AttrItemID                        = "goobers.item.id"
	AttrItemURL                       = "goobers.item.url"
	AttrOutcome                       = "goobers.outcome"
	AttrErrorCode                     = "goobers.error.code"
	AttrGateDecision                  = "goobers.gate.decision"
	AttrGateRepassNumber              = "goobers.gate.repass.n"
	AttrErrorType                     = string(semconv.ErrorTypeKey)
	AttrGenAIResponseModel            = string(semconv.GenAIResponseModelKey)
	AttrGenAIUsageInputTokens         = string(semconv.GenAIUsageInputTokensKey)
	AttrGenAIUsageOutputTokens        = string(semconv.GenAIUsageOutputTokensKey)
	AttrCopilotPremiumRequests        = "goobers.usage.copilot_premium_requests"
	AttrUsageCostUSD                  = "goobers.usage.cost_usd"
	AttrWorktreeID                    = "goobers.worktree.id"
	AttrStorageOperation              = "goobers.storage.operation"
	AttrUnmeasuredWorktrees           = "goobers.storage.unmeasured_worktrees"
	AttrErrorMessage                  = "goobers.error.message"
	AttrAgentID                       = "goobers.agent.id"
	AttrAgentParentID                 = "goobers.agent.parent_id"
	AttrAgentLifecycle                = "goobers.agent.lifecycle"
	AttrAgentRequestedModel           = "goobers.agent.requested_model"
	AttrAgentResolvedModel            = "goobers.agent.resolved_model"
	AttrAgentRequestedReasoningEffort = "goobers.agent.requested_reasoning_effort"
	AttrAgentResolvedReasoningEffort  = "goobers.agent.resolved_reasoning_effort"
	AttrAgentFidelity                 = "goobers.agent.fidelity"
	AttrAgentPlugin                   = "goobers.agent.plugin"
	AttrAgentMessageID                = "goobers.agent.message.id"
	AttrAgentMessageSenderID          = "goobers.agent.message.sender_id"
	AttrAgentMessageRecipientID       = "goobers.agent.message.recipient_id"
	AttrAgentMessagePurpose           = "goobers.agent.message.purpose"
)

// AllAttributes returns every canonical attribute in declaration order.
func AllAttributes() []Attribute {
	return []Attribute{
		AttrRunID,
		AttrGaggle,
		AttrWorkflow,
		AttrWorkflowVersion,
		AttrWorkflowDigest,
		AttrGooberDigest,
		AttrGoober,
		AttrModel,
		AttrHarnessVersion,
		AttrStage,
		AttrBranch,
		AttrStageType,
		AttrAttemptNumber,
		AttrAttemptKind,
		AttrItemID,
		AttrItemURL,
		AttrOutcome,
		AttrErrorCode,
		AttrGateDecision,
		AttrGateRepassNumber,
		Attribute(AttrErrorType),
		Attribute(AttrGenAIResponseModel),
		Attribute(AttrGenAIUsageInputTokens),
		Attribute(AttrGenAIUsageOutputTokens),
		AttrCopilotPremiumRequests,
		AttrUsageCostUSD,
		AttrWorktreeID,
		AttrStorageOperation,
		AttrUnmeasuredWorktrees,
		AttrErrorMessage,
		AttrAgentID,
		AttrAgentParentID,
		AttrAgentLifecycle,
		AttrAgentRequestedModel,
		AttrAgentResolvedModel,
		AttrAgentRequestedReasoningEffort,
		AttrAgentResolvedReasoningEffort,
		AttrAgentFidelity,
		AttrAgentPlugin,
		AttrAgentMessageID,
		AttrAgentMessageSenderID,
		AttrAgentMessageRecipientID,
		AttrAgentMessagePurpose,
	}
}

// KnownAttribute reports whether key is in the canonical registry.
func KnownAttribute(key string) bool {
	for _, attr := range AllAttributes() {
		if string(attr) == key {
			return true
		}
	}
	return false
}

// Canonical values for span stage types, attempt kinds, and outcomes.
const (
	StageTypeDeterministic = "deterministic"
	StageTypeAgentic       = "agentic"
	StageTypeGate          = "gate"
	StageTypeScheduler     = "scheduler"

	AttemptKindPolicy = "policy"
	AttemptKindInfra  = "infra"

	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeBlocked = "blocked"
)

const (
	// SpanKindRun marks the root workflow run span in journal span records.
	SpanKindRun = "run"
	// SpanKindTask marks a workflow task span in journal span records.
	SpanKindTask = "task"
	// SpanKindGate marks a workflow gate span in journal span records.
	SpanKindGate = "gate"
	// SpanKindScheduler marks a scheduler decision span in journal span records.
	SpanKindScheduler = "scheduler"
)

// RunAttributes describes a workflow run root span.
type RunAttributes struct {
	// StartedAt backdates the span to when the run actually began, instead of
	// stamping it at creation. Zero keeps the default (now), which is what a
	// live tier-1 run wants.
	//
	// It exists for the engine path: a tier-3 run's spans are synthesized
	// AFTER the fact, from workflow history via the journal projection, so a
	// span stamped at synthesis time would report the wrong moment and a
	// meaningless duration. WithTimestamp is already how this package backdates
	// span EVENTS (span.go:125); this extends the same idea to span starts.
	StartedAt       time.Time
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	WorkflowDigest  string
	GooberDigest    string
	RunID           string
	ItemID          string
	ItemURL         string
}

// TaskAttributes describes one task attempt span.
type TaskAttributes struct {
	// StartedAt backdates the span; see RunAttributes.StartedAt.
	StartedAt       time.Time
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	WorkflowDigest  string
	GooberDigest    string
	RunID           string
	TaskID          string
	Branch          int
	TaskType        string
	GooberID        string
	Model           string
	HarnessVersion  string
	Attempt         int
	AttemptKind     string
	ItemID          string
	ItemURL         string
}

// GateAttributes describes one gate evaluation span.
type GateAttributes struct {
	// StartedAt backdates the span; see RunAttributes.StartedAt.
	StartedAt       time.Time
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	WorkflowDigest  string
	GooberDigest    string
	RunID           string
	GateID          string
	Decision        string
	RepassNumber    int
	GooberID        string
	Agentic         bool
	Model           string
	HarnessVersion  string
	ItemID          string
	ItemURL         string
}

// SchedulerAttributes describes a scheduler decision span.
type SchedulerAttributes struct {
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	WorkflowDigest  string
	GooberDigest    string
	RunID           string
	Action          string
	ItemID          string
	ItemURL         string
}
