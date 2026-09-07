# Spec: Portal

> Status: **Approved for staged implementation** · Aligned to `../ARCHITECTURE.md` (2026-07-16) · Derives from
> `../VISION.md` §4 + portal-scope decision · Area prefix: `PORT`

## Purpose

The **Portal** is the web window into a Goobers instance. It is **observability-first**,
with a deliberately **minimal interactive surface** for *runtime operations only*. It is
**not** a configuration tool — config is code.

The portal is one of two observability surfaces over the same data. At tier 1 the
primary surface is the **`goobers` CLI** (`goobers status`, `goobers trace <run-id>`);
the web portal is optional locally and becomes the natural surface for teams
(tiers 2–3).

## Model

The portal reads one versioned daemon API and nothing else. That API is a thin
adapter over a shared read service whose sources are:

- **Run journals** (`ARCHITECTURE.md §4`) — the append-only, content-digested record
  every run produces, at every tier.
- **The run-telemetry store** — journal spans + the local rollup store at tiers 1–2;
  ADX at tier 3.

Together these feed a **stable product contract**: the portal never reads files,
SQLite, a runner's internals (local runner state or Temporal history), or task
queues directly. The daemon API and CLI read commands share the same read
service, so the product model does not drift by surface.

The dividing line for interactivity is **config-time vs. runtime**:

- **Config-time → code only.** Creating/changing gaggles, goobers, workflows, and gates
  happens in the `config` repo/directory; provisioning in `instance.yaml` (tiers 1–2)
  or the `infra` repo (tier 3). The portal never configures these.
- **Runtime ops → minimal portal interactivity.** A small set of operational actions that
  can't be (or are awkward to) express declaratively: **human-gate approvals** and
  **run-level intervention** (e.g. retry/abort). Kept minimal by design.

Primary value is observability: the live state of the gaggle and the journal/trace
behind every unit of work.

## Requirements

### Observability (primary)
- **PORT-001 (MUST):** On first boot the observability surface (CLI at tier 1, portal
  where deployed) MUST show an "I'm alive / ready" state with no gaggles/goobers
  configured. *(All tiers)*
- **PORT-002 (MUST):** The portal MUST show the current workforce — gaggles, goober
  definitions ("team members"), workflows — and live run status. *(All tiers)*
- **PORT-003 (MUST):** The portal MUST surface traces/telemetry for runs (observability
  into what happened and why) from the run journal and the goober-run telemetry store.
  *(All tiers)*
- **PORT-004 (MUST):** The initial portal view MUST prioritize runs needing human
  attention, active runs, and recent outcomes over aggregate vanity metrics.
- **PORT-005 (MUST):** Run detail MUST coordinate the pinned execution graph with
  the ordered journal event ledger, stage attempts, scalar outputs, and artifact
  provenance.
- **PORT-006 (SHOULD):** Completed run history SHOULD support event-sequence replay.
  Replay MUST remain understandable with animation disabled.

### Interactivity (minimal, runtime only)
- **PORT-010 (MUST):** The portal MUST NOT be used for setup/configuration; all config is
  declarative code (`CFG-005`). *(All tiers)*
- **PORT-011 (MUST):** The portal MUST provide a surface for **human-gate approvals**
  (resolving `GT-Q2`), letting an authorized human approve/reject a paused gate.
- **PORT-012 (SHOULD):** The portal SHOULD provide minimal **run-level operational
  actions** (e.g. retry, abort/cancel, intervene) — scoped to operations, not config.
- **PORT-013 (SHOULD):** Interactive actions SHOULD be access-controlled (who may approve
  gates / intervene) per the auth ladder (`SEC-*`): none at tier 1, optional OIDC at
  tier 2, **Tier 3 (V2):** Entra ID SSO + RBAC — coordinate with Security.

### Data contract & tiers
- **PORT-020 (MUST):** The portal MUST read only the versioned daemon product API.
  The API's shared read service reads provisioned definitions, the **run journal**,
  and the **run-telemetry store** - never private runner structures or raw Temporal
  history/task queues. *(All tiers)*
- **PORT-021 (MUST):** At tier 1 the **`goobers` CLI** (`status`, `trace <run-id>`) is
  the primary observability surface and MUST cover PORT-001/-002/-003 semantics
  (liveness, workforce + run status, per-run trace) without requiring the web portal
  to be running. *(Tiers 1–2)*
- **PORT-022 (MUST):** The web portal MUST be **optional** at tiers 1–2: an instance is
  fully operable and observable (including gate approvals — see PORT-023) with the
  portal not deployed. *(Tiers 1–2)*
- **PORT-023 (MUST):** Where the portal is not running, human-gate approvals MUST have
  a non-portal path (CLI approval and/or the git PR surface for code-merge gates) so
  PORT-011 never makes the portal a hard dependency at tier 1. *(Tiers 1–2)*
- **PORT-024 (SHOULD):** At tiers 1–2, the daemon API SHOULD serve the product
  contract directly from the on-disk journal + `telemetry.db`; no additional
  durable store is required. The browser never receives filesystem paths.
- **PORT-026 (MUST):** A historic run MUST render against the execution graph
  pinned by its recorded workflow version/digest, not mutable current config.
- **PORT-027 (MUST):** Portal artifact access MUST preserve journal containment,
  digest verification, redaction, and media-type controls.
- **PORT-028 (MUST):** The portal MUST provide keyboard operation, reduced-motion
  support, non-color status cues, and WCAG AA text/control contrast.
- **PORT-029 (MUST):** Run summaries and detail MUST mark a running run stale only
  when both its `lastActivityAt` age and the scheduler heartbeat age exceed the
  configured `runner.livenessTimeout` (two minutes by default). Terminal runs,
  runs with recent activity, and runs observed through an offline reader with no
  daemon heartbeat remain unmarked. Run list and detail views MUST render the
  stale state as "Stale / unmonitored", distinct from "Running".
- **PORT-025 (WON'T (v1)):** Runner-specific operational views (e.g. Temporal worker
  health, task-queue depth) are not portal scope; if ever surfaced they arrive as
  tier-3 annotations on the same journal shape, not a separate UI. **Tier 3 (V2).**

- **PORT-030 (MUST):** *(Tiers 1–3, shipped)* The portal-config read
  (`GET /api/v1/portal/config`) MUST carry a **capability object** alongside the
  co-branding payload, reporting which optional interactive surfaces this daemon
  was wired with (today `revealRun` and `workflowEnable`). The portal MUST use it
  to decide whether to render those controls. A capability flag reports **daemon
  wiring, not authorization**: a true flag means the seam exists, never that the
  caller may use it — the acting route remains responsible for authorization.
  Adding a flag is an API-contract change (`internal/apicontract`'s wire
  fixture), not a co-branding change.
- **PORT-031 (MUST):** *(Tiers 1–3, shipped)* Any portal action whose effect is
  **local to the daemon's own machine** MUST be withheld when the daemon's
  listener is not loopback, because off-loopback the daemon's machine is not
  necessarily the requesting user's. The reveal-run action is the shipped
  instance of this rule: the revealer is wired only under a loopback listener, in
  both `goobers up` and standalone `goobers dashboard`, and the capability then
  reports false. Withholding the control is required rather than failing at click
  time — a hidden control is the honest signal that the action does not apply to
  this deployment. See `docs/design/portal-reveal-remote-posture.md`.
- **PORT-032 (SHOULD):** *(Fleet, partially shipped)* The portal SHOULD remain a
  window over a **single instance**. Fleet-level aggregation across instances is
  designed in `docs/design/fleet-portal.md` and is **not** shipped: the shipped
  fleet slice is instance-side enrollment and connection only (`INST-015`–`INST-017`).
  Until a fleet read model and dashboard exist, no portal surface aggregates
  across instances, and this requirement is the record that the boundary has not
  moved.

## Relationships

- Reads → **run journals** (`ARCHITECTURE.md §4`) and the **Telemetry** run store
  (rollup at tiers 1–2, ADX at tier 3) — never runner internals.
- Complements → the **`goobers` CLI** (`status` / `trace`), the tier-1 primary surface.
- Hosts → **human-gate** approvals (`GT-012`) and run interventions.
- Surfaces → the **Instance** / **Gaggle** / **Goober** / **Workflow** state.
- Authenticated by → the **Security** auth ladder (none → optional OIDC → Entra).
- Never configures → anything (that's **Config-as-code**).

## Open questions

- **PORT-Q1:** **Resolved (default):** approvals live in the **portal** where deployed,
  with notifications (e.g. Teams/email) linking back; code-merge gates may also ride
  the git PR; at tier 1 the CLI/PR path stands in (PORT-023). *(Build-time: which
  notification channels.)*
- **PORT-Q2:** **Resolved (default):** v1 runtime actions = **gate approvals + run
  retry/abort/cancel**. Nothing more.
- **PORT-Q3:** **Resolved:** auth is a **ladder by tier** (`ARCHITECTURE.md §9`): none
  at tier 1, optional OIDC at tier 2; **Tier 3 (V2):** **Microsoft Entra ID**
  (SSO + RBAC) — see `SEC-020`. This is where Entra goes when you scale.
- **PORT-Q4:** **Resolved:** the browser uses the versioned daemon API. The API
  and CLI share a journal/telemetry-backed read service; there is no browser
  journals-direct path and no dashboard-only read implementation.

## Co-branding requirements

- **PORT-CBR-001 (MUST):** The portal MUST read brand identity (name, tagline,
  scope mark, logo, favicon) from `GET /api/v1/portal/config` on startup and
  apply it. An unconfigured instance MUST render standard Goobers defaults with
  no visible difference from today.
- **PORT-CBR-002 (MUST):** Accent color token overrides declared in
  `instance.yaml` under `portal.theme` MUST be applied to the portal's CSS
  token layer without altering semantic status colors (success, warning, danger).
  Both light and dark mode variants MUST be independently overridable.
- **PORT-CBR-003 (MUST):** The portal MUST render a support footer in the
  sidebar when any `portal.support` field is configured, with links for Docs,
  Get help, Chat, and operator-defined custom links (max 6). The footer MUST be
  hidden entirely when no support fields are set.
- **PORT-CBR-004 (MUST):** Logo and favicon assets MUST be served from the
  instance's `assets/` subdirectory via `GET /assets/<path>`. The handler MUST
  prevent path traversal outside the instance root.
- **PORT-CBR-005 (MUST):** All cobrand config URLs MUST be validated at
  `goobers validate` and daemon startup: asset URLs scoped to `/assets/`,
  support URLs as absolute HTTPS (or allowed deep-link schemes), accent values
  as valid CSS colors. Invalid values MUST block startup (color) or produce
  validation warnings (missing asset files).
- **PORT-CBR-006 (SHOULD):** The portal SHOULD update `document.title` and the
  favicon to reflect the configured brand on load.
- **PORT-CBR-007 (WON'T):** Per-gaggle theme overrides are out of scope.
  Branding is instance-wide identity. Full white-label (removing upstream
  attribution) is out of scope.

Design authority: [`docs/design/cobrand.md`](../design/cobrand.md).
