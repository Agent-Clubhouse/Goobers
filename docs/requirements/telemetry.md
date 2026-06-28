# Spec: Telemetry & Tracing

> Status: **Draft** · Derives from `../VISION.md` §4, §5 · Area prefix: `TEL`

## Purpose

Telemetry is the data backbone: every unit of work is a **trace**, automatically captured,
queryable at scale. It powers portal observability and is the fuel for the **Tutor**'s
learning loop.

## Model

- **Two separate stores** (never conflated):
  - **Goober-run telemetry** — owned by the instance (ADX the lead choice). Holds the
    gaggle's operational data: traces, step logs, agent actions, tool/script outputs,
    gate outcomes, scheduler decisions. *This spec governs it.*
  - **Project telemetry** — the team's *own* product observability (optional, external,
    e.g. project ADX). **Producer goobers read *from* it**; it is read-only input, not
    part of our store.
- **Every workflow run is a trace.** Tasks, gates, and scheduler decisions are
  spans/events within it.
- **Collection is automatic** — management tools injected via MCP/hooks, plus machine log
  collection — not dependent on a goober opting in.

## Requirements

### Store
- **TEL-001 (MUST):** The instance MUST provision a goober-run telemetry store, **separate
  from** any project telemetry store (`VISION §5`).
- **TEL-002 (MUST):** The store MUST be queryable at scale (the Tutor mines it).

### Capture
- **TEL-010 (MUST):** Every workflow run MUST be recorded as an **OpenTelemetry trace**;
  tasks, gates, and scheduler decisions MUST appear as spans (with standard OTel
  attributes: gaggle, workflow, goober, task type, item id, outcome) and be exported to
  ADX.
- **TEL-011 (MUST):** Capture MUST be automatic via injected management tools/hooks +
  machine log collection (`GBO-021`).
- **TEL-013 (MUST):** Secrets/PII MUST be **redacted at ingest** by the collection hooks;
  raw secrets MUST NOT land at rest. Retention MUST be a configurable window per instance.
- **TEL-012 (MUST):** Captured data MUST include: start/stop timing, per-step logs, agent
  actions, tool/script outputs, gate outcomes (+rationale where available), and scheduler
  decisions/claims/releases.

### Consumption
- **TEL-020 (MUST):** The portal MUST be able to read telemetry for observability.
- **TEL-021 (MUST):** The Tutor MUST be able to query telemetry to detect patterns across
  runs (Tutor spec).
- **TEL-022 (SHOULD):** Producer goobers MAY read from external **project** telemetry as
  input; this MUST stay distinct from the goober-run store.

## Relationships

- Provisioned by → the **Instance**.
- Written by → **Workflows/Tasks/Gates/Scheduler** (every run).
- Read by → the **Portal** (observability) and the **Tutor** (learning).

## Open questions

- **TEL-Q1:** **Resolved:** retention is a **configurable window per instance**.
  *(Build-time: defaults + cost controls.)*
- **TEL-Q2:** **Resolved:** secrets/PII are **redacted at ingest** (via the management
  collection hooks). *(Build-time: redaction ruleset.)*
- **TEL-Q3:** ~~Common trace/span schema~~ **Resolved:** OpenTelemetry-aligned (run=trace,
  task·gate·scheduler=span) exported to ADX. See `TEL-010`. *(Remaining: finalize the exact
  attribute set.)*
- **TEL-Q4:** **Resolved (default):** the goober-run store is **partitioned per gaggle**,
  aligning with gaggle isolation (`SEC-003`).
