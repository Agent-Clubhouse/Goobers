// Package interactive implements the opt-in InteractiveExecution prototype.
// It does not change declarative workflow scheduling or completion semantics.
package interactive

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Execution modes identify the explicitly selected host adapter.
const (
	ExecutionModeCodexLocal = "codex-local"
	ExecutionModeLocalTools = "local-tools"
)

// Surface describes an enabled experience and its resolved execution policy.
type Surface struct {
	// ConfigRevision covers the resolved non-secret DSL snapshot, including
	// authority reference mappings which are not carried as prompt material.
	ConfigRevision           string   `json:"configRevision,omitempty"`
	Gaggle                   string   `json:"gaggle,omitempty"`
	Goober                   string   `json:"goober,omitempty"`
	Model                    string   `json:"model,omitempty"`
	Harness                  string   `json:"harness,omitempty"`
	Tools                    []string `json:"tools,omitempty"`
	Workflows                []string `json:"workflows,omitempty"`
	AuthorityRefs            []string `json:"authorityRefs,omitempty"`
	ChildCommandCapabilities []string `json:"childCommandCapabilities,omitempty"`
	ExecutionModes           []string `json:"executionModes,omitempty"`
	DefaultExecutionMode     string   `json:"defaultExecutionMode,omitempty"`
	MaxTurnSeconds           int      `json:"maxTurnSeconds,omitempty"`
	LeaseSeconds             int      `json:"leaseSeconds,omitempty"`
	MaxActive                int      `json:"maxActive,omitempty"`
	MaxActivePerOwner        int      `json:"maxActivePerOwner,omitempty"`
	ID                       string   `json:"id"`
	Name                     string   `json:"name"`
	Description              string   `json:"description"`
	Mode                     string   `json:"mode"`
	Enabled                  bool     `json:"enabled"`
	MaxChildren              int      `json:"maxChildren"`
	Instructions             string   `json:"instructions,omitempty"`
}

// Options configures the session coordinator and its host boundaries.
type Options struct {
	// AvailableExecutionModes is the host adapter allowlist. Nil preserves the
	// package's local-tools default; it never enables a model-backed adapter.
	AvailableExecutionModes []string
	DefaultExecutionMode    string
	StateDir                string
	Surfaces                []Surface
	Executor                Executor
	Principal               func(*http.Request) (string, error)
	MaxTurnDuration         time.Duration
	WorkerID                string
	LeaseDuration           time.Duration
	MaxActive               int
	MaxActivePerOwner       int
	MaxSessionOutputBytes   int
	Now                     func() time.Time
	// OnStop must commit any external authority fence before returning. It is
	// invoked after the durable session fence, outside the coordinator mutex.
	// Failure leaves stop pending until a subsequent stop retries propagation.
	OnStop func(context.Context, string, string) error
}

// Executor runs one fenced interactive execution and joins its writers.
type Executor interface {
	// Execute returns only once all workspace writers it launched have stopped.
	// It must honor context cancellation and redact output before calling emit.
	Execute(context.Context, ExecutionRequest, func(ExecutionEvent) error) (ExecutionResult, error)
}

// RecoveryExecutor verifies writer custody before recovery may proceed.
type RecoveryExecutor interface {
	// Recover confirms old process custody and reconciles ambiguous effects.
	// It must refuse recovery while a previous writer might still be alive.
	Recover(context.Context, ExecutionRequest) error
}

// ExecutionRequest binds an execution to its session, policy, workspace, and lease.
type ExecutionRequest struct {
	ConfigRevision, Gaggle, Goober, Model, Harness            string
	Tools, Workflows, AuthorityRefs, ChildCommandCapabilities []string
	// Limits are resolved once from the admitted policy, never widened by reload.
	leaseDuration, maxTurnDuration                                 time.Duration
	turnExpiresAt                                                  time.Time
	ExecutionMode                                                  string
	WorkflowYAML                                                   string
	SessionID, TurnID, Owner, SurfaceID, Mode, Text, WorkspacePath string
	Fence                                                          uint64
	Attempt                                                        int
	History                                                        []Turn
	Instructions                                                   string
	ChildID                                                        string
	ReadOnly                                                       bool
	Steering                                                       <-chan Steering
	WorkerID, LeaseID                                              string
	Heartbeat                                                      func() error
}

// Steering carries an exact command and its executor delivery acknowledgement.
type Steering struct {
	CommandID, Text string
	// Ack(nil) means the harness actually accepted this exact steering message.
	Ack func(error)
}

// ExecutionEvent contains executor output admitted through the current lease.
type ExecutionEvent struct {
	Type string
	Data map[string]any
}

// ExecutionResult reports completed output, workspace changes, and custody state.
type ExecutionResult struct {
	// CustodyPending means Execute returned without proving all workspace writers stopped.
	CustodyPending               bool
	Message, WorkspacePath, Diff string
	Changes                      []Change
	WaitingAuth                  bool
	AuthReason                   string
}

// Change summarizes a changed workspace path.
type Change struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// Member associates a principal with a session role.
type Member struct {
	Principal string `json:"principal"`
	Role      string `json:"role"`
}

// Lease records fenced worker ownership and expiry.
type Lease struct {
	WorkerID  string    `json:"workerId"`
	LeaseID   string    `json:"leaseId"`
	Fence     uint64    `json:"fence"`
	ExpiresAt time.Time `json:"expiresAt"`
	Status    string    `json:"status"`
}

// Session contains the durable state and history of one interactive conversation.
type Session struct {
	ConfigRevision   string    `json:"configRevision,omitempty"`
	ExecutionMode    string    `json:"executionMode"`
	StopRequested    bool      `json:"stopRequested,omitempty"`
	OutputBytes      int       `json:"outputBytes"`
	CustodyPending   bool      `json:"custodyPending,omitempty"`
	StopFencePending bool      `json:"stopFencePending,omitempty"`
	ConfigDigest     string    `json:"configDigest"`
	Members          []Member  `json:"members"`
	Lease            *Lease    `json:"lease,omitempty"`
	ID               string    `json:"id"`
	Owner            string    `json:"owner"`
	SurfaceID        string    `json:"surfaceId"`
	Mode             string    `json:"mode"`
	Title            string    `json:"title"`
	Status           string    `json:"status"`
	Sequence         uint64    `json:"sequence"`
	Fence            uint64    `json:"fence"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	ActiveTurnID     string    `json:"activeTurnId,omitempty"`
	Turns            []Turn    `json:"turns"`
	Children         []Child   `json:"children"`
	Changes          []Change  `json:"changes"`
	Diff             string    `json:"diff,omitempty"`
	WorkspacePath    string    `json:"workspacePath,omitempty"`
	RecoveryReason   string    `json:"recoveryReason,omitempty"`
}

// Turn records a user request and its execution attempt.
type Turn struct {
	ID         string     `json:"id"`
	Text       string     `json:"text"`
	Status     string     `json:"status"`
	Attempt    int        `json:"attempt"`
	Fence      uint64     `json:"fence"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Child records a bounded child proposal and review outcome.
type Child struct {
	WorkflowYAML   string     `json:"workflowYaml,omitempty"`
	WorkflowDigest string     `json:"workflowDigest,omitempty"`
	ActorSource    string     `json:"actorSource"`
	SourceTurnID   string     `json:"sourceTurnId,omitempty"`
	Lease          *Lease     `json:"lease,omitempty"`
	ExecutionKind  string     `json:"executionKind"`
	ReadOnly       bool       `json:"readOnly"`
	ID             string     `json:"id"`
	Prompt         string     `json:"prompt"`
	Status         string     `json:"status"`
	Result         string     `json:"result,omitempty"`
	RequestedAt    time.Time  `json:"requestedAt"`
	ReviewedAt     *time.Time `json:"reviewedAt,omitempty"`
}

// Event is one ordered entry in a session’s durable history.
type Event struct {
	ActorSource string         `json:"actorSource,omitempty"`
	Actor       string         `json:"actor"`
	Sequence    uint64         `json:"sequence"`
	SessionID   string         `json:"sessionId"`
	TurnID      string         `json:"turnId,omitempty"`
	CommandID   string         `json:"commandId,omitempty"`
	Type        string         `json:"type"`
	Time        time.Time      `json:"time"`
	Data        map[string]any `json:"data"`
}

// Command identifies an idempotent session mutation and its fence preconditions.
type Command struct {
	WorkflowYAML     string `json:"workflowYaml,omitempty"`
	CommandID        string `json:"commandId"`
	Type             string `json:"type"`
	Text             string `json:"text,omitempty"`
	ExpectedSequence uint64 `json:"expectedSequence"`
	ExpectedFence    uint64 `json:"expectedFence"`
	TurnID           string `json:"turnId,omitempty"`
	ChildID          string `json:"childId,omitempty"`
	Decision         string `json:"decision,omitempty"`
}

// CommandReceipt records durable acceptance and subsequent delivery status.
type CommandReceipt struct {
	CommandID string `json:"commandId"`
	SessionID string `json:"sessionId"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Sequence  uint64 `json:"sequence"`
	TurnID    string `json:"turnId,omitempty"`
	ChildID   string `json:"childId,omitempty"`
}

// CreateRequest identifies an idempotent request for a new session.
type CreateRequest struct {
	ExecutionMode string `json:"executionMode,omitempty"`
	CommandID     string `json:"commandId"`
	SurfaceID     string `json:"surfaceId"`
	Title         string `json:"title,omitempty"`
}

// APIError contains a safe HTTP status, code, and message.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string                    { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
func problem(status int, code, message string) error { return &APIError{status, code, message} }
