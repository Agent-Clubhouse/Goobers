# Claude stream-json

Protocol and normalization are adapted from [Clubhouse](https://github.com/Agent-Clubhouse/Clubhouse),
MIT licensed, commit `3309665c`, `src/main/chat/claude/{stream-transport,normalizer,driver,events}.ts`.
The upstream license is retained in `testdata/LICENSE`.

The nine fixtures in `testdata` are copied unchanged from that commit's
`test/chat-fixtures/claude`. Despite resembling recordings, their headers and
`docs/chat-fixtures.md` explicitly identify them as **hand-made approximations**.
The question answer and subagent shapes include synthesized fields. These tests
exercise protocol compatibility without authenticating or calling a model; they
are not evidence that every shape has been verified against a live CLI.

Only normalized content enters chat events. Tool inputs, account metadata and
unknown raw payloads stay out of diagnostics. Token usage is per turn; USD cost
is the delta of session totals. The first resumed total has no known baseline,
so its cost is omitted. A lower total after a conversation reset starts a new
baseline. Assistant errors are coalesced into one error at the turn result.

`Driver` requires a trusted `Prepare` callback. Production callers obtain it from
`harness.ClaudeAdapter.ChatDriver(req)`, which preserves the batch adapter's
credential guards, isolated config, kill-by-name deny rules, MCP materialization,
and sandbox wrapper. The stream transport removes `CLAUDECODE`,
`CLAUDE_CODE_ENTRYPOINT`, and `CLAUDE_CODE_CHILD_SESSION`. User content travels in
stdin frames; the system prompt travels in initialize's `appendSystemPrompt`
field (also declared in the Claude Agent SDK's control request schema).

A probe initializes and closes a process without sending a prompt. The native
session ID arrives only after the first prompt's `system/init`. The driver supports
text, permission decisions, questions, live model/effort/mode controls, native
resume, `next` steering, and `later` queueing. It does not silently grant persistent
permission suggestions. Images and persistent plan approval are not advertised.

Native results do not prove quiescence: every result emits `custody.pending`.
The process remains the workspace writer between turns. `Close` captures and
stops its process tree before closing stdin, joins the process, then checks
`invoke.WorkspaceQuiescence`. Only a successful proof emits `custody.released`.
An unobservable platform or failed stop wraps `invoke.ErrWorkspaceNotQuiescent`;
Windows currently lacks the underlying process-tree proof. Hosts must not accept
completion artifacts or hand the workspace to another writer before Close.

The Go fake CLI replays all nine fixtures over real stdin/stdout, remaps control
request IDs, reports incomplete/mismatched scripts, and verifies actual outgoing
permission/setting frames. Additional tests exercise conformance, host environment
stripping, ceiling clamping, timeout cleanup, queue/steer priorities, cancellation,
overflow shutdown, and termination of a real descendant writer. No live Claude
calls are part of these tests.
