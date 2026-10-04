package apicontract

import "time"

// ChildWorkflowPage exposes bounded control-state summaries without generated
// source, credentials, workspace paths or artifact content.
type ChildWorkflowPage struct {
	ExecutionHistory          []ChildWorkflowExecution  `json:"executionHistory,omitempty"`
	PublicationRunID          string                    `json:"publicationRunId,omitempty"`
	Publications              []ChildPublicationSummary `json:"publications,omitempty"`
	PublicationCheckAvailable bool                      `json:"publicationCheckAvailable,omitempty"`
	PublicationCheckReason    string                    `json:"publicationCheckReason,omitempty"`
	RunID                     string                    `json:"runId"`
	Gaggle                    string                    `json:"gaggle"`
	Parent                    *ChildWorkflowParent      `json:"parent,omitempty"`
	Children                  []ChildWorkflowSummary    `json:"children"`
	NextCursor                string                    `json:"nextCursor,omitempty"`
}

// ChildWorkflowParent is retained provenance; it does not grant parent access.
type ChildWorkflowParent struct {
	RunID         string `json:"runId"`
	Workflow      string `json:"workflow"`
	InvocationKey string `json:"invocationKey"`
}

// ChildWorkflowSummary distinguishes queued/terminal custody from run existence.
type ChildWorkflowSummary struct {
	ExecutionEpoch        int       `json:"executionEpoch,omitempty"`
	OriginalRunID         string    `json:"originalRunId,omitempty"`
	PublicationNeedsHuman bool      `json:"publicationNeedsHuman,omitempty"`
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

// ChildWorkflowExecution is bounded historical metadata. Run availability is
// verified against journal custody; this projection grants no effect authority.
type ChildWorkflowExecution struct {
	Epoch        int       `json:"epoch"`
	RunID        string    `json:"runId"`
	RunAvailable bool      `json:"runAvailable"`
	Current      bool      `json:"current"`
	State        string    `json:"state"`
	SourceRunID  string    `json:"sourceRunId,omitempty"`
	Actor        string    `json:"actor,omitempty"`
	Stage        string    `json:"stage,omitempty"`
	AcceptedAt   time.Time `json:"acceptedAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}
