# Copilot SDK interactive driver

`internal/chat/copilotsdk` uses Go SDK v1.0.16 with an owned user-installed
Copilot process. `CopilotAdapter.OpenInteractive` retains the adapter's scoped
environment, credentials, tool allowlist, MCP preparation, optional sandbox and
process-tree joining. It launches `--headless --port 0`, generates a connection
token and connects with URIConnection. Interactive launches omit
`--no-auto-update` to avoid selecting a shim's bundled runtime (Clubhouse #2154).

A session supports multiple prompts, native immediate steering, native enqueue,
Abort, live model changes, channel-backed permission and question responses,
and plan approval. The existing copilotSessionPermissions gate runs before any
human request and again after a delayed approval. Changing the live level never
widens the host ceiling. Unknown SDK events become protocol_unknown with only
their type name; tool arguments and RPC params are not transcript diagnostics.

Resume first checks `$COPILOT_HOME/session-state/<id>` (or the HOME actually
passed to the CLI plus `.copilot`) without permitting path escape. Missing
state yields not_found. Resume has a default 15-second deadline; stalled resume
closes and joins the entire old server before reconnecting and creating a new
session. Other failures are not silently treated as missing sessions.

Only session.idle settles a conversational turn: assistant.turn_end can precede
agent-initiated continuation (Clubhouse #2191). An interrupted turn remains
cancelled even when the protocol later reports normal idle (#2158). Usage comes
from per-call assistant.usage records, not cumulative session totals. Close
stops the server, joins writers, then disconnects SDK session event dispatch.
The live process remains available between sends until the host closes it.

Validation uses fixture-backed tests and an opt-in TestLiveCopilotChat. A live
CLI 1.0.98 run verified two prompts in one session and successful writer join.
Ordinary tests never spend model tokens. The `GOOBERS_CHAT_FIXTURE` option on the
opt-in test captures only allowlisted reply/lifecycle/usage fields, normalizes
message IDs and excludes account, paths, tool arguments and RPC params. The
synthetic schema-drift fixture remains separately labelled.

Current capabilities are conservative: text, tokens, native queue/resume,
permissions/questions/plan approval and live model switching. Image storage,
credit interpretation, richer subagent projection, effort switching, version
range policy and Codex/Claude native transports remain follow-up work. SDK
handshake compatibility is checked before fallback; a guessed version range
is not used to block a working installed CLI.
