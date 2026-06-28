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
- **TEL-010 (MUST):** Every workflow run MUST be recorded as a trace; tasks, gates, and
  scheduler decisions MUST appear as spans/events within it.
- **TEL-011 (MUST):** Capture MUST be automatic via injected management tools/hooks +
  machine log collection (`GBO-021`).
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

- **TEL-Q1:** Retention policy and storage cost controls.
- **TEL-Q2:** PII / secret redaction in logs and captured outputs.
- **TEL-Q3:** Common trace/span schema so the Tutor and portal can rely on a stable shape.
- **TEL-Q4:** Per-gaggle scoping/partitioning within the shared store (coordinate with
  Security + Gaggle isolation).
