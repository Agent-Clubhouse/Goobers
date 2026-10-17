// Package interactiveops provides scoped, reviewable helper operations for the
// experimental interactive portal. It uses the ordinary Goobers validator,
// config transaction and CLI scheduler; it is never a production admin proxy.
package interactiveops

import (
	"context"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type Scope struct {
	ActorID   string `json:"actorId"`
	SessionID string `json:"sessionId"`
}
type Options struct {
	Root              string
	Binary            string
	Repository        string
	ConfigDir         string
	PolicyForScope    func(Scope) (Policy, error)
	ConfigDirForScope func(Scope) (string, error)
	// RepositoryForScope is trusted host wiring, never a browser-supplied path.
	RepositoryForScope func(Scope) (string, error)
	RunTimeout         time.Duration
	MaxActive          int
	MaxActivePerOwner  int
	// Nil enables both built-in capabilities; an empty slice disables all.
	CommandCapabilities []CommandCapability
}
type Request struct {
	Kind           string  `json:"kind"`
	IdempotencyKey string  `json:"idempotencyKey"`
	Gaggle         string  `json:"gaggle,omitempty"`
	Workflow       string  `json:"workflow,omitempty"`
	DisplayName    *string `json:"displayName,omitempty"`
	Enabled        *bool   `json:"enabled,omitempty"`
	WorkflowYAML   string  `json:"workflowYaml,omitempty"`
	RunID          string  `json:"runId,omitempty"`
	Message        string  `json:"message,omitempty"`
}
type Action struct {
	ID                   string     `json:"id"`
	OperationID          string     `json:"operationId"`
	ActorID              string     `json:"actorId"`
	SessionID            string     `json:"sessionId"`
	Kind                 string     `json:"kind"`
	Waiting              string     `json:"waiting,omitempty"`
	Status               string     `json:"status"`
	OperationStatus      string     `json:"operationStatus"`
	Summary              string     `json:"summary"`
	Preview              string     `json:"preview"`
	ExpectedConfigDigest string     `json:"expectedConfigDigest,omitempty"`
	ConfigDigest         string     `json:"configDigest"`
	BindingDigest        string     `json:"bindingDigest,omitempty"`
	PayloadDigest        string     `json:"payloadDigest"`
	Request              Request    `json:"request"`
	AuthorizedBy         string     `json:"authorizedBy,omitempty"`
	AuthorizedAt         *time.Time `json:"authorizedAt,omitempty"`
	CreatedAt            time.Time  `json:"createdAt"`
	UpdatedAt            time.Time  `json:"updatedAt"`
	RunID                string     `json:"runId,omitempty"`
	Output               string     `json:"output,omitempty"`
	Error                string     `json:"error,omitempty"`
	CleanupConfirmed     bool       `json:"cleanupConfirmed,omitempty"`
	StopRequested        bool       `json:"stopRequested,omitempty"`
}
type Run struct {
	ID          string    `json:"id"`
	Workflow    string    `json:"workflow"`
	Gaggle      string    `json:"gaggle"`
	Phase       string    `json:"phase"`
	Reason      string    `json:"reason,omitempty"`
	JournalPath string    `json:"journalPath"`
	StartedAt   time.Time `json:"startedAt"`
}
type BacklogItem struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Source string `json:"source"`
}
type State struct {
	ReadOnly            bool                `json:"readOnly,omitempty"`
	Policy              Policy              `json:"policy"`
	Goobers             []apiv1.Goober      `json:"goobers"`
	Mode                string              `json:"mode"`
	ActorID             string              `json:"actorId"`
	SessionID           string              `json:"sessionId"`
	InstanceRoot        string              `json:"instanceRoot"`
	Repository          string              `json:"repository"`
	ConfigDigest        string              `json:"configDigest"`
	Gaggles             []apiv1.Gaggle      `json:"gaggles"`
	Workflows           []apiv1.Workflow    `json:"workflows"`
	Runs                []Run               `json:"runs"`
	Actions             []Action            `json:"actions"`
	Backlog             []BacklogItem       `json:"backlog"`
	CommandCapabilities []CommandCapability `json:"commandCapabilities"`
	WorkflowDraft       string              `json:"workflowDraft"`
	Capabilities        []string            `json:"capabilities"`
	Limitations         []string            `json:"limitations"`
}

// ScopeResolver must derive the actor from authentication and verify session
// membership. A caller must never accept actorId from JSON or query strings.
type ScopeResolver func(context.Context, string) (Scope, error)
