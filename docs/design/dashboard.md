# Design: Dashboard / Portal - calm operations workbench over the daemon API

> Status: **Approved for staged implementation** (2026-07-16)
> Area prefixes: `API`, `DASH`
> Milestone: **Dashboard / Portal - calm live operations workbench** (#14)
> Epic: [#440](https://github.com/Agent-Clubhouse/Goobers/issues/440)
> Requirements: [`docs/requirements/portal.md`](../requirements/portal.md)
> Architecture: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md)
> Prototype: [`portal/`](../../portal/)

## 1. Product decision

The dashboard is a calm operations workbench for understanding:

1. What is happening now?
2. What needs human attention?
3. Why did a run reach its current state?
4. What did each stage produce?

It is not a chat client, a configuration editor, a marketing dashboard, or a
second orchestration path.

The product shorthand is:

> **Workbench, not command center. Ledger, not chat. Signal, not spectacle.**

The supplied Goobers mascot is the identity anchor. It may appear in the product
mark, first-boot state, and selected explanatory moments. It is not repeated as
an agent avatar and does not turn runtime state into character animation.
Operational language remains literal even when the brand is playful.

## 2. Non-negotiable architecture

The journal is the source of truth. A shared read service projects the journal,
telemetry rollup, and provisioned definitions into a versioned product contract.
The daemon's loopback HTTP API and CLI read commands are adapters over that same
service.

```text
run journals + telemetry rollup + provisioned definitions
                         |
                  shared read service
                  /                 \
      daemon loopback HTTP API      CLI read commands
                  |
               portal
```

This resolves the former `PORT-Q4` journals-direct position:

- The browser never reads journal files or telemetry SQLite directly.
- HTTP handlers do not own a second read implementation.
- CLI commands do not each reconstruct product state independently.
- Historic CLI inspection must remain available when the daemon is not running,
  through the same shared service in-process.

API parity is scoped to **runtime read models and runtime capabilities**. It does
not mean every CLI command appears in the portal. Setup, validation, config
authoring, daemon lifecycle, and other config-time commands remain CLI-only.

## 3. Current state and prototype authority

As of approval:

- `goobers up` has no HTTP server.
- `status`, `trace`, and `telemetry` read journals/SQLite independently.
- The existing `GoobersPortalApi` is a walking-skeleton contract. Its status
  enums and response shapes are not the production API contract.
- The portal prototype in `portal/` is the interaction and visual reference,
  backed by realistic static fixtures.

The prototype deliberately covers:

- Overview, workflow inventory, workflow detail, run history, and run detail.
- An execution graph synchronized with an ordered event ledger.
- Escalation cause, repass budget, attempts, outputs, and artifacts.
- Replay controls and event-by-event inspection.
- Light and dark themes, responsive layouts, keyboard focus, and reduced motion.

It is not production implementation. It uses static data, a fixture-specific
graph layout, and intentionally keeps components together for rapid iteration.
Production issues may refactor it while preserving the accepted behavior and
visual principles.

## 4. Information architecture

The portal has three primary destinations.

### 4.1 Overview

Answers "what needs me?" before "what exists?"

- Daemon connection and freshness.
- Runs needing attention.
- Active runs and their current stages.
- Recent outcomes.
- Active instance/config warnings.

There are no vanity KPI cards. Counts appear only when they help interpret the
operational lists.

### 4.2 Workflows

A dense inventory grouped or filtered by gaggle:

- Name and purpose.
- Trigger.
- Active/max concurrency.
- Last outcome and recency.

Workflow detail contains the static execution graph, definition/config summary,
stage inspection, and run history. A gaggle is primarily scope and context, not
a mandatory navigation step. Gaggle detail includes the provisioned goober
definitions (role, skills/capabilities, status, and workflow/stage ownership)
required by `PORT-002`; it does not promote the roster to a primary navigation
destination until richer capacity/health data exists.

### 4.3 Runs

A filterable history across workflows and gaggles. Run detail is the primary
diagnostic surface:

- Identity, pinned workflow version/digest, trigger, duration, and state.
- A fixed execution graph.
- An ordered, durable event ledger.
- Stage attempts, outputs, artifacts, and definition.
- Escalation cause when applicable.
- Replay for completed history and live-follow for active runs.

## 5. Interaction model

### 5.1 Graph and time stay separate

The graph explains structure. The event ledger explains time and causality. They
are coordinated but not collapsed into one overloaded visualization.

Selecting a graph node opens its current attempt context. Selecting an event
moves the playhead and selects the event's stage. Repasses reuse the same graph
nodes and appear as attempt counts/traversals, not duplicated nodes.

Tasks and gates use one stable visual grammar:

- Deterministic and agentic tasks share the task shape; kind and owner differ.
- Gates share the gate shape; evaluator kind differs.
- Color never carries state alone.
- Branch edges retain their declared outcome labels.

### 5.2 Replay is an explicit mode

Replay is driven by ordered journal sequence, not animation timing or OTel
spans. It provides:

- Play/pause.
- Direct scrubbing.
- Previous/next event keyboard operation.
- Selectable speed.
- Compressed/skip-idle time for long waits.
- Current event title, elapsed time, and durable sequence.
- A reduced-motion presentation with equivalent information.

The prior-version graph delta affordance is removed until real registry
versioning can back it. The UI does not ship dead controls.

### 5.3 Attempt inspector

The inspector uses **Attempt** and **Definition** concepts, not the ambiguous
`Run | Stage` labels from the earlier draft.

For a selected stage it shows:

- Attempt number and class (`initial`, `policy`, `infra`).
- Status and duration.
- Outcome summary and scalar outputs.
- Artifacts, with type, size, digest/provenance, and safe content access.
- Static definition and raw YAML through progressive disclosure.

Raw logs and transcripts are secondary diagnostic detail. Artifacts and
structured outcomes are first-class because they are the durable review units.

### 5.4 Escalation

Escalation is not a generic error page. The same run detail surface gains a
causal summary:

- Gate/condition that selected escalation.
- Selected branch.
- Repass/retry budget consumed.
- Terminal reason.
- Attempts and artifacts available at the point of escalation.

This milestone is view-only. Intervention controls arrive from the
Human-in-the-Loop milestone and call the same runtime API as the CLI.

## 6. Visual and content system

### 6.1 Brand

- The mascot anchors the product mark and selected empty states.
- Purple is a restrained brand accent, not a page background or generic
  gradient system.
- The brand can be friendly; runtime copy stays precise.
- No agent faces, moods, "thinking" theater, or anthropomorphic failure copy.

### 6.2 Surfaces and depth

- Base surfaces are opaque and border-defined.
- Shadow indicates real elevation only: menus, dialogs, and floating inspectors.
- Transparency is used only when it explains layering.
- Cards are reserved for true grouped objects; operational lists remain dense.

### 6.3 Motion

Motion explains a transition:

- Node activation and edge traversal.
- Live-follow updates.
- Inspector/dialog entry and exit.
- Replay progress.

No ambient glow, decorative pulse, or perpetual animation. All motion honors
`prefers-reduced-motion`.

### 6.4 Theme and accessibility

Light and dark are independently tuned themes, not inversions. Baseline:

- WCAG AA contrast for text and controls.
- Status encoded by text/icon/shape as well as color.
- Full keyboard operation for navigation, graph selection, ledger, replay, and
  inspector controls.
- Screen-reader labels for graph topology and state.
- Reduced-motion support.
- Colorblind-safe semantic states.
- Desktop-first, functional tablet/mobile layouts without clipped controls.

## 7. Shared read model and HTTP contract

The production contract is versioned under `/api/v1`. IDs are stable and
unambiguous; workflow identity includes gaggle and version/digest where needed.
List endpoints support deterministic sorting, pagination/cursors, and filters.
Errors use one structured envelope.

```json
{
  "error": {
    "code": "not_found",
    "message": "route not found"
  }
}
```

Every route, including unknown routes, method errors, authorization failures,
and internal read failures, uses this envelope. `code` is stable for adapters;
`message` is safe for display and does not expose internal paths or errors.
`goobers up` binds the API to `127.0.0.1:8080` by default. `api.listen` may
select another numeric port or loopback address; wildcard and non-loopback
listeners are rejected during instance configuration validation.

Minimum read surfaces:

- `GET /api/v1/health`
- `GET /api/v1/instance`
- `GET /api/v1/gaggles`
- `GET /api/v1/gaggles/{gaggle}/goobers`
- `GET /api/v1/gaggles/{gaggle}/workflows`
- `GET /api/v1/gaggles/{gaggle}/workflows/{workflow}`
- `GET /api/v1/runs`
- `GET /api/v1/runs/{run}`
- `GET /api/v1/runs/{run}/events`
- `GET /api/v1/runs/{run}/stages/{stage}/attempts`
- `GET /api/v1/runs/{run}/artifacts/{digest}`
- `GET /api/v1/telemetry/...` for the existing CLI telemetry projections
- `GET /api/v1/events` for live updates

The exact wire representation is defined in Go contract types and tested against
the TypeScript client. It must preserve canonical journal phases:
`running`, `completed`, `failed`, `aborted`, and `escalated`. Presentation
groupings such as "Needs attention" are UI derivations, not new persisted states.

Each event returned by `GET /api/v1/runs/{run}/events` adds a `category` and
`replayChapter` to the unmodified journal projection. Categories are the bounded
values `transition`, `decision`, `result`, `evidence`, `liveness`,
`bookkeeping`, and `unknown`. The chapter flag is the service's deterministic
recommendation for replay navigation: run/stage transitions, gate and condition
decisions, failures or escalations, terminal events, and meaningful external
results such as pull requests are chapters; heartbeats, spans, artifacts, and
maintenance records are not. `ref.touched` is payload-sensitive: pull requests
are results while other refs are bookkeeping. Unknown event types or schemas
remain in sequence with category `unknown` and are not chapters.

This metadata is only a presentation/query classification. It does not filter,
rewrite, or authorize retention of journal records; the complete append-only
journal remains authoritative and every event remains queryable in raw sequence
order.

A run response includes the graph pinned to its recorded workflow
version/digest. It never reconstructs historic topology from mutable current
config.

Artifact reads use the journal's containment, digest verification, redaction,
and media-type rules. The API never exposes arbitrary filesystem paths.

## 8. Live transport

Use Server-Sent Events for local live updates, with polling as a documented
fallback.

`GET /api/v1/events` returns `text/event-stream`. A new connection first
receives a `snapshot` event whose `id` and `data.cursor` are the same cursor and
whose models are `instance`, `run`, and `workflow`. The server registers the
subscriber atomically with that snapshot, so clients can refetch the ordinary
read endpoints and then apply every later invalidation without a missed-update
window.

Durable journal records produce ordered `invalidate` events. Each event has a
session-scoped monotonic `id`, repeats that value in `data.cursor`, names the
coarse models to refetch, and may narrow the change with `runIds` or workflow
identities. Durable run checkpoint changes also invalidate their run and
workflow models, including human-gate transitions that append no event.
Clients apply events in ID order and ignore an ID they have already applied.
They reconnect with `Last-Event-ID`; the daemon replays retained events with
their original IDs before continuing live delivery. Replay history is bounded.
A malformed cursor returns `400 invalid_cursor`; a cursor from an older daemon
session or outside the retained window returns `409 stale_cursor`.

The stream sends `heartbeat` events without IDs during idle periods. A client
is connected after the initial snapshot or replay completes, reconnecting
after a transport disconnect, and stale after `409 stale_cursor`. Slow clients
are disconnected rather than back-pressuring journal writers. Daemon shutdown
closes active streams before waiting for HTTP handlers. The local update target
is p95 under one second from durable append to visible state on the same
machine.

Polling is the complete recovery fallback: after an unavailable stream or
stale cursor, refetch the current `/api/v1/instance`, inventory, run, and
telemetry endpoints, discard the old cursor, and reconnect without
`Last-Event-ID`. Those versioned reads are the source of current state; SSE only
invalidates them and never carries page-specific state.

## 9. Delivery order

Implementation is intentionally split into independently reviewable PRs:

1. Shared read-service and UI foundations.
2. Canonical graph and inventory/run read contracts.
3. CLI convergence onto the shared service.
4. Overview, workflow, and run vertical slices.
5. Attempt inspection, replay, escalation, warnings, and live updates.
6. Runtime capability parity enforcement.

Every issue in epic #440 states its exact dependencies. An issue may be approved
before dependencies merge, but implementation must not open a production PR
until its blockers are complete.

### 9.1 Issue map

| Slice | Issue | Merge boundary |
|---|---:|---|
| Shared read service + HTTP lifecycle | #441 | Go foundation |
| Canonical graph projection after #398 | #442 | Compiler/contracts + CLI refactor |
| Instance/gaggle/goober/workflow reads | #510 | Go API |
| Run/event/attempt/artifact reads | #511 | Go API |
| `status` convergence | #443 | CLI adapter |
| `trace` convergence | #512 | CLI adapter |
| Telemetry reads + CLI convergence | #513 | Go telemetry vertical slice |
| Contract/capability parity | #444 | Contract + CI |
| SSE update stream | #452 | Go transport |
| Production UI foundation | #451 | TypeScript/CSS foundation |
| Typed daemon client | #445 | TypeScript data layer |
| Overview + gaggle/goober/workflow inventory | #446 | Portal page slice |
| Workflow detail + graph | #447 | Portal page slice |
| Run detail + ledger | #448 | Portal page slice |
| Stage-attempt inspector | #449 | Portal component slice |
| Deterministic replay | #514 | Portal interaction slice |
| Escalation view | #450 | Portal view slice |
| Config warnings | #515 | Portal view slice |
| Live client/freshness | #518 | TypeScript live-data slice |

## 10. Explicit non-goals for V1

- Chat or direct agent steering.
- Config/setup editing.
- Workflow authoring.
- Human intervention mutations (owned by Human-in-the-Loop).
- Cloud/team auth beyond preserving the authorizer seam.
- Generic infrastructure/Temporal internals.
- Prior-version graph diff.
- Mobile feature parity with the desktop diagnostic workspace.

---

# Addendum: V1.1 - diagnostic depth, insight, and visualization

> Status: **Approved for staged implementation** (2026-07-21)
> Area prefixes: `API`, `DASH`
> Milestone: **Dashboard / Portal - calm live operations workbench** (#14) follow-on
> Supersedes nothing above; extends sections 4-10 after the V1 slices shipped.

## 11. Why this pass exists

The V1 slices (DASH-1 - DASH-15, epic #440) shipped the foundation: a shared read
service, a versioned API, SSE freshness, an attention-first Overview, workflow
inventory, run detail with an event ledger, and accessibility/theming baselines.
Living with it against a real instance surfaced a consistent gap - the workbench
answers *"what exists"* and *"what is running"* well, but is weak at the two
questions operators actually open it for: **"why did this run reach this state?"**
and **"how is my instance doing over time?"**

Three structural facts drive this pass. They are stated plainly because they
change the shape of the work from "build new" to mostly "connect and promote what
already exists."

1. **A capable diagnostic layer was built against fixtures and never mounted.**
   `AttemptInspector`, `ArtifactViewer`, `EscalationPanel`, and the replay engine
   (`portal/src/replay.ts`) all exist, but each imports the prototype fixture
   model (`prototypeData.ts`) and has **zero render sites** in any app page. The
   live `RunPage` shows only a node list and a one-line-per-row event ledger. The
   daemon already returns `run.escalation` (structured cause), per-event
   `outputs`/`artifacts`, and per-attempt records via `listStageAttempts` /
   `getArtifact` - none of which the run page reads. The remediation is to wire
   these components to the live contract, not to invent them.

2. **A real topology graph exists, but only on the wrong surface.**
   `WorkflowTopologyGraph` is a genuine 2D SVG graph - topological layout, drawn
   edges with arrow markers, zoom/pan/fit. It renders on the workflow *definition*
   page. The *run* page renders a downgraded vertical `<ol>` list with `->` text
   edges. Runs deserve the real graph, overlaid with live per-node state.

3. **The analytics data already exists; the portal renders none of it.**
   `telemetry.db` durably holds run outcomes, per-stage attempt status and
   duration, and per-stage token/cost usage. `GET /api/v1/telemetry/stats` already
   returns per-workflow/stage success rate, P50/P95 duration, and P50/P95
   token/cost; `GET /api/v1/telemetry/errors` returns coded failure records. The
   portal has no insight destination and never calls these endpoints.

The product framing is unchanged: **workbench, not command center. Ledger, not
chat. Signal, not spectacle.** This pass deepens signal; it does not add
spectacle.

## 12. Run detail becomes the real diagnostic surface

Run detail keeps the section 5 model - the graph explains structure, the ledger
explains time, and they stay coordinated but separate. What changes is depth.

### 12.1 The run graph is the canvas graph, with live state overlay

Promote `WorkflowTopologyGraph` to the run surface. The pinned run graph renders
as the 2D topological canvas, and per-node run state (`pending`, `running`,
`completed`, `failed`, `escalated`, `skipped`) is overlaid on the same nodes,
computed "as of" the selected sequence exactly as `deriveNodeStates` does today.
Traversed edges are emphasized; the actual path taken through the graph is
visually distinct from declared-but-untraversed branches. State is never carried
by color alone (section 6.4 holds): node shape/icon and a text state label
continue to encode state for colorblind and screen-reader users, and the graph
retains full keyboard navigation and an equivalent list view under the
reduced-motion / narrow-viewport paths.

### 12.2 Selecting a node opens an inspector, not just a highlight

Today a node click sets `aria-pressed` and nothing else. It must open a
progressive-disclosure inspector for the selected stage's **current attempt in the
selected traversal**, backed by the live contract:

- Attempt number and class (`initial`, `policy`, `infra`), status, duration.
- Scalar `outputs` for the attempt.
- Artifacts with type, size, digest/provenance, and safe content access through
  the existing artifact-read rules (the `ArtifactViewer` already does this against
  fixtures; point it at `getArtifact`).
- Static definition and raw YAML behind disclosure.

This is the section 5.3 "Attempt inspector" contract, finally mounted against live
data. Artifacts and structured outcomes remain the first-class review units; raw
logs/transcripts stay secondary.

### 12.3 Escalation has one authoritative cause and one causal path

Escalation is currently ambiguous in two ways, both observed on a real run:

- **Double-surface.** The run header badge (`run.phase`) and per-node
  `run-node-state-escalated` are derived by two independent code paths with no
  reconciliation, so a run can show several "Escalated" markers with no indication
  of which is authoritative.
- **Two terminal park nodes read alike.** Workflows commonly declare both a
  `park-escalated` (-> `@escalate`) and a `park-needs-human` (-> `@abort`) sink.
  Both look terminal-and-human-ish in the graph, and nothing marks which one the
  run actually reached or which gate selected it.

The fix surfaces the daemon's structured `run.escalation` (`EscalationCause`:
selector, selected branch, repass/retry budget consumed, terminal reason, causal
event sequence) as a single authoritative "why" summary on run detail, and
highlights exactly one causal path: the triggering gate -> the selected branch ->
the terminal sink actually reached. The other declared sinks render as
untraversed. `EscalationPanel` is repurposed for this, rewritten against the live
`EscalationCause` shape (the fixture shape's `summary`/`budget` fields do not
exist on the contract and must not be reintroduced). The "why" is often a
reviewer verdict artifact (e.g. `verdict/review-N.json`); the panel links to it
through the same artifact inspector rather than restating it.

This milestone stays **view-only**. Acknowledging, re-queuing, or overriding an
escalation remains Human-in-the-Loop and calls the same runtime API as the CLI.

### 12.4 Replay is a real scrubber over live events

Section 5.2 specified replay; DASH-7 landed the engine but it was never mounted
and is written against the fixture event shape (which has a string `.elapsed`
field the live `RunEvent` lacks). Rebuild replay against the live ordered event
stream and mount it on run detail: a progress bar with play/pause, direct
scrubbing, previous/next event, selectable speed, and compressed/skip-idle time
for long waits. Replay drives the same `selectedSeq` that already recomputes graph
node state, so pressing play animates the state machine forward - node activation
and traversed-edge emphasis advance event by event. It is ordered by durable
sequence, not wall-clock or OTel timing, and honors `prefers-reduced-motion` with
an equivalent stepped presentation.

## 13. Insight - a new primary destination

The V1 Overview deliberately avoided "vanity KPI cards," and that stance holds for
Overview. But operators have standing questions that are not vanity and are not
answerable today:

- What is the success/failure rate of my instance, a gaggle, a workflow, a stage?
- For the stages that fail or escalate, what are the main reasons?
- Which stages are the slowest?
- Which goober/agentic stages incur the most AI cost?

These are diagnostic, not decorative. Add an **Insight** destination (fourth
primary alongside Overview / Workflows / Runs) that answers them. The distinction
from "vanity KPIs" is strict: every number is a question an operator asked,
filterable by scope (instance / gaggle / workflow / stage) and time window, and
each row drills through to the runs behind it. No lone hero metrics, no
gradient-for-its-own-sake tiles.

### 13.1 What is already backed by data

Most of Insight is a surfacing job over existing rollups:

| Insight view | Backing data | Status |
|---|---|---|
| Success/failure rate by workflow & stage | `/api/v1/telemetry/stats` (`successRate`, counts) | Exists, exposed |
| Slowest stages (P50/P95/min/max/avg duration) | `/api/v1/telemetry/stats` (stage rows) | Exists, exposed |
| AI cost & tokens by stage/workflow | `stage_usage` (P50/P95 tokens, nanoAIU-derived USD cost, retry-waste) via stats | Exists, exposed |
| Failure-reason breakdown | `/api/v1/telemetry/error-signatures` (`run_errors` + `TopErrorSignatures`) | Exists, exposed |
| Success/failure rate **per gaggle** (all gaggles at once) | `runs.gaggle` column | Exists, needs a thin `GROUP BY gaggle` query |

Visualization follows the section 6 system and the repo's dataviz conventions:
dense, honest, colorblind-safe, light/dark-tuned; distributions shown as
distributions (P50/P95), not as single averages that hide the tail.

### 13.2 What needs new capture

Two questions cannot be answered from existing columns and require durable
capture before they can be surfaced:

- **Structured escalation cause.** Runs escalate, but there is no escalation-cause
  code - only run `status='escalated'`, a gate `escalated:true` flag, and free-text
  `state.Reason`. "Top reasons runs escalated," as a category, needs a coded cause
  on the escalation event and a telemetry column. This also sharpens 12.3.
- **Per-model token/cost attribution.** `stage_usage` records tokens and
  nanoAIU-derived cost but
  the harness transcript **sums across models and discards model identity**, and
  there is no model column. "Which model/goober incurred cost" needs the model
  dimension captured at ingest. Note also that usage today is sourced from the
  Copilot harness transcript; non-Copilot runners leave usage null, which the
  Insight views must show as "unmeasured," never as zero.

Insight reports `AttrUsageCostUSD`, derived from Copilot `TotalNanoAIU`, as its
sole usage-cost figure. Premium-request quota values remain available only as a
legacy wire-contract field and are not presented as credits or cost.

## 14. Read-path performance - lists must not rescan the journal

The slow, worsening-with-history loading is not a client problem - the client is
already keyset-paginated over `limit`/`cursor`. The read service is the
bottleneck: `ListRuns` calls `runSummaries`, which `ReadDir`s every run directory
and fully parses each `events.jsonl` on **every** request, then filters and
paginates *after* materializing all of it. Cost is O(total_runs x events_per_run)
regardless of the page size requested, and the Overview fans this out into ~5
concurrent full scans per refresh (one per phase). On a young self-hosting
instance this is already ~20k runs / ~900MB of journals; it degrades linearly and
will hit every operator, not just large ones.

The durable fix is an indexed summary read path. The telemetry rollup already
holds exactly the columns a run list needs - `runs(run_id, workflow,
workflow_version, gaggle, trigger_*, status, started_at, finished_at,
duration_ms)`. Back list/summary reads (Runs page, Overview groups, `status`)
with that index (or an equivalent maintained run-summary store) so a bounded,
filtered, sorted page is a bounded query, not a full-history parse. Run *detail*
continues to read the authoritative journal for the one run in view. The journal
remains the source of truth (section 2); the index is a derived, rebuildable
projection, consistent with the shared-read-service architecture.

## 15. Smaller corrections carried in this pass

- **The loading spinner never animates.** `.loading-mark` is a border-ring with a
  tinted top arc but no `animation` property, and no `@keyframes` rotation is
  defined anywhere - so it paints frozen at the top of a rotation that never runs.
  Add the keyframe and animation; it is then correctly clamped by the existing
  `prefers-reduced-motion` block.
- **Attention shows every stale escalation forever.** Escalation is a permanent
  terminal phase with no lifecycle beyond the five canonical phases, and the
  attention list filters purely on that phase with no recency window - so an old
  escalation reappears until 20 newer ones push it off. Add a recency window and a
  session-scoped dismiss (the config-warnings dismiss is the existing precedent) so
  the list reflects what currently needs a human. A **durable, cross-session
  acknowledgement** is deliberately deferred: it is a persisted-state mutation and
  therefore belongs with Human-in-the-Loop, not this view-only pass.

## 16. Delivery order and issue map (V1.1)

Independently reviewable, each stating its blockers. Surfacing work (existing data)
is separated from capture work (new data) so the capture gaps never block the
views that already have data.

| Slice | Issue | Depends on | Merge boundary |
|---|---|---|---|
| Animate loading spinner | DASH-16 | - | CSS fix |
| Attention recency window + session dismiss | DASH-17 | - | Portal view slice |
| Index-backed run list/summary reads | DASH-18 | - | Go read service |
| Canvas run graph with live state overlay | DASH-19 | - | Portal graph slice |
| Live stage/attempt/artifact inspector on node click | DASH-20 | DASH-19 | Portal component slice |
| Authoritative escalation cause + causal-path highlight | DASH-21 | DASH-19, DASH-20 | Portal view slice |
| Deterministic replay scrubber over live events | DASH-22 | DASH-19 | Portal interaction slice |
| Insight destination: success/failure rate + slowest stages | DASH-23 | - | Portal page slice |
| Failure-reason breakdown (surface `TopErrorSignatures`) | DASH-24 | DASH-23 | Go telemetry route + portal |
| AI cost & token analytics by stage/workflow | DASH-25 | DASH-23 | Portal page slice |
| Capture: per-model token/cost attribution | DASH-26 | - | Go telemetry capture |
| Capture: structured escalation-cause code | DASH-27 | - | Go journal/telemetry capture |

DASH-24's escalation-reason categorization and DASH-21's cause summary become
fully complete once DASH-27 lands, but both ship useful behavior on existing data
first (coded run/stage errors, and the existing `EscalationCause` fields) and are
not blocked on it.

## 17. Non-goals for this pass

- Durable, cross-session escalation acknowledgement or any run mutation (still
  Human-in-the-Loop).
- Alerting, budgets, or cost caps - Insight reports; it does not enforce.
- Custom/user-defined dashboards or saved queries.
- Billing-grade cost accounting - usage is best-effort from runner transcripts and
  is labeled "unmeasured" where a runner does not report it.
- Cross-instance / fleet aggregation (single local instance stays the scope).
- Replacing the ledger with the graph, or vice versa - they stay coordinated but
  separate.
