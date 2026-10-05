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
