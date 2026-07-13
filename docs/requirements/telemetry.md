# Spec: Telemetry & Tracing

> Status: **Draft** · Aligned to ../ARCHITECTURE.md (2026-07-12) · Derives from ../VISION.md §4, §5 · Area prefix: `TEL`

## Purpose

Telemetry is the data backbone: every unit of work is a **trace**, automatically
captured, queryable at every tier. It powers portal observability, feeds the
**work-nomination workflow**, and is the fuel for the **Tutor**'s learning loop.
Instrumentation is OpenTelemetry throughout; **only the exporter changes per tier**
(`ARCHITECTURE.md §8`).

## Model

- **Two separate stores** (never conflated — unchanged doctrine, every tier):
  - **Goober-run telemetry** — owned by the instance. Holds the gaggle's operational
    data: traces, per-stage success/duration, within-stage harness events, step logs,
    agent actions, tool/script outputs, gate outcomes, scheduler decisions. *This spec
    governs it.* Tiers 1–2: spans in the run journal + a **SQLite rollup**
    (`telemetry.db`), queryable via CLI. **Tier 3 (V2):** OTLP → **Azure Data Explorer**.
  - **Project telemetry** — the team's *own* product observability (optional, external,
    e.g. project ADX or whatever the team already runs). **Producer goobers read *from*
    it**; it is read-only input, never part of our store.
- **Every workflow run is a trace.** Tasks, gates, and scheduler decisions are
  spans/events within it; per-stage spans live in the run journal's `spans/` directory
  (`ARCHITECTURE.md §4`).
- **Collection is automatic** — management tools injected via MCP/hooks, plus machine log
  collection — not dependent on a goober opting in.
- **Redaction happens at ingest**, before anything lands at rest — unchanged at every
  tier.

## Requirements

### Store

- **TEL-001 (MUST):** *(All tiers)* The instance MUST provision a goober-run telemetry
  store, **separate from** any project telemetry store (`VISION §5`). Tiers 1–2: run
  journal spans + the SQLite rollup (`telemetry.db`). **Tier 3 (V2):** ADX.
- **TEL-002 (MUST):** *(All tiers)* The store MUST be queryable — via the CLI over the
  local rollup at tiers 1–2 (this feeds the work-nomination workflow, and the Tutor
  mines it); at monorepo scale via ADX queries at tier 3.

### Capture

- **TEL-010 (MUST):** *(All tiers)* Every workflow run MUST be recorded as an
  **OpenTelemetry trace**; tasks, gates, and scheduler decisions MUST appear as spans
  (with standard OTel attributes: gaggle, workflow, goober, task type, item id, outcome)
  and be exported to the tier's run store — journal `spans/` + SQLite rollup at tiers
  1–2; **Tier 3 (V2):** OTLP → ADX.
- **TEL-011 (MUST):** *(All tiers)* Capture MUST be automatic via injected management
  tools/hooks + machine log collection (`GBO-021`).
- **TEL-013 (MUST):** *(All tiers)* Secrets/PII MUST be **redacted at ingest** by the
  collection hooks/stage runners, before events or spans are written; raw secrets MUST
  NOT land at rest (`SEC-041`). Retention MUST be a configurable window per instance.
- **TEL-012 (MUST):** *(All tiers)* Captured data MUST be rich per stage: stage
  success/outcome, start/stop timing and duration, per-step logs, agent actions,
  **within-stage harness events/traces**, tool/script outputs, gate outcomes
  (+rationale where available), and scheduler decisions/claims/releases.

### Instrumentation & local store mechanics

- **TEL-030 (MUST):** *(All tiers)* Instrumentation MUST be OpenTelemetry throughout the
  codebase; **only the exporter changes per tier**. Instrumented code paths are
  identical whether exporting to the journal/SQLite or over OTLP to ADX.
- **TEL-031 (MUST):** *(Tiers 1–2)* The local store MUST be queryable via the CLI
  (`goobers status`, `goobers trace <run-id>`, and rollup queries) with no external
  service — good enough for the work-nomination workflow to select and rank candidate
  work.
- **TEL-032 (MUST):** *(Tiers 1–2)* Per-stage spans MUST be persisted in the run
  journal's `spans/` directory and rolled up into `telemetry.db`; the rollup MUST be
  rebuildable from the journals (journals are the source of truth, the rollup a
  projection).

### Consumption

- **TEL-020 (MUST):** *(All tiers)* The portal MUST be able to read telemetry for
  observability — run journals + rollup at tiers 1–2, ADX at tier 3.
- **TEL-021 (MUST):** *(All tiers)* The Tutor MUST be able to query the goober-run store
  to detect patterns across runs (Tutor spec).
- **TEL-022 (SHOULD):** *(All tiers)* Producer goobers MAY read from external **project**
  telemetry as input (e.g. the nomination workflow's configured sources); this MUST stay
  distinct from the goober-run store.

## Relationships

- Provisioned by → the **Instance** (`goobers init` creates `telemetry.db`; Bicep
  provisions ADX at tier 3).
- Written by → **Workflows/Tasks/Gates/Scheduler** (every run; spans land in the run
  journal).
- Read by → the **Portal** (observability), the **work-nomination workflow** (candidate
  selection), and the **Tutor** (learning).

## Open questions

- **TEL-Q1:** **Resolved:** retention is a **configurable window per instance**.
  *(Build-time: defaults + cost controls; local journal/rollup pruning at tiers 1–2.)*
- **TEL-Q2:** **Resolved:** secrets/PII are **redacted at ingest** (via the management
  collection hooks/stage runners). *(Build-time: redaction ruleset.)*
- **TEL-Q3:** ~~Common trace/span schema~~ **Resolved (updated 2026-07-12):**
  OpenTelemetry-aligned (run=trace, task·gate·scheduler=span); the **exporter is
  per-tier** — journal spans + SQLite rollup at tiers 1–2, OTLP → ADX as the tier-3
  drop-in (`ARCHITECTURE.md §8`). See `TEL-010`, `TEL-030`. *(Remaining: finalize the
  exact attribute set.)*
- **TEL-Q4:** **Resolved (default):** the goober-run store is **partitioned per gaggle**,
  aligning with gaggle isolation (`SEC-003`) — attribute-scoped locally, partitioned in
  ADX.
- **TEL-Q5:** *(build-time design)* SQLite rollup schema and journal→rollup rebuild
  mechanics (`TEL-032`).
