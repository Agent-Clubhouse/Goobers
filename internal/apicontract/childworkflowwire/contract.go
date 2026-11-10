// Package childworkflowwire is the dependency-free child workflow transport contract.
package childworkflowwire

import "time"

// ChildWorkflowSourceRequest carries authored DSL only. Authority is never body data.
type ChildWorkflowSourceRequest struct {
	Source string `json:"source"`
}

// ChildWorkflowStatusRequest selects an invocation within the authenticated occurrence.
type ChildWorkflowStatusRequest struct {
	InvocationKey string `json:"invocationKey"`
}

// ChildWorkflowResolveRequest chooses disposition of an exact terminal result.
type ChildWorkflowResolveRequest struct {
	InvocationKey         string `json:"invocationKey"`
	Action                string `json:"action"`
	ResultRef             string `json:"resultRef"`
	ExpectedRequestDigest string `json:"expectedRequestDigest,omitempty"`
}

// ChildWorkflowResolutionResponse acknowledges a request, separately from its
// verified application. The parent runner yields before editing its workspace.
type ChildWorkflowResolutionResponse struct {
	InvocationKey string     `json:"invocationKey"`
	Action        string     `json:"action"`
	ResultRef     string     `json:"resultRef"`
	RequestedAt   time.Time  `json:"requestedAt"`
	Applied       bool       `json:"applied"`
	RequestDigest string     `json:"requestDigest"`
	PlanPublished bool       `json:"planPublished"`
	AppliedAt     *time.Time `json:"appliedAt,omitempty"`
}

// ChildWorkflowDiagnostic is a bounded, source-free authoring refusal.
type ChildWorkflowDiagnostic struct {
	Code    string `json:"code"`
	Stage   string `json:"stage,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// ChildWorkflowValidationResponse is advisory; start always validates again.
type ChildWorkflowValidationResponse struct {
	Valid           bool                      `json:"valid"`
	Advisory        bool                      `json:"advisory"`
	SourceDigest    string                    `json:"sourceDigest,omitempty"`
	CanonicalDigest string                    `json:"canonicalDigest,omitempty"`
	ConfigDigest    string                    `json:"configDigest,omitempty"`
	PolicyDigest    string                    `json:"policyDigest,omitempty"`
	WorkflowDigest  string                    `json:"workflowDigest,omitempty"`
	Diagnostics     []ChildWorkflowDiagnostic `json:"diagnostics"`
	// Novelty is present for a valid proposal when a cluster model is
	// configured, including an unscored result when the model is unusable.
	Novelty *ChildWorkflowNovelty `json:"novelty,omitempty"`
}

// ChildWorkflowNovelty is bounded advisory cluster evidence. It grants no
// authority; HumanApprovalRequired reports only the configured policy outcome.
type ChildWorkflowNovelty struct {
	Scored                bool                            `json:"scored"`
	Reason                string                          `json:"reason,omitempty"`
	ModelID               string                          `json:"modelId,omitempty"`
	ModelVersion          string                          `json:"modelVersion,omitempty"`
	FeatureVersion        string                          `json:"featureVersion,omitempty"`
	ClusterID             string                          `json:"clusterId,omitempty"`
	Nearest               string                          `json:"nearest,omitempty"`
	NearestMedoid         string                          `json:"nearestMedoid,omitempty"`
	NearestSize           int                             `json:"nearestSize,omitempty"`
	NearestStable         bool                            `json:"nearestStable,omitempty"`
	Distance              float64                         `json:"distance"`
	OutOfDistribution     bool                            `json:"outOfDistribution"`
	HistoricalOutcome     *ChildWorkflowHistoricalOutcome `json:"historicalOutcome,omitempty"`
	HumanApprovalRequired bool                            `json:"humanApprovalRequired,omitempty"`
	Summary               string                          `json:"summary"`
}

// ChildWorkflowHistoricalOutcome is the admission-time cluster outcome snapshot.
type ChildWorkflowHistoricalOutcome struct {
	RunCount         int     `json:"runCount"`
	SuccessRate      float64 `json:"successRate"`
	InsufficientRuns bool    `json:"insufficientRuns"`
}

// ChildWorkflowAdmission is the durable human-review hold of a queued child.
// A pending child is retained but not dispatched until an operator decides.
type ChildWorkflowAdmission struct {
	State       string     `json:"state"`
	Reason      string     `json:"reason"`
	RequestedAt time.Time  `json:"requestedAt"`
	DecidedAt   *time.Time `json:"decidedAt,omitempty"`
}

// ChildWorkflowResponse reports durable custody, not a claim that execution began.
// Result/workspace references do not themselves grant access to those resources.
type ChildWorkflowResponse struct {
	ChildID               string                           `json:"childId"`
	AcceptanceID          string                           `json:"acceptanceId"`
	RunID                 string                           `json:"runId"`
	InvocationKey         string                           `json:"invocationKey"`
	Sequence              int                              `json:"sequence"`
	State                 string                           `json:"state"`
	Duplicate             bool                             `json:"duplicate,omitempty"`
	SourceDigest          string                           `json:"sourceDigest"`
	CanonicalDigest       string                           `json:"canonicalDigest"`
	ConfigDigest          string                           `json:"configDigest"`
	PolicyDigest          string                           `json:"policyDigest"`
	WorkflowDigest        string                           `json:"workflowDigest"`
	CancellationRequested bool                             `json:"cancellationRequested"`
	Acknowledged          bool                             `json:"acknowledged"`
	ResultRef             string                           `json:"resultRef,omitempty"`
	WorkspaceRef          string                           `json:"workspaceRef,omitempty"`
	AcceptedAt            time.Time                        `json:"acceptedAt"`
	UpdatedAt             time.Time                        `json:"updatedAt"`
	Disposition           *ChildWorkflowResolutionResponse `json:"disposition,omitempty"`
	Novelty               *ChildWorkflowNovelty            `json:"novelty,omitempty"`
	Admission             *ChildWorkflowAdmission          `json:"admission,omitempty"`
}

// MaxChildWorkflowSourceBytes caps decoded UTF-8 proposal bytes at admission.
const MaxChildWorkflowSourceBytes = 1 << 20

// MaxChildWorkflowInvocationKeyBytes is shared by start and occurrence-scoped lookup.
const MaxChildWorkflowInvocationKeyBytes = 256

// These paths are shared by the daemon route catalog and stage MCP client.
const (
	ValidatePath = "/api/v1/runs/{run}/child-workflows/validate"
	StartPath    = "/api/v1/runs/{run}/child-workflows/start"
	StatusPath   = "/api/v1/runs/{run}/child-workflows/status"
	ResolvePath  = "/api/v1/runs/{run}/child-workflows/resolve"
)
