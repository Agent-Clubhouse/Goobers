// Package interactive implements the opt-in InteractiveExecution prototype.
// It does not change declarative workflow scheduling or completion semantics.
package interactive

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type Surface struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Mode         string `json:"mode"`
	Enabled      bool   `json:"enabled"`
	MaxChildren  int    `json:"maxChildren"`
	Instructions string `json:"instructions,omitempty"`
}

type Options struct {
	StateDir              string
	Surfaces              []Surface
	Executor              Executor
	Principal             func(*http.Request) (string, error)
	MaxTurnDuration       time.Duration
	WorkerID              string
	LeaseDuration         time.Duration
	MaxActive             int
	MaxActivePerOwner     int
	MaxSessionOutputBytes int
	Now                   func() time.Time
	// OnStop must commit any external authority fence before returning. It is
	// invoked after the durable session fence, outside the coordinator mutex.
	// Failure leaves stop pending until a subsequent stop retries propagation.
	OnStop func(context.Context, string, string) error
}

type Executor interface {
	// Execute returns only once all workspace writers it launched have stopped.
	// It must honor context cancellation and redact output before calling emit.
	Execute(context.Context, ExecutionRequest, func(ExecutionEvent) error) (ExecutionResult, error)
}

type RecoveryExecutor interface {
	// Recover confirms old process custody and reconciles ambiguous effects.
	// It must refuse recovery while a previous writer might still be alive.
	Recover(context.Context, ExecutionRequest) error
}

type ExecutionRequest struct {
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

type Steering struct {
	CommandID, Text string
	// Ack(nil) means the harness actually accepted this exact steering message.
	Ack func(error)
}

type ExecutionEvent struct {
	Type string
	Data map[string]any
}

type ExecutionResult struct {
	Message, WorkspacePath, Diff string
	Changes                      []Change
	WaitingAuth                  bool
	AuthReason                   string
}

type Change struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type Member struct {
	Principal string `json:"principal"`
	Role      string `json:"role"`
}
type Lease struct {
	WorkerID  string    `json:"workerId"`
	LeaseID   string    `json:"leaseId"`
	Fence     uint64    `json:"fence"`
	ExpiresAt time.Time `json:"expiresAt"`
	Status    string    `json:"status"`
}
type Session struct {
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

type Child struct {
	Lease         *Lease     `json:"lease,omitempty"`
	ExecutionKind string     `json:"executionKind"`
	ReadOnly      bool       `json:"readOnly"`
	ID            string     `json:"id"`
	Prompt        string     `json:"prompt"`
	Status        string     `json:"status"`
	Result        string     `json:"result,omitempty"`
	RequestedAt   time.Time  `json:"requestedAt"`
	ReviewedAt    *time.Time `json:"reviewedAt,omitempty"`
}

type Event struct {
	Actor     string         `json:"actor"`
	Sequence  uint64         `json:"sequence"`
	SessionID string         `json:"sessionId"`
	TurnID    string         `json:"turnId,omitempty"`
	CommandID string         `json:"commandId,omitempty"`
	Type      string         `json:"type"`
	Time      time.Time      `json:"time"`
	Data      map[string]any `json:"data"`
}

type Command struct {
	CommandID        string `json:"commandId"`
	Type             string `json:"type"`
	Text             string `json:"text,omitempty"`
	ExpectedSequence uint64 `json:"expectedSequence"`
	ExpectedFence    uint64 `json:"expectedFence"`
	TurnID           string `json:"turnId,omitempty"`
	ChildID          string `json:"childId,omitempty"`
	Decision         string `json:"decision,omitempty"`
}

type CommandReceipt struct {
	CommandID string `json:"commandId"`
	SessionID string `json:"sessionId"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Sequence  uint64 `json:"sequence"`
	TurnID    string `json:"turnId,omitempty"`
	ChildID   string `json:"childId,omitempty"`
}

type CreateRequest struct {
	CommandID string `json:"commandId"`
	SurfaceID string `json:"surfaceId"`
	Title     string `json:"title,omitempty"`
}

type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string                    { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
func problem(status int, code, message string) error { return &APIError{status, code, message} }
