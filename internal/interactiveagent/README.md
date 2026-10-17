# Invocation-bound native MCP tools

`interactiveagent` connects a native MCP client to a small set of typed host tools. It starts no model or CLI. The host creates one bridge for one admitted execution, binds its callbacks to the verified actor/session/turn/lease, and gives its native adapter a stdio MCP server command such as the application's own `agent-tools --socket <path>` subcommand.

```go
bridge, err := interactiveagent.Start(executionContext, interactiveagent.Options{
    Tools: toolDefinitions,
    Check: func(ctx context.Context) error {
        return sessions.CheckExecution(owner, sessionID, turnID, leaseID, fence)
    },
    Invoke: func(ctx context.Context, call interactiveagent.Call) (any, error) {
        // Decode typed arguments and use the host-bound authority, never a
        // caller-supplied owner/session/lease. Host services own effect receipts.
        return invokeHostTool(ctx, call)
    },
})
// Handle err, then defer bridge.Close().
// The stdio subcommand calls:
err = interactiveagent.ServeStdio(ctx, socketPath, os.Stdin, os.Stdout)
```

`Tool` contains `Name`, `Description`, and `InputSchema map[string]any`. `Call` contains the original `ID json.RawMessage`, `Name`, and `Arguments json.RawMessage`. `Options.Emit func(AuditEvent)` receives only a configured tool name and phase; it never receives arguments, results, RPC IDs, or private errors. `Bridge.ToolNames()` returns a copy of configured names.

The bridge uses raw newline-delimited JSON-RPC over a Unix socket. There is no HTTP endpoint or TCP listener. Its short `/tmp/gbr-agent-*/rpc.sock` path fits Darwin's socket limit; the containing directory is `0700` and the socket is `0600`. It is a local transport locator, not a Microsoft token or a replacement for authority checks. Same-OS-user processes are within this local prototype's trust boundary. It is not an isolation boundary against hostile processes with that user's filesystem access.

Supported methods are MCP `initialize`, `tools/list`, `tools/call`, `ping`, `notifications/initialized`, and `notifications/cancelled`, plus a harmless `version` diagnostic. Version negotiation follows the existing `internal/mcpio` subset: supported `2025-11-25`, `2025-06-18`, and `2024-11-05`; an unknown nonempty requested version receives the newest supported version. Tools are unavailable until initialization. Notifications never launch untracked tool effects.

Requests are limited to 64 KiB and responses to 1 MiB. Strict decoding rejects unknown protocol/call fields, duplicate JSON keys, nesting beyond 32 levels, and invalid IDs. Tool input is validated against its closed object JSON schema; external schema loading is disabled. Argument fields for owner, session, actor, principal, authority, lease, fence, tokens, credentials, approvals, and decision receipts are forbidden. Approval, grant, credential, and reauthentication tools cannot be registered. The host still performs domain validation and controls all decisions and external effects.

Every tool call passes `Check` immediately before `Invoke`. Both callbacks are serialized across connections, with a bounded pending queue. `Check` must fail closed for stale or unavailable execution state. The host's broker or control service still performs its own final effect admission: this bridge does not make a transaction with an external provider. Cancellation closes the transport and cancels queued/in-flight callback contexts; callbacks must honor their context to stop work. There is no token acquisition, provider token vending, or application-identity fallback.

Callbacks must return safe public result objects. Private errors and panics become fixed public failures, without raw diagnostic text. Recognizable credential-shaped input/output is rejected as a defense in depth; this pattern guard cannot identify every possible secret. Broker-owned provider credentials must never enter the bridge's DTOs in the first place.

The transport preserves request IDs and argument bytes. It deliberately neither retries nor deduplicates completed calls: host tools own stable intent IDs, authorization, idempotency, and reconciliation. A repeated active request ID is rejected. A canceled or disconnected MCP client does not prove that an admitted effect did not happen.

`ServeStdio` forwards complete bounded lines and drains accepted replies after normal input EOF. On cancellation or early socket failure it closes an input implementing `io.Closer`. Custom readers must be finite or cancellation-aware; the CLI uses its standard input pipe. The host must close the bridge when its execution ends, and also keep current lease checks on every dispatched operation.

The package tests use real private Unix sockets, real stdio proxying, and controlled host callbacks. They exercise permissions, version negotiation, exact arguments, host-owned replay, unknown tools/fields, identity injection, stale leases, serialization, cancellation, oversized messages, sanitized failures, and schema immutability. They do not invoke Codex, a model, Azure, or ADO. Restricted test environments may require permission to open a local Unix listener. The bridge is enabled on Linux, Darwin, and the listed BSD targets; unsupported platforms return `ErrUnavailable`.
