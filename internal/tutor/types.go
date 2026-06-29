package tutor

import (
	"context"
	"time"

	"github.com/goobers/goobers/providers"
)

// SignalKind identifies the telemetry span category the Tutor can mine.
type SignalKind string

const (
	// SignalRun is the root workflow run telemetry signal.
	SignalRun SignalKind = "run"
	// SignalTask is a workflow task telemetry signal.
	SignalTask SignalKind = "task"
	// SignalGate is a workflow gate telemetry signal.
	SignalGate SignalKind = "gate"
)

// Signal is the Tutor's provider-neutral telemetry model. Implementations may
// populate it from ADX queries, OTel spans, or deterministic test fixtures.
type Signal struct {
	Kind            SignalKind
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	RunID           string
	TaskID          string
	GateID          string
	GooberID        string
	Decision        string
	Status          string
	Error           string
	Duration        time.Duration
	RetryCount      int
	EvidenceURL     string
	ObservedAt      time.Time
}

// Query constrains telemetry reads.
type Query struct {
	Gaggle string
	Since  time.Time
	Until  time.Time
}

// TelemetryStore abstracts the telemetry backend. The production implementation
// can query ADX, while tests and local runs can use OTel span snapshots.
type TelemetryStore interface {
	QuerySignals(ctx context.Context, q Query) ([]Signal, error)
}

// FindingType identifies a recurring improvement trigger.
type FindingType string

const (
	// FindingGateRejection flags repeated negative gate decisions.
	FindingGateRejection FindingType = "gate-rejection"
	// FindingTaskFailure flags repeated task errors.
	FindingTaskFailure FindingType = "task-failure"
	// FindingSlowTask flags repeatedly slow task executions.
	FindingSlowTask FindingType = "slow-task"
	// FindingRetries flags retry-heavy task executions.
	FindingRetries FindingType = "retries"
)

// Evidence captures the concrete telemetry samples backing a finding.
type Evidence struct {
	RunID      string
	Signal     string
	Status     string
	Decision   string
	Duration   time.Duration
	RetryCount int
	URL        string
	ObservedAt time.Time
}

// Finding is a deterministic candidate problem surfaced from telemetry.
type Finding struct {
	Type           FindingType
	Severity       string
	Gaggle         string
	WorkflowID     string
	TaskID         string
	GateID         string
	GooberID       string
	Observed       int
	ProblemCount   int
	Rate           float64
	Rationale      string
	Recommendation string
	Evidence       []Evidence
}

// Proposal is a config-repo change the Tutor is ready to commit.
type Proposal struct {
	Finding    Finding
	BranchName string
	Title      string
	Body       string
	Files      []providers.CommitFile
}
