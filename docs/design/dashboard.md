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

A run response includes the graph pinned to its recorded workflow
version/digest. It never reconstructs historic topology from mutable current
config.

Artifact reads use the journal's containment, digest verification, redaction,
and media-type rules. The API never exposes arbitrary filesystem paths.

## 8. Live transport

Use Server-Sent Events for local live updates, with polling as a documented
fallback.

The protocol must define:

- Snapshot-plus-cursor startup with no race between them.
- Stable event IDs/cursors.
- `Last-Event-ID` reconnect.
- Heartbeats and explicit connected/reconnecting/stale states.
- Client deduplication and ordered application.
- A measurable local update target (p95 under one second from durable append to
  visible state on the same machine).

The event stream invalidates/refetches versioned read models; it does not create
a second ad hoc shape for every page.

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
