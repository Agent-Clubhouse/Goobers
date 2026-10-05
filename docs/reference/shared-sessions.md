# Shared interactive sessions

The session ledger, HTTP/portal adapters and native daemon runtime are implemented.
The daemon advertises execution only after installing the runtime and its lifecycle
hooks. Creating schema tables or wiring a read route alone does not enable it.

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
native harness session continuity. A queued cancellation can stop one turn while
preserving the conversation and later messages. Typed source reads, field edits, needs-human resolution and selected
PR repairs are installed under their own grants. Repository changes use governed
PR publication; conversational permission alone grants no source mutation.

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
| `POST /api/v1/gaggles/{gaggle}/sessions/{session}/messages` | `text`, optional inspected `repairTarget`; durable message and turn acceptance |
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
browser qualification remains outstanding. The portal does not stream agent
output. It exposes typed backlog operations and explicit selected-PR repair through
the authorized session tools described below.


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

The native driver executes the pinned Claude or Codex profile in a model-only
scratch task, through the current human execution lease. Provider reads and typed
source mutations require separate installed host operations; this driver supplies
no default repository credentials, automation claims or external MCP servers.


## Turn coordinator and execution capacity

The coordinator serializes each session's turns, observes only exact matching run
journals, and publishes a run link after that observation succeeds. A completed
journal does not alone release execution capacity: every writer must have joined.
Closing a session requests cancellation and waits for this evidence. Uncertain
writers retain capacity and FIFO ownership across daemon restarts. Only startup
reconciliation can retry a dispatch whose journal is strongly proven absent.

A reserved scheduler operation bucket admits at most four executing turns per
gaggle while sharing the instance's global/resource limits. It is not a fake
catalog workflow and does not inherit automation polling cadence. Ordinary
watchdogs cannot release unjoined session custody. Startup must restore the entire
unsettled inventory before allowing new admission. Bounded sweeps reconcile old
turns and prune only settled, closed session records.

Composed tests cover browser disconnect, queued turn ordering, current-policy
revocation, close/cancel/join, unknown writers, startup custody restoration, exact
provenance and concurrent per-gaggle/global limits. The native daemon now installs this coordinator, restores its full unsettled
inventory before new admission, and retains configuration and journals until
writer custody settles. Session-specific start/join and runtime-close evidence
are required; an unknown writer cannot release capacity or launch a replacement.


## Native runtime qualification

Composed HTTP acceptance tests execute the actual Runner and Claude adapter with
a fake process boundary. They cover queued response context, cancellation, journal
publication failure and a writer whose exit cannot be proven. Exact host writer
markers distinguish model completion from whole-runtime cleanup. These checks do
not qualify live model, Kubernetes or provider execution. The native
model process remains isolated from provider credentials. Installed typed host
operations independently authorize and audit source effects.


## Typed backlog read tools

The session runtime can install host-owned `get_backlog_item` and
`list_backlog_items` tools for explicitly configured backlog sources. Each turn
gets an ephemeral grant bound to the verified human, session, run and committed
stage attempt. The tool sees permitted binding names, never provider credentials
or alternate targets. External MCP configuration cannot add this authority.

Calls are bounded, audited to the exact session run, and store returned source
material as unapproved data. Closing the turn revokes its grant and joins any
outstanding read before runtime custody settles. A gaggle with no authorized
source reader continues with model-only conversation. Native source mutations
need separately installed durable typed command custody; these read tools grant
no edit, PR or repository publication capability.


## Choosing a PR for an agent message

When current repository-read and PR-repair permissions are available, the session
composer offers a configured repository source and PR number. Inspect the PR,
review its branch and exact head, then select it for the message. The retained
human message shows that selection to other authorized participants. A lost
submission reply retries the same message, key and selected head against the
original connection and target. A different connection cannot replay that pending
command. Refresh clears a PR selection that has not yet been submitted; inspect
and select it again before sending.

Inspection only reads with the gaggle's explicit interactive repository credential.
It does not grant repair permission or reserve the branch. The executing turn
checks the same native PR/repository identities, current repair policy and writer
custody before each change. Fork PRs are unsupported; a changed head requires a
fresh human selection unless it is the verified result recorded in this turn's
repair receipt or separate positive observation.
The picker and local daemon repair adapter are installed. The host holds claim,
admission and repository-manager locks through the bounded effect and receipt,
and uses an immutable snapshot during reload. Shared/Temporal ownership, pinned
workspaces and custom provider endpoints remain refused pending qualified adapters.


## Retained PR repair checks

The conversation view exposes **Check a retained PR repair**, including in closed
sessions, to current operators with repository-read and PR-repair authority and
an explicit credential binding. Enter the repair command ID returned by the agent.
`GET /api/v1/gaggles/{gaggle}/pr-repairs/{command}` loads retained evidence without
a provider read. An explicit `POST` to the same route plus `/check`, with `{}`,
checks only a joined unknown effect against its original repository, commit parent,
command marker and files. Callers cannot override the actor, target or credentials.

A verified result is shown as `observed-applied`; the original provider
acknowledgement and outcome remain unchanged. Exact positive proof releases the
reserved target. Missing, moved or foreign evidence remains unknown; a check never
repeats a branch update. An unjoined `attempting` effect cannot be settled here.
The original human and checking operator are separate attributed fields. History
retains at most 16 checks plus an omitted count; settled retention starts from the
positive observation time (30 days of detail, then 30 days of replay tombstone).
