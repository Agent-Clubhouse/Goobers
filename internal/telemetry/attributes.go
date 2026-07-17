package telemetry

const (
	// AttrSpanKind identifies the Goobers semantic kind for a span.
	AttrSpanKind = "goobers.span.kind"
	// AttrGaggle is the gaggle that owns the run.
	AttrGaggle = "gaggle"
	// AttrWorkflowID is the workflow definition id.
	AttrWorkflowID = "workflowId"
	// AttrWorkflowVersion is the pinned workflow definition version.
	AttrWorkflowVersion = "workflow.version"
	// AttrRunID is the workflow run id and OTel trace id.
	AttrRunID = "runId"
	// AttrItemID is the provider-native backlog item id.
	AttrItemID = "item.id"
	// AttrItemProvider is the backing backlog provider.
	AttrItemProvider = "item.provider"
	// AttrTrigger is the scheduler trigger that admitted the run.
	AttrTrigger = "trigger"
	// AttrTaskID is the task state id within the workflow.
	AttrTaskID = "taskId"
	// AttrTaskType identifies agentic versus deterministic tasks.
	AttrTaskType = "task.type"
	// AttrGooberID is the goober definition executing the task or review.
	AttrGooberID = "goober.id"
	// AttrGateID is the gate state id within the workflow.
	AttrGateID = "gateId"
	// AttrGateEvaluator identifies automated, agentic, or human evaluators.
	AttrGateEvaluator = "gate.evaluator"
	// AttrGateDecision records the gate outcome.
	AttrGateDecision = "gate.decision"
	// AttrSchedulerAction records the scheduler decision/action.
	AttrSchedulerAction = "scheduler.action"
	// AttrSchedulerReason records the scheduler's rationale.
	AttrSchedulerReason = "scheduler.reason"
	// AttrBusinessStatus records a run/stage span's actual business outcome
	// (issue #710) — set via Span.Complete, distinct from OTel's own Ok/Error
	// status axis (which Complete also sets, but coarser: only a business
	// failure maps to codes.Error, every other outcome — success, completed,
	// aborted, escalated — stays codes.Ok).
	AttrBusinessStatus = "goobers.business_status"
)

const (
	// SpanKindRun marks the root workflow run span.
	SpanKindRun = "run"
	// SpanKindTask marks a workflow task span.
	SpanKindTask = "task"
	// SpanKindGate marks a workflow gate span.
	SpanKindGate = "gate"
	// SpanKindScheduler marks a scheduler decision span.
	SpanKindScheduler = "scheduler"
)

// RunAttributes describes a workflow run root span.
type RunAttributes struct {
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	RunID           string
	ItemID          string
	ItemProvider    string
	Trigger         string
}

// TaskAttributes describes a task execution span.
type TaskAttributes struct {
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	RunID           string
	TaskID          string
	TaskType        string
	GooberID        string
	ItemID          string
	ItemProvider    string
}

// GateAttributes describes a gate evaluation span.
type GateAttributes struct {
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	RunID           string
	GateID          string
	Evaluator       string
	Decision        string
	GooberID        string
	ItemID          string
	ItemProvider    string
}

// SchedulerAttributes describes a scheduler decision span.
type SchedulerAttributes struct {
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	RunID           string
	Action          string
	Reason          string
	ItemID          string
	ItemProvider    string
}
