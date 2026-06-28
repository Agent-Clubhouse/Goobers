# Spec: Portal

> Status: **Draft** · Derives from `../VISION.md` §4 + portal-scope decision · Area prefix: `PORT`

## Purpose

The **Portal** is the web window into a Goobers instance. It is **observability-first**,
with a deliberately **minimal interactive surface** for *runtime operations only*. It is
**not** a configuration tool — config is code.

## Model

The dividing line is **config-time vs. runtime**:

- **Config-time → code only.** Creating/changing gaggles, goobers, workflows, and gates
  happens in the `config` repo; infra/connections in the `infra` repo. The portal never
  configures these.
- **Runtime ops → minimal portal interactivity.** A small set of operational actions that
  can't be (or are awkward to) express declaratively: **human-gate approvals** and
  **run-level intervention** (e.g. retry/abort). Kept minimal by design.

Primary value is observability: the live state of the gaggle and the traces behind every
unit of work.

## Requirements

### Observability (primary)
- **PORT-001 (MUST):** On first boot the portal MUST show an "I'm alive / ready" state
  with no gaggles/goobers configured.
- **PORT-002 (MUST):** The portal MUST show the current workforce — gaggles, goober
  definitions ("team members"), workflows — and live run status.
- **PORT-003 (MUST):** The portal MUST surface traces/telemetry for runs (observability
  into what happened and why) from the goober-run telemetry store.

### Interactivity (minimal, runtime only)
- **PORT-010 (MUST):** The portal MUST NOT be used for setup/configuration; all config is
  declarative code (`CFG-005`).
- **PORT-011 (MUST):** The portal MUST provide a surface for **human-gate approvals**
  (resolving `GT-Q2`), letting an authorized human approve/reject a paused gate.
- **PORT-012 (SHOULD):** The portal SHOULD provide minimal **run-level operational
  actions** (e.g. retry, abort/cancel, intervene) — scoped to operations, not config.
- **PORT-013 (SHOULD):** Interactive actions SHOULD be access-controlled (who may approve
  gates / intervene) — coordinate with Security.

## Relationships

- Reads → the **Telemetry** store (observability) and live system state.
- Hosts → **human-gate** approvals (`GT-012`) and run interventions.
- Surfaces → the **Instance** / **Gaggle** / **Goober** / **Workflow** state.
- Never configures → anything (that's **Config-as-code**).

## Open questions

- **PORT-Q1:** Are human-gate approvals portal-only, or also deliverable via PR review /
  chat / notification? (Multiple channels?)
- **PORT-Q2:** Exact set of runtime operational actions in v1 (just approvals + retry, or
  more?).
- **PORT-Q3:** AuthN/AuthZ model for the portal and its actions (Security spec).
