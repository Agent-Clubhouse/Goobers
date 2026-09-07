# Spec: Instance / Tenant

> Status: **Draft** · Aligned to `../ARCHITECTURE.md` (2026-07-12) · Derives from
> `../VISION.md` §3, §4, §5 · Area prefix: `INST`

## Purpose

An **Instance** (Tenant) is a deployed Goobers installation — the top-level boundary that
owns shared runtime state and hosts one or more gaggles. It is what a user or team stands
up, owns, and operates — at any of the three deployment tiers, without a product fork.

## Model

- An instance is **self-hosted, not a managed service**, and its shape follows the tier:
  - **Tiers 1–2 (local runner, ships first):** one Go binary (`goobers`), no database,
    no message bus, no service cluster. `goobers init` scaffolds the instance root;
    durable state is plain, inspectable files:

    ```
    <instance-root>/
      instance.yaml     # connections: target repo(s), provider (GitHub/ADO),
                        # token refs, telemetry settings
      config/           # the config repo/directory: gaggles, goobers, workflows,
                        # gates, instruction markdown (the ONLY Tutor-writable path)
      gaggles/<gaggle>/
        runs/           # run journals (append-only events, snapshots, artifacts)
        workcopies/     # managed working copies and per-run worktrees
      scheduler/        # instance journal: scheduler decisions + claim ledger

      ## Cost report publication

      Successful shipped workflows publish provider-native sticky cost reports by
      default. Configure the instance default with:

      ```yaml
      cost:
        enabled: false
      ```

      A gaggle may override that default with `spec.cost.enabled`. Resolution is
      explicitly most-specific-first: a non-nil gaggle value wins, otherwise the
      instance value wins, otherwise reporting is enabled. Omitting `enabled` is
      therefore not the same as setting it to `false`. journal: scheduler decisions + claim ledger
      telemetry.db      # local telemetry rollup store
    ```

  - **Tier 3 (Temporal runner, V2):** bootstrapped from a real **`infra` repo**
    (Bicep + GitOps) via a release pipeline the team configures with their connection
    info; workflows run on self-hosted Temporal, stages in Kubernetes agent pods.
- It is **bring-your-own**: the user supplies the machine or cloud it runs on, the
  agent harness subscription (GitHub Copilot first), project repo(s) (GitHub or ADO),
  and a backlog.
- It hosts **many gaggles** (siloed workforces).
- The Goobers side of an instance splits **provisioning from `config`** (see
  Config-as-code spec). The `config` repo/directory is always a singleton and the only
  thing the Tutor may write; provisioning collapses to the binary + `instance.yaml` at
  tiers 1–2 and is the `infra` repo at tier 3. The platform/engine code is upstream
  (consumed as a released binary / images / charts), not a per-instance repo.

## Requirements

- **INST-001 (MUST):** **Tier 3 (V2):** at cloud scale an instance MUST be deployable
  from the `infra` repo via a release pipeline configured with the team's connection
  info (no managed service) — the cloud drop-in for the provisioning seam that
  `goobers init` implements locally (`INST-010`, `ARCHITECTURE.md §10`). *(Tier 3, V2)*
- **INST-002 (MUST):** On first boot the instance MUST report an "I'm alive" / ready
  state with no gaggles/goobers configured yet — via `goobers status` at tier 1 (V0)
  and via the portal once it reads run journals (V1+). *(All tiers)*
- **INST-003 (MUST):** **Tier 3 (V2):** at cloud scale an instance MUST provision/own
  shared infra: an AKS cluster, storage + logging, and the goober-run telemetry store
  (ADX) — the cloud drop-ins for the runtime-state seam the instance root implements
  locally (`INST-010`). *(Tier 3, V2)*
- **INST-004 (MUST):** An instance MUST host one or more gaggles. *(All tiers)*
- **INST-005 (MUST):** An instance MUST integrate bring-your-own subscriptions and
  resources: the machine or cloud it runs on (Azure at tier 3), the agent harness
  (GitHub Copilot first), project repo(s), and a backlog. *(All tiers)*
- **INST-006 (MUST):** All instance configuration MUST be code (no UI configuration);
  see Config-as-code spec. *(All tiers)*
- **INST-007 (MUST):** The `config` repo/directory MUST be a singleton per instance,
  and so MUST the provisioning source — the binary + `instance.yaml` at tiers 1–2,
  the `infra` repo at tier 3. *(All tiers)*
- **INST-008 (SHOULD):** An instance SHOULD be reproducible/redeployable from its
  provisioning source plus `config` alone (idempotent apply of desired state) —
  `goobers init` + `instance.yaml` at tiers 1–2; the `infra` repo at tier 3.
  *(All tiers)*
- **INST-010 (MUST):** At tiers 1–2, `goobers init` MUST scaffold the instance root:
  `instance.yaml` (connections: target repos, provider, token refs, telemetry
  settings), `config/`, `gaggles/`, `scheduler/` (the instance journal), and the local
  telemetry rollup store. Per-gaggle `runs/` and `workcopies/` are created under
  `gaggles/<gaggle>/`. This is the owning statement of the
  local scaffold (`DEP-021` defers here). *(Tiers 1–2)*
- **INST-011 (MUST):** At tiers 1–2 an instance MUST run as a single binary with no
  database, message-bus, or service-cluster dependency; all durable state MUST be
  plain files a human can inspect with standard tools. *(Tiers 1–2)*
- **INST-012 (MUST):** The instance MUST provide lifecycle and inspection commands:
  `goobers validate` (check definitions + instance config, failing closed on invalid
  input), `goobers up` (long-lived daemon: scheduler + runner), `goobers run
  <workflow>` (manual trigger, still honoring run conditions), `goobers status`, and
  `goobers trace <run-id>`. This is the owning statement of the CLI lifecycle surface
  (`DEP-022`/`DEP-023` defer here). *(Tiers 1–2)*
- **INST-018 (MUST, Shipped):** The **command registry is the single source of
  truth for the CLI surface** (#4521). Every command, subcommand, and flag MUST be
  declared once in that registry, and every derived artifact — `goobers help`
  output, shell completions, the generated man pages, and `docs/cli/` — MUST be
  **generated from it and drift-guarded in CI**, never hand-maintained. A new
  subcommand that does not regenerate its documentation is a CI failure, not a
  follow-up. This requirement exists because the CLI is a public contract with
  four rendered surfaces, and hand-editing any one of them is how they diverge.
- **INST-019 (MUST, Shipped):** The instance MUST support **operator-requested
  self-update** of its own binary: a staged, verified replacement of the running
  binary with health checking and rollback, driven through the daemon's supervised
  host so an update never leaves the instance without a runnable binary. The
  update **policy selects the target** (`manual` requires a named release tag,
  `on-release` resolves the newest stable release, `on-main` builds a tracked
  branch); it does **not** make the update automatic. Self-update MUST be
  explicitly invoked — by the operator, or by the operator-requested `self-update`
  workflow — and MUST NOT fire on a timer. It MUST NOT interrupt an in-flight run:
  the handoff requests the daemon's ordinary graceful drain rather than killing
  work (see the drain contract in `../guides/supervision.md`).
- **INST-013 (MUST):** After a crash or restart, the local runner MUST recover by
  replaying each run's `state.json` + journal and resuming in-flight runs from the
  last completed stage; recovery MUST never rewrite journal history. Owning
  requirement: `WF-054`; this ID defers to it. *(Tiers 1–2)*
- **INST-014 (MUST):** The Tutor write-boundary MUST hold at every tier: the Tutor
  can write only the `config` repo/directory — enforced by capability-scoped write
  grants locally (hardened to a true permission boundary when `config` is backed by
  its own reviewed git remote) and repo + identity permissions in the cloud
  (`SEC-021`). *(All tiers)*

- **INST-015 (MAY):** *(Tiers 1–3, shipped)* An instance MAY enroll with **one**
  Fleet service and maintain a durable outbound connection to it
  (`goobers fleet join|status|leave`, `internal/fleet`). Enrollment is
  operator-initiated and opt-in: an instance that never joins behaves exactly as
  before the capability existed, and no fleet code path runs. Discovery is a
  `GET` of the service's `/.well-known/goobers-fleet` document; enrollment
  consumes a one-time grant.
- **INST-016 (MUST):** *(Tiers 1–3, shipped)* Fleet identity and credentials MUST
  be stored **outside the instance root**, so copying or restoring an instance
  directory does not clone its fleet identity (`internal/fleet/storage.go`, with
  per-OS private-file enforcement). This is the instance-identity counterpart to
  `INST-010`'s layout rule: the instance root is the unit of copy, and identity
  is deliberately not in it.
- **INST-017 (MUST):** *(Tiers 1–3, shipped)* The connection an enrolled instance
  maintains MUST be **outbound-only**. Fleet enrollment MUST NOT require inbound
  network reachability to the instance, and MUST NOT change the instance's own
  listener posture (`SEC-043` continues to govern `api.listen` unchanged).

## Relationships

The `GET /api/v1/instance` response includes a bounded `maintenance` snapshot
for the retention sweep. The snapshot reports its kind, startup or periodic
trigger, state (`none`, `queued`, `running`, `completed`, `failed`, or
`cancelled`), timestamps, current phase, counters, last result, and sanitized
error summary. It contains no credentials, authenticated URLs, environment
values, or per-candidate history; CLI status adapters use the same model.

- Scaffolded by → **`goobers init`** (tiers 1–2) or deployed from the **`infra` repo**
  via release pipeline (tier 3), with the **`config` repo/directory** as reconciled
  desired state (Deployment spec).
- Hosts → many **Gaggles**.
- Owns → runtime state: run journals, working copies, and the goober-run **Telemetry**
  store.
- Surfaced by → the **Portal** (a window over run journals, never a control plane).

## Open questions

- **INST-Q1:** **Resolved:** dev/prod are **separate instances** (separate provisioning
  + config), not envs-within-one-instance. See `VISION §8`. At tiers 1–2 that is
  simply two instance roots.
- **INST-Q2:** **Resolved (updated 2026-07-12):** what is shared at instance level
  follows the tier — tiers 1–2: the daemon and telemetry rollup store are shared,
  while `runs/` and `workcopies/` are scoped per gaggle; tier 3: AKS, the ADX telemetry store (partitioned per
  gaggle), and logging. Isolated per gaggle at every tier per the isolation ladder
  (`GAG-011/012`, `SEC-001/002/003`, `ARCHITECTURE.md §9`).
