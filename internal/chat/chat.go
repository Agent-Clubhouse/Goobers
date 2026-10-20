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

type Delivery string

const (
	Prompt Delivery = "prompt"
	Steer  Delivery = "steer"
	Queued Delivery = "queued"
)

var (
	ErrUnsupported    = errors.New("chat: unsupported operation")
	ErrClosed         = errors.New("chat: session closed")
	ErrBusy           = errors.New("chat: turn active")
	ErrUnknownRequest = errors.New("chat: unknown request")
)

type Level string

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

func NewCeiling(policy, sandbox, credentials Level) Ceiling {
	c := Ceiling{policy}
	for _, l := range []Level{sandbox, credentials} {
		if rank(l) < rank(c.maximum) {
			c.maximum = l
		}
	}
	return c
}
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

type Capabilities struct {
	Steer         bool    `json:"steer"`
	Queue         string  `json:"queue"` // native | client | none
	Interrupt     bool    `json:"interrupt"`
	Resume        string  `json:"resume"` // native | replay | none
	PlanApproval  bool    `json:"planApproval"`
	Questions     bool    `json:"questions"`
	Subagents     string  `json:"subagents"` // full | ids_only | none
	Usage         string  `json:"usage"`     // cost | tokens | context | none
	Limits        bool    `json:"limits"`
	Images        bool    `json:"images"`
	SlashCommands bool    `json:"slashCommands"`
	ModelSwitch   string  `json:"modelSwitch"` // live | per_turn | restart | none
	EffortSwitch  string  `json:"effortSwitch"`
	LiveLevels    []Level `json:"liveLevels"`
	Login         string  `json:"login"`
	SystemPrompt  bool    `json:"systemPrompt"`
}

func MinimalCapabilities() Capabilities {
	return Capabilities{Queue: "none", Resume: "none", Subagents: "none", Usage: "none", ModelSwitch: "none", EffortSwitch: "none", LiveLevels: []Level{}, Login: "terminal"}
}

type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Path     string `json:"path,omitempty"`
	URI      string `json:"uri,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
}

func Text(text string) []ContentBlock { return []ContentBlock{{Type: "text", Text: text}} }
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

type StartOptions struct {
	AgentID, SessionID, TurnID, Workspace, Model, SystemPrompt, ResumeNativeID string
	Fence, LastSeq                                                             uint64
	Level                                                                      Level
	Ceiling                                                                    Ceiling
	ResumeTimeout                                                              time.Duration
}
type SendOptions struct {
	Delivery  Delivery
	CommandID string
}
type PermissionResponse struct {
	OptionID     string
	Message      string
	UpdatedInput map[string]any
}
type QuestionAnswers map[string][]string

type Driver interface {
	ID() string
	Probe(context.Context) (Probe, error)
	Start(context.Context, StartOptions) (Session, error)
}
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
type ResumeFallback string

const (
	ResumeNotFound ResumeFallback = "not_found"
	ResumeStalled  ResumeFallback = "stalled"
)

type ResumeInfo interface{ ResumeFallback() ResumeFallback }

type StartReason string

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

type Probe struct {
	HandshakeOK  bool         `json:"handshakeOk"`
	CLIVersion   string       `json:"cliVersion,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
	Reason       StartReason  `json:"reason,omitempty"`
	ErrorCode    string       `json:"errorCode,omitempty"`
}
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
