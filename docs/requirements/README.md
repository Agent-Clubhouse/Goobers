# Goobers — Requirements & Specs

This directory turns the product vision (`../VISION.md`) into structured, traceable
requirements that decompose into build work. The **architecture of record**
is [`../ARCHITECTURE.md`](../ARCHITECTURE.md) — one system across three deployment
tiers, two runners behind one seam — and specs carry tier annotations aligned to it
(`ARCHITECTURE.md §13`). Where an older spec passage contradicts it, the architecture
doc wins and the spec is updated to match.

## How these specs are organized

One spec per **primitive** plus one per **cross-cutting system**. Each spec is
self-contained: purpose, model, requirements, and open questions.

## Conventions

- **Requirement IDs** are stable and area-prefixed: `<AREA>-NNN` (e.g. `GBO-001` for a
  Goober requirement). IDs never get reused or renumbered — they're referenceable from
  build items, tests, and other specs.
- **One owning ID per contract:** where the same contract appears in more than one
  spec, exactly one requirement owns it and the others say so explicitly ("Owning
  requirement: `X-NNN`; this ID defers to it"). Deferring IDs stay stable and citable;
  only the owner's text is normative.
- **Priority** uses MoSCoW: **MUST**, **SHOULD**, **COULD**, **WON'T (v1)** — where
  "(v1)" means explicitly out of scope for the V0/V1 milestones.
- **Status** per spec: `Draft` → `Reviewed` (maintainer-reviewed) → `Approved`
  (locked for build).
- **Traceability:** each spec links back to the vision section(s) it derives from.
- **Tier applicability** is annotated inline where useful — italic applicability
  suffixes (*(All tiers)*, *(Tiers 1–2)*) and the bold prefix **Tier 3 (V2):** for
  tier-3-only requirements. Tier-3-only requirements (Azure/cluster substrate) are
  marked, never deleted: they are the drop-in specs for V2.
- **Open questions** live in each spec; their overall disposition is summarized in
  `../VISION.md §8`.

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
| System | [Telemetry & tracing](telemetry.md) | Draft | Two separate stores; journal+rollup → ADX at tier 3 |
| System | [Tutor & learning loop](tutor.md) | Draft | Writes only to `config`; reads journals; ships narrow-slice, forward scope per `../design/tutor-redesign.md` |
| System | [Portal](portal.md) | Approved for staged implementation | Observability-first; reads run journals; minimal runtime ops |
| System | [Deployment & infra](deployment.md) | Draft | Tiered: local install → AKS/Bicep drop-in (V2) |
| System | [Security & isolation](security.md) | Draft | Auth/isolation ladder; Key Vault refs at tier 3 |
| System | [Config-as-code model](config-as-code.md) | Draft | Manifest + markdown + folder layout |

## Source decisions

Foundational decisions are recorded in `../VISION.md §8`; the architectural ones are
elaborated in `../ARCHITECTURE.md`. Specs must not contradict them; if a spec surfaces
a reason to revisit one, flag it rather than diverging.
