package v1alpha1

import "time"

// GaggleHealthSchemaVersion versions every gaggle-health wire payload.
const GaggleHealthSchemaVersion = "goobers.dev/gaggle-health/v1alpha1"

// GaggleHealthState is a gaggle's aggregate health, or one finding's
// contribution to it.
type GaggleHealthState string

// Aggregate health states, in ascending precedence from healthy to
// operator-required.
const (
	GaggleHealthHealthy          GaggleHealthState = "healthy"
	GaggleHealthDegraded         GaggleHealthState = "degraded"
	GaggleHealthStalled          GaggleHealthState = "stalled"
	GaggleHealthInconsistent     GaggleHealthState = "inconsistent"
	GaggleHealthRecovering       GaggleHealthState = "recovering"
	GaggleHealthOperatorRequired GaggleHealthState = "operator-required"
)

// GaggleHealthSeverity ranks a finding's urgency.
type GaggleHealthSeverity string

// Finding severities, in ascending order.
const (
	GaggleHealthSeverityInfo     GaggleHealthSeverity = "info"
	GaggleHealthSeverityWarning  GaggleHealthSeverity = "warning"
	GaggleHealthSeverityError    GaggleHealthSeverity = "error"
	GaggleHealthSeverityCritical GaggleHealthSeverity = "critical"
)

// GaggleHealthRemediationMode selects how a finding code is handled.
type GaggleHealthRemediationMode string

// Per-finding remediation modes; repair is only valid for codes the hard
// safety policy classifies as idempotent.
const (
	GaggleHealthObserve    GaggleHealthRemediationMode = "observe"
	GaggleHealthRepairMode GaggleHealthRemediationMode = "repair"
	GaggleHealthEscalate   GaggleHealthRemediationMode = "escalate"
)

// GaggleHealthRepairDisposition records whether and how a repair ran.
type GaggleHealthRepairDisposition string

// Repair dispositions.
const (
	GaggleHealthRepairNotAttempted GaggleHealthRepairDisposition = "not-attempted"
	GaggleHealthRepairStarted      GaggleHealthRepairDisposition = "started"
	GaggleHealthRepairSucceeded    GaggleHealthRepairDisposition = "succeeded"
	GaggleHealthRepairFailed       GaggleHealthRepairDisposition = "failed"
	GaggleHealthRepairRefused      GaggleHealthRepairDisposition = "refused"
)

// GaggleHealthFollowUpState tracks what happens after a repair decision.
type GaggleHealthFollowUpState string

// Repair follow-up states.
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

// GaggleHealthRepair separates the recommended action and its policy
// authorization from the attempted repair and its observed result.
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

// GaggleHealthEventType names one journaled health transition.
type GaggleHealthEventType string

// Journaled health transitions.
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

// GaggleHealthResponse is the read-API envelope for one gaggle's health.
type GaggleHealthResponse struct {
	SchemaVersion string               `json:"schemaVersion"`
	Health        GaggleHealthSnapshot `json:"health"`
}
