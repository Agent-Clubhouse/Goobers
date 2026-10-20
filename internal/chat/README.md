# Interactive chat drivers

`Driver` probes and starts a native session. `Session` can survive many turns;
only `Close` ends its writer custody. The host assigns its immutable permission
ceiling from policy, sandbox and credentials. A level change never grants new
host authority. Unsupported features return `ErrUnsupported` and capabilities
advertise only implemented behavior.

The v2 envelope uses Clubhouse's vocabulary, plus Goobers command, custody and
completion events. `seq` increases within a session stream; `fence` is the host
lease fence. `commandId` correlates acceptance and actual transport delivery.
The durable store remains the authority for global ordering and stop fencing.
RPC params are never diagnostic payloads. Unknown protocol shapes become
`protocol_unknown` with only driver, kind and name. Rich media is intentionally
unsupported until host-owned media storage can replace bytes with references.

A session must join process trees and verify `internal/invoke.WorkspaceQuiescence`
before Close succeeds. Failure retains custody and prevents another writer.
Receiving `session.idle` or a completion file alone is not quiescence evidence.
Events are bounded; a driver must stop on stream overflow rather than silently
lose lifecycle events. Consumers should drain events while the session runs.

Design attribution: [Clubhouse](https://github.com/Agent-Clubhouse/Clubhouse),
MIT licensed, commit `3309665c`, `src/shared/chat/{runtime,events,capabilities}.ts`,
`src/main/services/agent-runtime/{chat-runtime,chat-spawn}.ts` and
`docs/chat-fixtures.md`. This is a Go implementation of those interface and
fixture-format ideas, not a copy of the TypeScript implementation. Native
Codex app-server and Claude stream-json drivers can implement the same seam;
the exec shim preserves their existing per-turn adapters meanwhile.

Session-preserving interrupt requires `ActiveTurnIdentifier`: the host captures
the outstanding native turn before requesting interruption, then waits for a
settled `turn_ended` carrying that `data.nativeTurnId` and verifies that no native
work remains active. A buffered earlier idle is not sufficient. Drivers without
that identity use custody-proven close/restart; attached management bridges
always use close/restart until a reviewed per-turn callback drain exists.
