# Shared interactive sessions

The session ledger and HTTP/portal adapters are implemented. Agent execution
availability remains disabled until the daemon's session runtime is installed.
Creating schema tables or wiring a read route does not enable model execution.

## Human surface

The gaggle page lists shared sessions and provides a conversation view. An
explicitly configured interactive policy controls creation and messaging. Current
view access controls every page read; instance operator status alone does not
grant gaggle access. Removing interactive configuration preserves existing
read-only monitoring for authorized users.

A session selects an existing configured Goober. Creation pins its configuration
and content; callers cannot submit execution pins, credentials or human identity.
Messages display the separate verified identity issuer and subject. Agent replies
have their own authorship. A run link appears only after its journal identity is
verified. An accepted or queued turn is not proof the model has begun working.

Messages are processed one turn at a time in durable order. New messages do not
interrupt the active turn, and closing the browser does not cancel accepted work.
The first runtime reconstructs bounded conversational context; it does not claim
native harness session continuity. Explicit interruption and linked source-write
operations remain later typed capabilities. Source repository changes must pass
policy-governed PR publication before those operations can be enabled.

The portal uses explicit refresh and bounded pages. A failed authorization refresh
removes previously displayed conversation data. A request with an unknown outcome
retains its key and exact content for retry while the conversation remains open;
other submissions and session switching are disabled until it resolves. Drafts
and retry keys are not persisted in browser storage. After reloading or reopening,
read the durable session history before submitting another command.

## API

Every route requires an authenticated human and current gaggle access. Writes
also require the instance operator role, current gaggle action permission, a
same-origin JSON request and an `Idempotency-Key` header. Bodies contain only the
listed fields; unknown authority fields are rejected. Responses use `no-store`.

| Route | Content / result |
| --- | --- |
| `GET /api/v1/gaggles/{gaggle}/sessions` | `cursor`, `limit`; bounded session summaries |
| `POST /api/v1/gaggles/{gaggle}/sessions` | `title`, configured `goober`; durable acceptance |
| `GET /api/v1/gaggles/{gaggle}/sessions/{session}` | Current session state |
| `GET /api/v1/gaggles/{gaggle}/sessions/{session}/messages` | Numeric `after`, `limit`; ordered messages |
| `POST /api/v1/gaggles/{gaggle}/sessions/{session}/messages` | `text`; durable message and turn acceptance |
| `POST /api/v1/gaggles/{gaggle}/sessions/{session}/close` | Optional `reason`; intake closure and cancellation request |

Mutation responses are HTTP 202, including exact replay. They acknowledge custody
rather than execution or cancellation completion. `cancel-requested` remains
separate from `closed`; close fences new input and requires active writer custody
to settle. `lastOutcome` is independent of idle/running/closed session state.

The common ledger bounds each text message to 64 KiB, title to 256 bytes, page to
200 records and message page to 1 MiB. Initial admission limits are 32 open
sessions per gaggle, four executing turns per gaggle, and 64 queued turns per
session. Response capacity is reserved before accepting a turn. Closed details
retain for 30 days, followed by 30 days of command tombstones. Live or uncertain
execution is not evicted to make space.

## Qualification

Transport tests cover verified actor binding, forged authority rejection,
malformed/oversized requests, bounded pagination and permission refusal. Portal
tests cover attribution, queued-versus-executed links, retry after a lost response,
refresh, close-pending status and permission revocation. Real model, worker and
browser qualification remains outstanding. The initial portal does not stream
agent output or expose backlog/PR mutations from a conversation.


## Execution identity and queued context

Accepted turns now carry a genuine session lineage in their own run journals.
The dedicated session driver executes one scratch agent task with frozen bounded
context; it does not inherit ordinary automation claims or provider hooks. A
session run cannot use ordinary resume, rerun or stage-restart paths to acquire
a different authority. A trusted execution callback is required before any
journal can be published.

Context is assembled only after the prior turn settles, then frozen before
dispatch. Thus a queued second human message can include the first agent reply,
but cannot include a later queued human message. Each accepted turn reserves up
to 256 KiB for its input manifest in the shared bounded ledger, alongside its
response reservation. Existing and new databases apply this as an additive
migration after their source-start tables. Journal/schema, actual Runner.Start,
parallel message ordering, byte bounds and tamper tests cover these contracts.

These primitives do not yet enable the daemon runtime; session availability
remains false until the coordinator, human model driver and daemon lifecycle
have been installed.
