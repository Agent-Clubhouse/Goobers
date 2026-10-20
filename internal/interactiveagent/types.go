// Package interactiveagent exposes an invocation-bound set of host tools to a
// native MCP client over a private local Unix socket. It owns no provider token,
// session identity, approval capability, or retry/effect policy.
package interactiveagent

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Bridge limits bound request sizes and tool execution output.
const (
	MaxRequestBytes  = 64 << 10
	MaxResponseBytes = 1 << 20
)

// Tool describes a mediated tool and its input schema.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Call preserves the client's JSON-RPC ID and arguments. The ID is not authority
// or an automatic idempotency key; the host must own stable effect/replay policy.
type Call struct {
	ID        json.RawMessage
	Name      string
	Arguments json.RawMessage
}

// AuditEvent deliberately omits arguments, IDs, output, and internal errors.
type AuditEvent struct {
	Tool  string
	Phase string
}

// Options binds the bridge to its invocation, policy, and tool handler.
type Options struct {
	Tools []Tool
	// Check must validate the exact invocation's current server lease/fence.
	// Check and Invoke are serialized across every connection to this bridge.
	Check  func(context.Context) error
	Invoke func(context.Context, Call) (any, error)
	Emit   func(AuditEvent)
	// ShutdownTimeout bounds waiting for admitted host callbacks to finish.
	// A timeout fails custody closed; it never means the callback was stopped.
	ShutdownTimeout time.Duration
}

// Bridge errors describe rejected transport and tool requests.
var (
	ErrUnavailable          = errors.New("interactive tool bridge unavailable")
	ErrInvalidConfiguration = errors.New("invalid interactive tool bridge configuration")
	ErrInvalidMessage       = errors.New("invalid or oversized MCP message")
	ErrNotQuiescent         = errors.New("interactive host tool callbacks have not stopped")
)
