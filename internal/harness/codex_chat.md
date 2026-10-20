# Native Codex interactive driver

`NewCodexChatDriver(CodexChatConfig{Adapter, Request, StateDir})` implements the
chat contract with driver ID `codex_app_server`; the host registers it under
provider key `codex`. The adapter and RunRequest supply the same credential,
MCP, shell environment and outer sandbox authority as batch Codex invocations.
The driver owns process setup and verified shutdown. Unsupported rich media,
native queues, tool allowlists and unknown models/efforts fail explicitly.

`StateDir` is an absolute, host-owned directory unique to one chat session,
outside its workspace (including symlink resolution). It is locked while active,
retained for native resume, and cleaned by the host on session disposal.
Probes create disposable state beside the host-owned session directory, rather
than inheriting a batch adapter temp root inside the workspace. Without a
StateDir, the configured ephemeral root must itself be outside the workspace;
state is temporary and native resume is not advertised. Failed quiescence retains state and reports
`invoke.ErrWorkspaceNotQuiescent`; the host must retain custody. Close does not
require an event subscriber. Event backpressure stops the writer.

The private `CODEX_HOME` contains configuration and native sessions, never a
copied `auth.json`. API keys remain environment-based. Ambient ChatGPT mode
requires authentication available to Codex through the OS keyring in that
private home. File-only logins fail with a diagnostic: copying renewable tokens
can invalidate the original login. Keyring lookup is scoped by Codex; a login
available only under another home is not assumed reusable. State must not be
shared with an ordinary user Codex home.

App-server has no `--ignore-user-config` or `--ignore-rules`. Private state,
project document size zero, empty project markers/fallback names, disabled
hooks/web search, and an explicit shell environment enforce the corresponding
boundary. Required MCP servers are supplied through both private configuration
and `config.mcp_servers`; credentials are referenced by `env_vars` or
`env_http_headers`, never inserted into protocol configuration values.

Version 1 supports Plan (read only) and Auto (workspace write), intersected with
the host's immutable ceiling. Full Access clamps to Auto; Ask/Auto Edit are not
advertised, start conservatively as Plan, and cannot be selected live. Level,
model and effort changes apply to the next turn. Every turn explicitly sends
sandbox, approval, model and effort overrides to avoid sticky native settings.
Empty effort selects the current catalog's concrete default, since native
`effort: null` does not reset an earlier override.

All execution stays inside Codex's native sandbox and the configured Goobers
outer sandbox. Network access is disabled and approval policy is `never`.
Unexpected command/file approvals, permission expansion, network grants and
exec-policy amendments are automatically declined with a visible notice.
There is no human accept path and no persisted exec-policy amendment. Broader
human approvals are a follow-up requiring proof that each accepted request
remains within the existing ceiling. Plain user questions are supported;
secret questions and unsupported server requests are rejected. Unknown requests
always receive `-32601`; diagnostics never include raw request params.

The process-backed fixture suite exercises framing, handshake order, turn and
steer races, resume, auth isolation and shutdown. The opt-in installed-CLI smoke
starts a native thread and reads its model catalog without sending a turn or
using a real credential:

```
GOOBERS_CODEX_CHAT_SMOKE=1 go test -race -timeout 30m ./internal/harness -run '^TestCodexChatInstalledSmoke$' -v
```

Protocol bindings and their reproducible drift check are documented in
[`internal/codexprotocol/README.md`](../codexprotocol/README.md). The driver ports
lifecycle and event-normalization ideas from MIT-licensed Clubhouse commit
`3309665c`; its license is retained there. Process custody and isolation follow
Goobers' existing adapter boundaries. Windows quiescence is currently
unsupported by the shared process layer and therefore fails closed.
