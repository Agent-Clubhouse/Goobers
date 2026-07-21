package telemetry

import semconv "go.opentelemetry.io/otel/semconv/v1.37.0"

// Attribute is a key in the canonical Goobers span attribute registry.
type Attribute string

// The canonical span attribute registry. Add new Goobers attributes here first.
const (
	AttrRunID                  = "goobers.run.id"
	AttrGaggle                 = "goobers.gaggle"
	AttrWorkflow               = "goobers.workflow"
	AttrWorkflowVersion        = "goobers.workflow.version"
	AttrWorkflowDigest         = "goobers.workflow.digest"
	AttrGoober                 = "goobers.goober"
	AttrStage                  = "goobers.stage"
	AttrStageType              = "goobers.stage.type"
	AttrAttemptNumber          = "goobers.attempt.n"
	AttrAttemptKind            = "goobers.attempt.kind"
	AttrItemID                 = "goobers.item.id"
	AttrItemURL                = "goobers.item.url"
	AttrOutcome                = "goobers.outcome"
	AttrErrorCode              = "goobers.error.code"
	AttrGateDecision           = "goobers.gate.decision"
	AttrGateRepassNumber       = "goobers.gate.repass.n"
	AttrErrorType              = string(semconv.ErrorTypeKey)
	AttrGenAIUsageInputTokens  = string(semconv.GenAIUsageInputTokensKey)
	AttrGenAIUsageOutputTokens = string(semconv.GenAIUsageOutputTokensKey)
	AttrCopilotPremiumRequests = "goobers.usage.copilot_premium_requests"
	AttrUsageCostUSD           = "goobers.usage.cost_usd"
)

// AllAttributes returns every canonical attribute in declaration order.
func AllAttributes() []Attribute {
	return []Attribute{
		AttrRunID,
		AttrGaggle,
		AttrWorkflow,
		AttrWorkflowVersion,
		AttrWorkflowDigest,
		AttrGoober,
		AttrStage,
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
		Attribute(AttrGenAIUsageInputTokens),
		Attribute(AttrGenAIUsageOutputTokens),
		AttrCopilotPremiumRequests,
		AttrUsageCostUSD,
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
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	WorkflowDigest  string
	RunID           string
	ItemID          string
	ItemURL         string
}

// TaskAttributes describes one task attempt span.
type TaskAttributes struct {
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	WorkflowDigest  string
	RunID           string
	TaskID          string
	TaskType        string
	GooberID        string
	Attempt         int
	AttemptKind     string
	ItemID          string
	ItemURL         string
}

// GateAttributes describes one gate evaluation span.
type GateAttributes struct {
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	WorkflowDigest  string
	RunID           string
	GateID          string
	Decision        string
	RepassNumber    int
	GooberID        string
	ItemID          string
	ItemURL         string
}

// SchedulerAttributes describes a scheduler decision span.
type SchedulerAttributes struct {
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	WorkflowDigest  string
	RunID           string
	Action          string
	ItemID          string
	ItemURL         string
}
