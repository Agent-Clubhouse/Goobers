# Harness-neutral interactive sessions

The shared contract is `internal/chat`. Providers implement Driver and Session;
the host uses capabilities instead of provider checks. Goobers keeps policy,
credential selection, durable command receipts and workspace custody outside
the protocol transport. Changing a chat permission level cannot widen the
immutable host ceiling. A transport permission grant still passes host policy.

Copilot SDK is the first native driver. Execshim provides per-turn compatibility
for existing adapters: it cannot steer, and continuation means joining the old
writer before starting a new prompt over preserved files. Receipts report actual
transport delivery. Native Codex app-server and Claude stream-json are follow-ups;
their resume, question, model and permission hooks fit the same interface.

A native idle event ends a turn, not the session or writer custody. Copilot's
assistant turn-end can occur before agent-initiated follow-up work; session idle
is the completion boundary. After interruption, cancellation remains the turn
reason even if the native protocol subsequently reports end_turn. Snapshot
capture still requires Close and WorkspaceQuiescence verification.

Events use a v2 envelope and Clubhouse vocabulary. Unknown protocol input is
observable as protocol_unknown without its raw payload. The portal projection
belongs to internal/interactiveagent; durable ordering and stop fencing remain
owned by internal/interactive. Images are unavailable until a host media store
can replace binary content with validated references. Rich media must not be
written inline into transcripts.

Design sources read at Clubhouse commit `3309665c` (MIT): shared chat runtime,
events, capabilities, media and README; agent-runtime chat runtime, spawn,
health and version policy; structured-chat research, Copilot SDK design and
fixture documentation. No TypeScript runtime logic was copied. In particular,
Goobers must not copy a UI permission ladder as an authority mechanism.
