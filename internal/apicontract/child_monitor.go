package apicontract

import "time"

// ChildWorkflowPage exposes bounded control-state summaries without generated
// source, credentials, workspace paths or artifact content.
type ChildWorkflowPage struct {
	RunID      string                 `json:"runId"`
	Gaggle     string                 `json:"gaggle"`
	Parent     *ChildWorkflowParent   `json:"parent,omitempty"`
	Children   []ChildWorkflowSummary `json:"children"`
	NextCursor string                 `json:"nextCursor,omitempty"`
}

// ChildWorkflowParent is retained provenance; it does not grant parent access.
type ChildWorkflowParent struct {
	RunID         string `json:"runId"`
	Workflow      string `json:"workflow"`
	InvocationKey string `json:"invocationKey"`
}

// ChildWorkflowSummary distinguishes queued/terminal custody from run existence.
type ChildWorkflowSummary struct {
	ChildID               string    `json:"childId"`
	RunID                 string    `json:"runId,omitempty"`
	RunAvailable          bool      `json:"runAvailable"`
	Stage                 string    `json:"stage,omitempty"`
	Workflow              string    `json:"workflow,omitempty"`
	InvocationKey         string    `json:"invocationKey"`
	Sequence              int       `json:"sequence"`
	State                 string    `json:"state"`
	CancellationRequested bool      `json:"cancellationRequested"`
	Acknowledged          bool      `json:"acknowledged"`
	Expired               bool      `json:"expired"`
	AcceptedAt            time.Time `json:"acceptedAt"`
	UpdatedAt             time.Time `json:"updatedAt"`
}
