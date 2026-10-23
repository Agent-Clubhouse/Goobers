package triggerqueue

import (
	"context"
	"time"
)

// AcceptanceStore is the ordinary durable acceptance and dispatch custody used
// by scheduler policy. Child-workflow storage deliberately stays outside it.
type AcceptanceStore interface {
	Accept(context.Context, string, string, []byte, time.Time) (Record, bool, error)
	ByKey(context.Context, string) (Record, error)
	Get(context.Context, string, string) (Record, error)
	Pending(context.Context, int) ([]Record, error)
	RetainedPage(context.Context, string, int) ([]Record, error)
	BeginDispatch(context.Context, string) error
	Requeue(context.Context, string, string) error
	RecordDispatch(context.Context, string, string) error
	Finish(context.Context, string, State, string, string, time.Time) error
	Close() error
}

// SourceAcceptanceStore atomically records source receipts, recipient starts,
// schedule demand, and workflow pending limits.
type SourceAcceptanceStore interface {
	SourceReceipt(context.Context, string) (SourceReceipt, error)
	AcceptSource(context.Context, SourceBatch, time.Time) (SourceReceipt, bool, error)
	PendingWorkflowStarts(context.Context, WorkflowPendingLimit) (int, error)
	CaptureScheduleDemand(context.Context, ScheduleDemandCapture) error
	ScheduleDemand(context.Context, string) (ScheduleDemand, error)
	ScheduleDemandByID(context.Context, string) (ScheduleDemand, error)
	ScheduleDemandPage(context.Context, string, int) ([]ScheduleDemand, error)
	ObserveScheduleDemand(context.Context, string, int) error
}

// ScheduleCursorStore owns durable coalesced and per-declaration schedule
// positions independently from scheduler process memory.
type ScheduleCursorStore interface {
	SourceCursorRevision(context.Context, string, string, time.Time, time.Time, bool) (time.Time, bool, error)
	SourceOccurrences(context.Context, string, string, int) ([]time.Time, bool, error)
	ResetExistingSourceRevision(context.Context, string, string, time.Time) error
}

// RetentionStore exposes the scheduler-policy rows that pin applied
// configuration generations.
type RetentionStore interface {
	RetainedPage(context.Context, string, int) ([]Record, error)
	ScheduleDemandPage(context.Context, string, int) ([]ScheduleDemand, error)
}

// OrdinaryStore is the accepted-trigger lifecycle plus generation retention
// needed by ordinary manual admission.
type OrdinaryStore interface {
	AcceptanceStore
	RetentionStore
}

// SchedulerPolicyStore is the complete triggerqueue surface used by local
// scheduler policy. The SQLite Store satisfies it; future shared backends use
// the same semantic suite.
type SchedulerPolicyStore interface {
	AcceptanceStore
	SourceAcceptanceStore
	ScheduleCursorStore
	RetentionStore
}

var _ SchedulerPolicyStore = (*Store)(nil)
