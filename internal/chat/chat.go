// Package chat defines the harness-neutral interactive driver contract.
// Its event vocabulary and capability design follow Clubhouse (MIT), commit
// 3309665c, src/shared/chat. See README.md for attribution and boundaries.
package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Delivery identifies how content actually entered a session.
type Delivery string

// Supported delivery kinds report transport behavior, not user intent.
const (
	Prompt Delivery = "prompt"
	Steer  Delivery = "steer"
	Queued Delivery = "queued"
)

// Stable operation errors let hosts distinguish unsupported and stale requests.
var (
	ErrUnsupported    = errors.New("chat: unsupported operation")
	ErrClosed         = errors.New("chat: session closed")
	ErrBusy           = errors.New("chat: turn active")
	ErrUnknownRequest = errors.New("chat: unknown request")
)

// Level is a requested permission mode, subordinate to host authority.
type Level string

// Permission levels increase from read-only planning to full native access.
const (
	Plan       Level = "plan"
	Ask        Level = "ask"
	AutoEdit   Level = "auto_edit"
	Auto       Level = "auto"
	FullAccess Level = "full_access"
)

func rank(l Level) int {
	switch l {
	case Plan:
		return 0
	case Ask:
		return 1
	case AutoEdit:
		return 2
	case Auto:
		return 3
	case FullAccess:
		return 4
	}
	return 0
}

// Ceiling is a value with no exported mutators. The host intersects policy,
// sandbox and credential authority before starting a session. Zero means plan.
type Ceiling struct{ maximum Level }

// NewCeiling intersects policy, sandbox and credential authority.
func NewCeiling(policy, sandbox, credentials Level) Ceiling {
	c := Ceiling{policy}
	for _, l := range []Level{sandbox, credentials} {
		if rank(l) < rank(c.maximum) {
			c.maximum = l
		}
	}
	return c
}

// Clamp limits a requested level to the immutable host maximum.
func (c Ceiling) Clamp(l Level) Level {
	if rank(l) > rank(c.maximum) {
		l = c.maximum
	}
	switch l {
	case Plan, Ask, AutoEdit, Auto, FullAccess:
		return l
	}
	return Plan
}

// Capabilities advertises the operations implemented by a transport.
type Capabilities struct {
	PermissionExtras PermissionExtras `json:"permissionExtras"`
	Steer            bool             `json:"steer"`
	Queue            string           `json:"queue"` // native | client | none
	Interrupt        bool             `json:"interrupt"`
	Resume           string           `json:"resume"` // native | replay | none
	PlanApproval     bool             `json:"planApproval"`
	Questions        bool             `json:"questions"`
	Subagents        string           `json:"subagents"` // full | ids_only | none
	Usage            string           `json:"usage"`     // cost | tokens | context | none
	Limits           bool             `json:"limits"`
	Images           bool             `json:"images"`
	SlashCommands    bool             `json:"slashCommands"`
	ModelSwitch      string           `json:"modelSwitch"` // live | per_turn | restart | none
	EffortSwitch     string           `json:"effortSwitch"`
	LiveLevels       []Level          `json:"liveLevels"`
	Login            string           `json:"login"`
	SystemPrompt     bool             `json:"systemPrompt"`
}

// MinimalCapabilities returns a conservative capability record with no optional operations.
func MinimalCapabilities() Capabilities {
	return Capabilities{Queue: "none", Resume: "none", Subagents: "none", Usage: "none", ModelSwitch: "none", EffortSwitch: "none", LiveLevels: []Level{}, Login: "terminal"}
}

// ContentBlock is normalized text or a reference to host-managed content.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Path     string `json:"path,omitempty"`
	URI      string `json:"uri,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
}

// Text wraps a plain string as normalized content.
func Text(text string) []ContentBlock { return []ContentBlock{{Type: "text", Text: text}} }

// PlainText extracts supported text blocks and rejects other content.
func PlainText(content []ContentBlock) (string, error) {
	var b strings.Builder
	for _, c := range content {
		if c.Type != "text" {
			return "", ErrUnsupported
		}
		b.WriteString(c.Text)
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("chat: empty content")
	}
	return b.String(), nil
}

// StartOptions contains host-owned identity, authority and session configuration.
type StartOptions struct {
	AgentID, SessionID, TurnID, Workspace, Model, SystemPrompt, ResumeNativeID string
	Fence, LastSeq                                                             uint64
	Level                                                                      Level
	Ceiling                                                                    Ceiling
	ResumeTimeout                                                              time.Duration
}

// SendOptions carries requested delivery and command correlation.
type SendOptions struct {
	Delivery  Delivery
	CommandID string
}

// PermissionResponse answers one pending permission request.
type PermissionResponse struct {
	OptionID     string
	Message      string
	UpdatedInput map[string]any
}

// QuestionAnswers maps question identities to selected or free-form answers.
// A nil map cancels the request without selecting or inventing an answer.
type QuestionAnswers map[string][]string

// Driver probes a transport and opens sessions within host authority.
type Driver interface {
	ID() string
	Probe(context.Context) (Probe, error)
	Start(context.Context, StartOptions) (Session, error)
}

// Session is a live conversation whose Close operation joins workspace writers.
type Session interface {
	NativeID() string
	Capabilities() Capabilities
	Events() <-chan Event
	Send(context.Context, []ContentBlock, SendOptions) (Delivery, error)
	Interrupt(context.Context) error
	RespondPermission(context.Context, string, PermissionResponse) error
	AnswerQuestion(context.Context, string, QuestionAnswers) error
	SetLevel(context.Context, Level) (Level, error)
	SetModel(context.Context, string) error
	// Close joins all owned workspace writers. Failure must wrap
	// invoke.ErrWorkspaceNotQuiescent; cancellation alone is never evidence.
	// Close is idempotent and does not depend on draining Events.
	Close() error
}

// ResumeFallback identifies why a fresh session replaced a requested resume.
type ResumeFallback string

// Resume fallback reasons distinguish missing state from a stalled protocol.
const (
	ResumeNotFound ResumeFallback = "not_found"
	ResumeStalled  ResumeFallback = "stalled"
)

// ResumeInfo optionally exposes a session resume fallback.
type ResumeInfo interface{ ResumeFallback() ResumeFallback }

// StartReason is a bounded startup outcome suitable for diagnostics.
type StartReason string

// Startup reasons form a closed diagnostic vocabulary.
const (
	NoDriver                StartReason = "no_driver"
	BinaryMissing           StartReason = "binary_missing"
	Paused                  StartReason = "paused"
	HandshakeFailed         StartReason = "handshake_failed"
	LoginRequired           StartReason = "login_required"
	VersionBelowMin         StartReason = "version_below_min"
	VersionKnownBad         StartReason = "version_known_bad"
	StartFailed             StartReason = "start_failed"
	FailedAfterSessionStart StartReason = "failed_after_session_start"
	Killed                  StartReason = "killed"
	Superseded              StartReason = "superseded"
	StartErrorReason        StartReason = "error"
)

// Probe describes a zero-model-request availability check.
type Probe struct {
	HandshakeOK  bool         `json:"handshakeOk"`
	CLIVersion   string       `json:"cliVersion,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
	Reason       StartReason  `json:"reason,omitempty"`
	ErrorCode    string       `json:"errorCode,omitempty"`
}

// Attempt records one driver selection attempt without RPC parameters.
type Attempt struct {
	Driver         string      `json:"driver"`
	Reason         StartReason `json:"reason,omitempty"`
	ErrorCode      string      `json:"errorCode,omitempty"`
	CheckMS        int64       `json:"checkMs"`
	SessionStartMS int64       `json:"sessionStartMs,omitempty"`
}

// EffortSetter is optional; Session stays stable for drivers without an effort
// control. Empty effort requests the provider default when supported.
type EffortSetter interface {
	SetEffort(context.Context, string) error
}

// PermissionExtras advertises optional permission response semantics.
type PermissionExtras struct {
	DenyMessage bool `json:"denyMessage"`
}

// Question is a provider-neutral structured user question.
type Question struct {
	QuestionID    string           `json:"questionId"`
	Prompt        string           `json:"prompt"`
	Options       []QuestionOption `json:"options"`
	AllowMultiple bool             `json:"allowMultiple"`
	AllowFreeText bool             `json:"allowFreeText"`
}

// QuestionOption is one selectable stable answer identity and label.
type QuestionOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ModelInfo contains only advertised model and effort choices.
type ModelInfo struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"defaultEffort,omitempty"`
}
