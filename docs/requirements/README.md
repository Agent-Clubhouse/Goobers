# Goobers — Requirements & Specs

This directory turns the product vision (`../VISION.md`) into structured, traceable
requirements that the PM can decompose into build work.

## How these specs are organized

One spec per **primitive** plus one per **cross-cutting system**. Each spec is
self-contained: purpose, model, requirements, and open questions.

## Conventions

- **Requirement IDs** are stable and area-prefixed: `<AREA>-NNN` (e.g. `GBO-001` for a
  Goober requirement). IDs never get reused or renumbered — they're referenceable from
  build items, tests, and other specs.
- **Priority** uses MoSCoW for **v1**: **MUST**, **SHOULD**, **COULD**, **WON'T (v1)**.
- **Status** per spec: `Draft` → `Reviewed` (PO red-lined) → `Approved` (locked for
  build).
- **Traceability:** each spec links back to the vision section(s) it derives from.
- **Open questions** live in each spec and are mirrored in `../VISION.md §8` until
  resolved.

## Spec index (also our spec backlog)

| Area | Spec | Status | Notes |
|---|---|---|---|
| Primitive | [Goober](goober.md) | Draft | Template spec |
| Primitive | [Instance / Tenant](instance.md) | Draft | |
| Primitive | [Gaggle](gaggle.md) | Draft | |
| Primitive | [Workflow](workflow.md) | Draft | Couples tightly with Scheduler |
| Primitive | [Task](task.md) | Draft | |
| Primitive | [Gate](gate.md) | Draft | Unified gate, pluggable evaluator |
| System | [Scheduler & work distribution](scheduler.md) | Draft | Routing + work-claiming |
| System | [Backlog & providers (GitHub/ADO)](backlog-providers.md) | Draft | Provider abstraction |
| System | [Telemetry & tracing](telemetry.md) | Draft | Two separate stores |
| System | [Tutor & learning loop](tutor.md) | Draft | Modifies goobers + workflows/gates; approval configurable |
| System | [Portal](portal.md) | Draft | Observability-first; minimal runtime ops |
| System | [Deployment & infra](deployment.md) | Draft | AKS, Bicep, release pipeline |
| System | [Security & isolation](security.md) | Draft | Namespace + identity per gaggle; Key Vault refs |
| System | [Config-as-code model](config-as-code.md) | Draft | Manifest + markdown + folder layout |

## Source decisions

Foundational decisions are recorded in `../VISION.md §8`. Specs must not contradict
them; if a spec surfaces a reason to revisit one, flag it rather than diverging.
