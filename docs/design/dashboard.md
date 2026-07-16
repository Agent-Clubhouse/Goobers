# Design: Dashboard / Portal — live read-only visual hub over the daemon API

> Status: **Draft for review — not implemented** · Area prefix: `DASH` (new), `API` (new) · Milestone: **Dashboard / Portal** (#14)
> Requirements: [`docs/requirements/portal.md`](../requirements/portal.md) (supersedes PORT-Q4 journals-direct — see §2)
> Architecture: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) · [`docs/design/v1/observability-substrate.md`](v1/observability-substrate.md)
> Related issues: #37 (Portal v1), #169 (daemon read-only API), #171 (portal retarget), #402 (live viz + DAG), #170 (CLI approval surface), #398 (`workflow show --dot`)

## 1. Why this exists

A view-only, low-latency dashboard for watching gaggles, workflows, and runs — a "hub for watching"
with delightful, professional, modern UI (light + dark). This is brain-dump **item 11**. It also hosts
**item 13 tier-1** (escalation viewing) and consumes **item 8** warnings.

**Non-negotiable architectural principle (PO):** the dashboard is built *entirely* on the same API the
CLI uses. There are **no dashboard-only code paths or mutations** — any mutation the UI offers calls the
exact same API surface the CLI does. New CLI actions should appear in the UI and vice versa; we build a
**continuous CLI↔UI parity check** to enforce this.

## 2. Current state (grounded) & the prerequisite

- **There is no daemon API.** The `goobers up` daemon runs scheduler + runner in-process and exposes
  **no HTTP/gRPC surface** (no `ListenAndServe`/`grpc.NewServer` anywhere). `api/` is Go types + JSON
  schemas, **not** a network API.
- CLI `status`/`trace`/`telemetry` each read the **on-disk journal** (`runs/<id>/…`) and telemetry
  SQLite (`internal/telemetry/rollup`) **inline** (`cmd/goobers/status.go`, `trace.go`, `telemetry.go`).
- The portal is a **mock-only React 19 + Vite scaffold** (`portal/src/api/mockClient.ts`); the
  `GoobersPortalApi` contract exists (`portal/src/api/types.ts:113`) but binds to in-memory fixtures.
  MSAL auth deps are present but unused.
- **Spec tension resolved here:** `portal.md` PORT-Q4 currently says the portal reads journals
  *directly*. That conflicts with the PO principle above and with #169/#171. **Decision (arch authority):**
  we adopt a **shared daemon read-only API** as the single source both CLI and portal read through.
  `portal.md` will be updated to reflect this. Rationale: journals-direct would fork the read logic
  between CLI and UI, breaking parity by construction and coupling the UI to on-disk layout.

**Therefore this milestone's foundation is building the daemon read-only API** (#169), then retargeting
both the CLI's read commands and the portal onto it (#171). The DAG-export work (#398, #402) feeds §4.

## 3. The shared read-only API (API)

A local HTTP (JSON) service embedded in the `goobers up` daemon, localhost-only (see §6). It exposes
exactly the read model the CLI needs, so the CLI's `status/trace/telemetry` become thin API clients and
the portal binds to the same shapes.

Endpoints (read-only for dashboard v0):

- `GET /gaggles` — provisioned gaggles + per-gaggle workflow count + active-run count.
- `GET /gaggles/{g}/workflows` — workflows with displayName, description, config (maxConcurrency,
  retry, schedule), active-run indicator.
- `GET /workflows/{w}` — workflow definition incl. **serialized execution graph** (nodes/edges from the
  compiled `workflow.Machine`) + run list (active vs historic, current stage, retry used/remaining, duration).
- `GET /runs/{id}` — run detail: per-run execution graph, per-stage status/attempts/timing, and the
  **event stream** needed to drive replay (ordered `stage.*`/`gate.evaluated` events with timestamps).
- `GET /runs/{id}/stages/{s}` — stage-run detail: logs, inputs, artifacts, repeat-invocation list.
- `GET /workflows/{w}/stages/{s}` — static stage definition (config, raw YAML).
- Mutations (later milestones, still API-first): the human-gate approve/override/rerun surface from the
  Human-in-the-Loop milestone (#16) and #170 — the dashboard calls these, never its own path.

**Parity check:** a test/CI harness that diffs the CLI command/action registry against the API route set
(and the UI's action registry against the API) so a new CLI action that has no API route — or a UI
action not backed by the shared API — fails CI.

## 4. UX specification (DASH)

Themeable (light/dark), low-latency (near-real-time to actual runs), with tasteful progress animation.
**View-only for dashboard v0** — every page footer states that editing is done via the workflow CD
(milestone #15), not the UI.

- **Home** — cards/rows of provisioned gaggles; per gaggle: # workflows defined, # active runs.
- **Gaggle Summary** — horizontal rows, one per workflow, with displayName, description, and common
  config (max concurrency, retry count, schedule).
- **Workflow Page** — two areas: (top) the **execution graph**, zoom/pan; (below) the run list with a
  clear active-vs-historic indicator, current stage for active runs, retry used/remaining, duration.
- **Run Page** — drill into one run: the run's execution graph, optionally a **delta vs the graph of a
  prior workflow version** (deferred until we have real registry versioning — see Versioning #12; stub
  the delta affordance now, populate later). A **pause / play / scrubber replay** that animates the run
  via focus/light indicators at selectable speeds (1×/2×/5×/10×/50×), making implement↔review cycles
  visually obvious. Replay is driven by the ordered event stream from `GET /runs/{id}`.
- **Stage Page (from Workflow)** — click a stage → side panel with its *definition*: what it is, inputs,
  configuration, the raw YAML (structured + pretty).
- **Stage-Run Page (from Run)** — click a stage in a run → side panel with a **[Run | Stage] tab
  selector**. *Run* tab: logs/details/outcome for that stage-invocation, with a dropdown when it ran
  multiple times (repasses/retries). *Stage* tab: the same static definition as the Stage Page.
- **Escalation view (item 13 tier-1)** — escalated/terminal runs are first-class in the run list and
  run page: show run history, artifacts-at-each-step, current state, and the gate/condition that forced
  escalation. View-only here; unblock actions come from milestone #16.

### Craft bar (explicit)
Wireframe and design-review the UI heavily; **scrub common low-quality "AI-generated app" patterns**
(generic gradients, purple-on-white default palettes, emoji-as-icons, dead placeholder cards, inconsistent
spacing). Professional, simplistic, modern. Both themes designed, not just inverted.

## 5. Data latency

Dashboard must feel live. Options to decide during build: server-push (SSE/WebSocket) from the daemon on
journal append vs short-interval poll of the read API. Leaning SSE for run/stage updates given the daemon
already observes journal writes; poll as fallback.

## 6. Auth (scope note)

Dashboard v0 is **localhost-only, local-only, no auth** (matches tier-1 posture). Scoped identity/OIDC
(Entra or generic OIDC) is **V3+** and tracked by the existing auth-seam issues (#173/#174/#38) — out of
scope here beyond not painting ourselves into a corner (the API stays behind the same authenticator seam).

## 7. Issue breakdown (milestone #14)

- **[EPIC]** Dashboard / Portal.
- API-1: Daemon read-only HTTP API — embed in `goobers up`, localhost-only (foundation; extends #169).
- API-2: Serialize the compiled execution graph (nodes/edges) + `goobers workflow show --dot` (folds #398).
- API-3: Retarget CLI `status`/`trace`/`telemetry` onto the shared API client (extends #171).
- API-4: CLI↔UI↔API parity check in CI.
- DASH-1: Portal retarget off mock onto the real API client (extends #171/#37).
- DASH-2: Home + Gaggle Summary pages.
- DASH-3: Workflow page — execution-graph viz (zoom/pan) + run list.
- DASH-4: Run page — per-run graph + replay scrubber (1×–50×) driven by event stream.
- DASH-5: Stage & Stage-Run side panels ([Run|Stage] tabs, multi-invocation dropdown).
- DASH-6: Escalation view (item 13 tier-1) + item-8 warning surfacing in the UI.
- DASH-7: Theming (light/dark) + motion system + design-review/AI-slop-scrub pass.
- DASH-8: Live latency transport (SSE/poll) decision + implementation.

## 8. Open questions

- Do we need the prior-version graph **delta** before real registry versioning lands? (Stub now, wire later.)
- SSE vs poll for the live transport (§5) — decide with a latency spike.
