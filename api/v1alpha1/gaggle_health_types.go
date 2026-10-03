package v1alpha1

import "time"

const GaggleHealthSchemaVersion = "goobers.dev/gaggle-health/v1alpha1"

type GaggleHealthState string

const (
	GaggleHealthHealthy          GaggleHealthState = "healthy"
	GaggleHealthDegraded         GaggleHealthState = "degraded"
	GaggleHealthStalled          GaggleHealthState = "stalled"
	GaggleHealthInconsistent     GaggleHealthState = "inconsistent"
	GaggleHealthRecovering       GaggleHealthState = "recovering"
	GaggleHealthOperatorRequired GaggleHealthState = "operator-required"
)

type GaggleHealthSeverity string

const (
	GaggleHealthSeverityInfo     GaggleHealthSeverity = "info"
	GaggleHealthSeverityWarning  GaggleHealthSeverity = "warning"
	GaggleHealthSeverityError    GaggleHealthSeverity = "error"
	GaggleHealthSeverityCritical GaggleHealthSeverity = "critical"
)

type GaggleHealthRemediationMode string

const (
	GaggleHealthObserve    GaggleHealthRemediationMode = "observe"
	GaggleHealthRepairMode GaggleHealthRemediationMode = "repair"
	GaggleHealthEscalate   GaggleHealthRemediationMode = "escalate"
)

type GaggleHealthRepairDisposition string

const (
	GaggleHealthRepairNotAttempted GaggleHealthRepairDisposition = "not-attempted"
	GaggleHealthRepairStarted      GaggleHealthRepairDisposition = "started"
	GaggleHealthRepairSucceeded    GaggleHealthRepairDisposition = "succeeded"
	GaggleHealthRepairFailed       GaggleHealthRepairDisposition = "failed"
	GaggleHealthRepairRefused      GaggleHealthRepairDisposition = "refused"
)

type GaggleHealthFollowUpState string

const (
	GaggleHealthFollowUpNone      GaggleHealthFollowUpState = "none"
	GaggleHealthFollowUpVerifying GaggleHealthFollowUpState = "verifying"
	GaggleHealthFollowUpResolved  GaggleHealthFollowUpState = "resolved"
	GaggleHealthFollowUpEscalated GaggleHealthFollowUpState = "escalated"
)

// GaggleHealthIdentity scopes a finding without exposing environment values or
// host paths. Empty optional identities are omitted from the wire contract.
type GaggleHealthIdentity struct {
	Gaggle      string `json:"gaggle"`
	Workflow    string `json:"workflow,omitempty"`
	Run         string `json:"run,omitempty"`
	Stage       string `json:"stage,omitempty"`
	BacklogItem string `json:"backlogItem,omitempty"`
	PullRequest string `json:"pullRequest,omitempty"`
	Claim       string `json:"claim,omitempty"`
	Runner      string `json:"runner,omitempty"`
	Worker      string `json:"worker,omitempty"`
}

// GaggleHealthEvidence references authoritative, already-redacted evidence.
// Detail is bounded by schema and must never contain raw logs or private paths.
type GaggleHealthEvidence struct {
	Kind     string `json:"kind"`
	Run      string `json:"run,omitempty"`
	Sequence uint64 `json:"sequence,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type GaggleHealthRepair struct {
	RecommendedAction string                        `json:"recommendedAction,omitempty"`
	PolicyAuthorized  bool                          `json:"policyAuthorized"`
	Disposition       GaggleHealthRepairDisposition `json:"disposition"`
	AttemptedAt       *time.Time                    `json:"attemptedAt,omitempty"`
	CompletedAt       *time.Time                    `json:"completedAt,omitempty"`
	IdempotencyKey    string                        `json:"idempotencyKey,omitempty"`
	ResultSummary     string                        `json:"resultSummary,omitempty"`
	FollowUp          GaggleHealthFollowUpState     `json:"followUp"`
}

// GaggleHealthFinding separates detection evidence, recommendation, policy
// authorization, repair attempt, observed result, and eventual resolution.
type GaggleHealthFinding struct {
	SchemaVersion      string                 `json:"schemaVersion"`
	Code               string                 `json:"code"`
	Severity           GaggleHealthSeverity   `json:"severity"`
	Contribution       GaggleHealthState      `json:"contribution"`
	Identity           GaggleHealthIdentity   `json:"identity"`
	FirstObserved      time.Time              `json:"firstObserved"`
	LastObserved       time.Time              `json:"lastObserved"`
	ObservationCount   uint64                 `json:"observationCount"`
	EpisodeKey         string                 `json:"episodeKey"`
	Evidence           []GaggleHealthEvidence `json:"evidence"`
	Summary            string                 `json:"summary"`
	Confidence         float64                `json:"confidence"`
	EvidenceAssessment string                 `json:"evidenceAssessment"`
	Repair             GaggleHealthRepair     `json:"repair"`
	ResolvedAt         *time.Time             `json:"resolvedAt,omitempty"`
	ResolutionEvidence []GaggleHealthEvidence `json:"resolutionEvidence,omitempty"`
}

type GaggleHealthEventType string

const (
	GaggleHealthEvaluated           GaggleHealthEventType = "evaluation"
	GaggleHealthFindingOpened       GaggleHealthEventType = "finding-opened"
	GaggleHealthFindingUpdated      GaggleHealthEventType = "finding-updated"
	GaggleHealthRepairStartedEvent  GaggleHealthEventType = "repair-started"
	GaggleHealthRepairFinishedEvent GaggleHealthEventType = "repair-finished"
	GaggleHealthEscalated           GaggleHealthEventType = "escalation"
	GaggleHealthFindingResolved     GaggleHealthEventType = "finding-resolved"
)

// GaggleHealthEvent is the append-only instance-journal payload. Finding is a
// complete bounded snapshot, making projection rebuild independent of process state.
type GaggleHealthEvent struct {
	SchemaVersion string                `json:"schemaVersion"`
	Sequence      uint64                `json:"sequence"`
	OccurredAt    time.Time             `json:"occurredAt"`
	Type          GaggleHealthEventType `json:"type"`
	Gaggle        string                `json:"gaggle"`
	EpisodeKey    string                `json:"episodeKey,omitempty"`
	Finding       *GaggleHealthFinding  `json:"finding,omitempty"`
}

// GaggleHealthSnapshot is the restart-recoverable derived projection for one gaggle.
type GaggleHealthSnapshot struct {
	SchemaVersion string                `json:"schemaVersion"`
	Gaggle        string                `json:"gaggle"`
	State         GaggleHealthState     `json:"state"`
	UpdatedAt     time.Time             `json:"updatedAt"`
	Active        []GaggleHealthFinding `json:"active"`
	History       []GaggleHealthFinding `json:"history"`
	LastSequence  uint64                `json:"lastSequence"`
}

type GaggleHealthResponse struct {
	SchemaVersion string               `json:"schemaVersion"`
	Health        GaggleHealthSnapshot `json:"health"`
}
