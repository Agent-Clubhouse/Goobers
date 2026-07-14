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

### Standards & capture depth (V1 — design: `../design/v1/observability-substrate.md`)

- **TEL-040 (MUST):** *(All tiers)* Spans MUST carry the **canonical attribute
  registry** (design D1): standard OTel semconv names where they exist, `goobers.*`
  otherwise; agentic stage spans MUST follow the **OTel GenAI semantic conventions**
  (`gen_ai.*`) (design D2). The registry is code + drift-guarded by test.
- **TEL-041 (MUST):** *(All tiers)* Agentic stages MUST report usage the harness
  exposes — `gen_ai.usage.input_tokens`/`output_tokens`,
  `goobers.usage.copilot_premium_requests`, `goobers.usage.cost_usd` — in
  `ResultEnvelope.Metrics` and on the stage span. Missing data is *absent*, never
  zero. Once usage is reported, the runner MUST enforce declared
  `Limits.MaxTokens`/`MaxCostUSD` (fail with `budget-exceeded`, branch per policy).
- **TEL-042 (MUST):** *(All tiers)* Every agentic stage attempt MUST capture at
  minimum the **composed invocation prompt and final output** as a scrubbed,
  digested **transcript artifact** (`transcript.jsonl`, GenAI-events shape);
  harness-native full transcripts SHOULD be converted in when available (design D4).
  Instructed agent self-logging MAY supplement but never substitutes.
- **TEL-043 (MUST):** *(Tiers 1–2)* Stages MUST have a zero-dependency custom
  metric/log emission surface (`GOOBERS_TELEMETRY_DIR`: `metrics.jsonl`,
  `events.jsonl`) ingested at stage completion (scrub → journal → rollup); malformed
  telemetry MUST NOT fail a run (design D5).
- **TEL-044 (MUST):** *(Tiers 1–2)* Run spans MUST additionally be exported as
  **OTLP/JSON files** in the journal, re-emittable from journals
  (`goobers telemetry export`); an **opt-in OTLP push** endpoint MAY be configured
  per instance. Standard OTel tooling consumes both. Cloud collection is the tier-3
  drop-in of the same exporter (design D3).
- **TEL-045 (MUST):** *(All tiers)* The query surface MUST be reachable from inside
  workflows as a built-in **connector stage** (the `backlog-query` pattern) emitting
  a **versioned candidate-findings artifact** with journal evidence pointers
  (design D6) — the deterministic half of the Tutor and a general workflow primitive.

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
