# Spec: Config-as-Code Model

> Status: **Draft** · Aligned to ../ARCHITECTURE.md (2026-07-12) · Derives from ../VISION.md §3, §4, §5 · Area prefix: CFG

## Purpose

Everything about a Goobers deployment — instances, gaggles, goobers, workflows, gates,
connections — is **defined and managed as code**. This is the control plane; the portal
is only a window. This spec defines the model and conventions for that code, and how it
is delivered to a running instance at each deployment tier.

## Model

- Config is split by cadence and blast radius (`ARCHITECTURE.md §6`, `VISION.md §5`):
  - **Instance provisioning** *(singleton)* — at tiers 1–2 this collapses to the
    binary + `instance.yaml` written by `goobers init` (connections: target repos,
    provider, token refs, telemetry settings). **Tier 3 (V2):** a real **`infra`
    repo** — Bicep / cluster bootstrap / connection wiring, with strict review.
    Rarely changes either way.
  - **`config` repo/directory** — the workforce as code: **goober/workflow/gate
    definitions** (markdown + folders) + a **YAML manifest** declaring desired state
    (Helm-like: counts + characteristics). Changes constantly; **the only place the
    Tutor writes**.
  - (Platform/engine code is upstream, consumed as a released binary / images /
    charts — not a per-instance repo.)
- **Config delivery by tier — same schemas everywhere:**
  - *(Tiers 1–2)*: a **local config directory** loaded, validated, and **watched by
    the local runner daemon**; edits (usually landing via git + PRs) take effect on
    reload with no redeploy machinery.
  - **Tier 3 (V2):** **ArgoCD sync → CRDs → the Goobers operator** — the cloud
    drop-in for the config-delivery seam (`ARCHITECTURE.md §10`). The definition
    schemas are identical; only delivery changes.
- A **reconcile drives desired state** into the running instance. Changing a scale
  factor yields more replicas; adding a goober definition makes a new team member
  appear — whether the reconciler is the watching daemon or the operator.
- A Goober may select an optional, harness-scoped `spec.model` and provide
  string-valued `spec.harnessOptions`. The platform preserves the options as an
  opaque map; the selected harness adapter validates the model and every option
  before the definition can run. Unknown values fail closed during validation.
  The Copilot adapter accepts `auto` or one of the model identifiers listed in
  the Goober schema, plus `context` (`default`/`long_context`) and
  `reasoningEffort` (`none` through `max`) harness options.
- **Setup and configuration are declarative and code-only.** No imperative/portal
  configuration. (Runtime *operations* — gate approvals, retries — are a separate,
  minimal portal surface; see Portal spec.)
- Changes flow through **git + PRs** — including the **Tutor**'s training PRs against
  the `config` repo/directory.

## Requirements

- **CFG-001 (MUST):** All configuration MUST live as code: provisioning in
  `instance.yaml` (tiers 1–2) or the `infra` repo (**Tier 3 (V2)**); gaggle/goober/
  workflow/gate definitions + manifest in the `config` repo/directory. *(All tiers)*
- **CFG-010 (MUST):** Provisioning and `config` MUST be separable by access control,
  so the Tutor can be granted write to `config` only (structural containment — see
  `TUT-005`, `SEC-021`). The boundary holds at every tier: capability-scoped write
  grants locally, hardened to a true permission boundary when `config` is backed by
  its own reviewed git remote; repo + identity permissions in the cloud.
- **CFG-002 (MUST):** A YAML manifest MUST declare desired state — gaggles, goobers
  (with scale factors), workflows (triggers, tasks, gates, selectors), and
  connections. *(All tiers — same schema.)*
- **CFG-003 (MUST):** Goober instructions/personas MUST be authored as markdown; skills
  and tools are referenced by the definition.
- **CFG-004 (MUST):** The platform MUST reconcile declared desired state into the
  running instance (idempotent apply) — the watching daemon at tiers 1–2; ArgoCD +
  operator at tier 3.
- **CFG-005 (MUST):** Setup/config MUST be declarative; the platform MUST NOT require
  (or offer) UI-based configuration.
- **CFG-006 (MUST):** Changes MUST be versioned via git; PRs are the change mechanism,
  including Tutor-authored `config` changes.
- **CFG-007 (SHOULD):** The config directory (and the tier-3 `infra` repo) SHOULD
  follow a documented reference folder layout so definitions are discoverable and
  composable (scaffolded by `goobers init` locally; layout details `CFG-Q2`).
- **CFG-008 (SHOULD):** Manifests SHOULD be validatable/lintable before apply (catch
  bad definitions early) — surfaced locally as `goobers validate`.
- **CFG-009 (MUST):** Secrets MUST be referenced (not stored) in config: env vars /
  token file refs at tiers 1–2; **Tier 3 (V2):** Key Vault references. One
  secret-resolver seam, per-tier implementations (see Security).

### Delivery by tier
- **CFG-020 (MUST):** *(Tiers 1–2)*: config delivery MUST be a **local config
  directory** that the local runner daemon loads, validates, and watches — no
  database, cluster, or sync service required. Valid changes take effect on reload;
  in-flight runs stay pinned to their started definition version (`WF-016`). This is
  the owning statement of tiers-1–2 config delivery (`DEP-025` defers here).
- **CFG-021 (MUST):** **Tier 3 (V2):** config delivery MUST be **ArgoCD sync → CRDs →
  the Goobers operator** — the cloud drop-in for the same config-delivery seam
  (`ARCHITECTURE.md §10`). No definition changes shape when an instance moves tiers.
  Owning requirement: `DEP-012`; this ID defers to it.
- **CFG-022 (MUST):** Definition schemas MUST be identical at every tier: a config
  directory valid at tier 1 MUST be valid, unchanged, as tier-3 config content. Tier
  only selects the delivery mechanism, never the schema. DSL features implemented
  only by the Temporal runner (parallel branches, child workflows —
  `ARCHITECTURE.md §3.2`) are **tier-3 (V2) schema extensions**: a definition using
  them is valid only at tier 3 and sits outside the cross-runner conformance surface
  until the local runner gains them. *(All tiers)*
- **CFG-023 (MUST):** Validation MUST fail closed: definitions that do not validate
  MUST NOT be loaded or trigger runs — the instance keeps the last-known-good config
  and surfaces the error (CLI/portal), rather than degrading. *(All tiers)*

## Relationships

- Defines → **Instance**, **Gaggle**, **Goober**, **Workflow**, **Task**, **Gate**.
- Applied by → the **local runner daemon** (tiers 1–2) / **ArgoCD + operator**
  (tier 3, V2).
- Scaffolded/validated by → `goobers init` / `goobers validate` (tiers 1–2).
- Edited by → humans and the **Tutor** (via PRs).

## Open questions

- **CFG-Q1:** *(build-time design)* Concrete manifest schema for each primitive (the
  actual YAML shape; the tier-3 CRDs wrap the same schema).
- **CFG-Q2:** *(build-time design)* Reference folder layout for the config directory
  (and the tier-3 `infra` repo).
- **CFG-Q3:** *(build-time design)* Definition composition/reuse mechanism (shared
  fragments — `GBO-004`, `WF-005`).
- **CFG-Q4:** ~~Secrets in config~~ **Resolved (updated):** secrets are **references,
  never stored** (`CFG-009`) — env/file refs at tiers 1–2, **Key Vault references**
  as the tier-3 drop-in (`SEC-010`, `ARCHITECTURE.md §9–§10`).
- **CFG-Q5:** *(build-time design)* Version compatibility between the upstream
  platform release and the config definitions (the daemon checks compatibility at
  tiers 1–2; the operator does at tier 3).
