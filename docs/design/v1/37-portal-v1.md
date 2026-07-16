# Historical design: Portal v1 - superseded by dashboard and HITL designs

> Status: **Superseded for dispatch** (2026-07-16) · Historical context only
> Requirements: [`docs/requirements/portal.md`](../../requirements/portal.md)
> (PORT-010..025), [`docs/requirements/gate.md`](../../requirements/gate.md) (GT-003/GT-012)
>
> Read/dashboard work moved to [`../dashboard.md`](../dashboard.md) and epic
> [#440](https://github.com/Agent-Clubhouse/Goobers/issues/440). Human approval,
> pause/resume, and intervention moved to
> [`../human-in-the-loop.md`](../human-in-the-loop.md) and epic #462.
> Do not dispatch P0-P4 from this document.

## Supersession map

| Historical slice | Current authority |
|---|---|
| P0 human gate + pause/resume | HITL issues #465-#469 |
| P1 daemon read API | Dashboard API issues under #440 |
| P2 CLI approve/reject | HITL issue #466 |
| P3 portal retarget | Dashboard UI issues under #440 |
| P4 access-control seam | HITL issue #469 |

The remainder is retained to explain earlier decisions and issue history. Where
it conflicts with the approved dashboard or HITL designs, those designs win.

## 1. Verdict

**The read surfaces are cheap; the load-bearing piece is the human gate.** The observable
data already exists — [`internal/journal/reader.go`](../../../internal/journal/reader.go)
is a read-only journal view and [`internal/telemetry/rollup/query.go`](../../../internal/telemetry/rollup/query.go)
answers stats — but nothing serves them over an API, and the portal is still a **mock
client** (`portal/src/App.tsx`). The real risk is **GT-003**: the human-gate evaluator is
a V0 stub that errors (`internal/gate/evaluate.go:118` — "human evaluator is not supported
at V0, ships V1"). Shipping it means the **runner must durably pause a run at a human gate
and resume on an external approval signal** — a lifecycle change, not just a UI.

The doctrine (portal.md): the portal is **optional** at tiers 1–2 (PORT-022); approvals
MUST have a **non-portal path** (PORT-023) so the portal is never a hard dependency. So the
approval flow is built **API-first**, with the portal panel and `goobers approve` as two
front-ends over the same daemon endpoint.

## 2. Scope boundary

**Historical scope (V1, tiers 1–2):** human-gate evaluator with durable
pause/resume; a thin read-only daemon API; CLI approval paths; portal retarget;
and an access-control seam. These concerns are now split across #440 and #462 so
each can ship independently.

**Out of scope — V2 / WON'T-v1:** runner-specific operational views (PORT-025, WON'T-v1);
OIDC *implementation* (that's **#38** — this epic ships only the auth seam + tier-1 no-auth
default); any config/setup surface (PORT-010 — config is git-only).

## 3. Architecture

```
run hits a `human` gate
  → runner records a durable "awaiting-approval" journal event, pauses the run
  → approval arrives via EITHER surface, both hitting the same daemon endpoint:
        portal approval panel  ─┐
        goobers approve <run> <gate> ─┴─→ POST /approvals  (the one write path)
  → daemon resumes the run (Runner.Resume, #17) → gate branch (pass / needs-changes / reject)

daemon read API (served by `goobers up`, local port):
  GET /roster       ← config definitions
  GET /runs, /runs/{id}/trace  ← journal reader (PORT-020/024)
  GET /stats        ← telemetry rollup
  GET /approvals    ← pending human gates
```

## 4. Missions (dispatchable, single-PR-sized)

### P0 — Human-gate evaluator + durable pause/resume (GT-003) — load-bearing
- Implement `EvaluatorHuman`: on reaching a `human` gate the runner writes a durable
  awaiting-approval journal event and suspends the run; an approval/rejection signal
  resumes it to the gate's branch (`pass`/`needs-changes`/`reject`). Reuses `Runner.Resume`
  (#17). Fail-closed: an unknown/expired approval never auto-passes.
- *(2026-07-13 review notes: the pause half already exists — `internal/runner/run.go:311-321`
  checkpoints and halts at a human gate; the gap is the approval signal → resume →
  verdict-branch mapping. P0 inherits the resume-context fixes (#107/#108) and MUST
  agree with #135's ruling on whether paused runs hold max-parallel slots — today a
  paused run starves its workflow after any daemon restart.)*
- **Seams:** `internal/gate/evaluate.go`, `internal/runner` (pause/resume), `internal/journal`.
- **Test plan:** run pauses at a human gate (journal event present); approve → resumes to
  `pass` branch; reject → `reject`/`@abort` branch; crash between pause and approve →
  resume still awaits (no auto-advance); replay is deterministic.

### P1 — Daemon read-only API
- Thin HTTP served by `goobers up`: `/roster` (config), `/runs` + `/runs/{id}/trace`
  (journal reader), `/stats` (rollup), `/approvals` (pending) + `POST /approvals` (the one
  write path from P0). JSON; read-only except the approval submit.
- **Seams:** `cmd/goobers/up.go` (`daemon.go`), `internal/journal/reader.go`, `internal/telemetry/rollup`.
- **Test plan:** each endpoint returns journal/rollup-backed data over a fixture instance;
  no endpoint mutates state except `POST /approvals`; API is inert when no instance is up.

### P2 — CLI approval surface (PORT-023, the non-portal path)
- `goobers approve <run-id> <gate>` and `goobers reject ...`, plus `goobers approvals`
  (pending list) — all over P1's `POST/GET /approvals`, so tier-1 is fully operable without
  the portal. Depends on **P0/P1**.
- **Seams:** `cmd/goobers`, P1 API.
- **Test plan:** `approvals` lists a paused run; `approve` resumes it; `reject` aborts;
  matches the portal panel's behavior over the same endpoint.

### P3 — Portal retarget to the real API
- Replace the mock client: roster view, run list + trace viewer, human-gate approval panel,
  all wired to P1. Portal stays **optional** (PORT-022); the browser reads the daemon
  API, whose shared read service projects the journal (PORT-024). No config/setup
  surface. Depends on **P1** (and P0 for the panel).
- **Seams:** `portal/src/*`, P1 API.
- **Test plan:** component/integration tests against a fake P1 API — run list renders from
  `/runs`, trace from `/runs/{id}/trace`, approval panel submits to `POST /approvals`;
  portal-absent path still works (covered by P2).

### P4 — Access-control seam (none at tier 1)
- An `Authorizer` seam gating interactive actions (approve/intervene) per PORT-013: **none
  at tier 1** (default open, documented), pluggable so **#38** drops OIDC in at tier 2
  without caller changes.
- **Seams:** daemon API middleware, shared seam with #38.
- **Test plan:** tier-1 default allows approval with no auth; the seam rejects when a
  (fake) authorizer denies — proving #38 can plug in.

## 5. End-to-end / integration test

A run reaches a human gate through the **real local runner + fake harness**, pauses
(journal event), is approved via **both** `goobers approve` and a simulated `POST
/approvals`, and resumes to the correct branch; the read API serves the run list + trace
for that run. Journal-only assertions, no network.

## 6. Dependencies

- **P0 gates** the approval flow (P2's approve, P3's panel).
- **#38** (auth) plugs into P4's seam for tier-2; not required for tier-1.
- Reuses `Runner.Resume` (#17, merged) and the journal reader / rollup query surfaces.

## 7. Open questions (for PM / PO)

- **OQ-1 — pause state location:** record awaiting-approval in the **run journal**
  (recommend — durable, consistent with `Runner.Resume`) vs. a side store? *(Recommend: journal.)*
- **OQ-2 — tier-1 approval authz:** none at tier 1 (anyone local may approve) per PORT-013 —
  confirm that's acceptable for the dogfood/self-host case. *(Recommend: yes; #38 adds authz at tier 2.)*
- **OQ-3 — reject semantics:** a human reject maps to the gate's `reject`/`@abort` branch;
  "request changes" maps to `needs-changes`. Confirm both verbs are exposed in CLI + portal.
- **OQ-4 — API transport/port:** local JSON/HTTP on a configurable loopback port served by
  `goobers up`. *(Recommend: yes, loopback-only by default.)*
