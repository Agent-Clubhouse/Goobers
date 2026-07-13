# Goobers — Target Architecture

> Status: **Approved — architecture of record.** Supersedes the implicit "Temporal-first"
> architecture assumed by earlier specs and code. Where an older spec or code path
> contradicts this document, this document wins and the spec/code carries a status
> banner pointing here.
> Last updated: 2026-07-12

## 1. One system, three deployment tiers

Goobers is **one system** that scales across three frontiers without changing what a
workforce *is* or how it is defined:

| Tier | Who | Shape | Substrate |
|---|---|---|---|
| **1 — Solo** | A single user on a laptop or headless desktop running one gaggle against hobby projects | Single binary, no service dependencies, files as durable state | **Local runner** |
| **2 — Small team** | A team with a moderate repo and one or two gaggles; runs on a workstation, shared box, or a small cloud VM/container | Same binary as tier 1, run as a long-lived daemon | **Local runner** |
| **3 — Cloud scale** | A team on a very large monorepo; several gaggles, each with its own area of the codebase, its own backlog, prompts, and priorities | Clustered orchestration, distributed workers, Kubernetes agent pods | **Temporal runner** |

The tier you run at is a **deployment choice, not a product fork**. Workflow
definitions, goober definitions, gates, provenance, and the portal are identical at
every tier. Scaling up means changing *where workflows run*, never *what they are*.

## 2. Invariants (true at every tier)

1. **Definitions as code.** Goobers, gaggles, workflows, and gates are markdown +
   YAML in a config repo/directory. No UI creates or mutates them (`CFG-*`).
2. **Workflows are deterministic step-machines.** A workflow definition compiles to a
   deterministic state machine of stages (tasks) and gates. All side effects happen
   inside stages; the machine itself reads no wall clock and takes no hidden inputs
   (`WF-002`, `WF-020`).
3. **Every run produces a run journal** (§4) — an append-only, inspectable,
   content-digested record of what happened. The journal is the *product's* history;
   runner internals are an implementation detail behind it.
4. **Stages communicate through envelopes and artifact pointers** (§5). No stage
   reaches into another stage's state.
5. **Systems of record live outside the instance.** Durable truth is the user's repos
   and backlog; Goobers owns only runtime state and its own run telemetry.
6. **Fail closed.** Undeclared capabilities, unvalidated definitions, or a journal
   that cannot be written all stop a run rather than degrade it.
7. **The portal is a window.** It reads the journal and telemetry stores; it is never
   a control plane.

## 3. The runner seam

The single load-bearing abstraction is the **runner**: the component that advances a
compiled workflow state machine, durably records progress, and schedules stage
execution. Two runners implement the same contract:

### 3.1 Local runner (tiers 1–2, ships first)

- One Go binary (`goobers`). No database, no message bus, no service cluster.
- Owns the run journal directly as **plain files** (§4). Durability = append + fsync;
  crash recovery = replay `state.json` + journal on restart and resume from the last
  completed stage.
- Executes stages as local processes in isolated git worktrees.
- An embedded scheduler fires cron triggers and enforces run conditions
  (max-parallel, budgets).

### 3.2 Temporal runner (tier 3, V2)

- The same compiled state machine hosted as a Temporal workflow; stages become
  activities dispatched to distributed workers; agentic stages run in ephemeral
  Kubernetes pods.
- Temporal history is the *internal* durability mechanism. The runner **projects
  history down into the same run-journal format** (§4) so the portal, telemetry,
  Tutor, and operators see one shape everywhere. Raw Temporal mechanics (replay,
  task queues, worker lifecycle) are never part of the product surface.
- Brings durable long waits (multi-day human gates), schedules at scale, parallel
  branches, child workflows, and per-gaggle worker isolation.

### 3.3 Conformance property

Because workflow compilation and stage semantics live *above* the seam, the same
workflow definition + pinned inputs must produce **semantically identical run
journals** on either runner (timing and runner-specific annotations aside). A
conformance harness (V2) runs shared fixtures through both and diffs the journals.
This property is what makes "one system, three tiers" enforceable rather than
aspirational.

## 4. The run journal (provenance contract)

Every run — local or cloud — produces:

```
runs/<run-id>/
  run.yaml          # pinned identity: workflow name+version, gaggle, trigger, inputs
  state.json        # current machine state; atomically replaced checkpoint
  events.jsonl      # append-only event journal (stage started/finished, gate verdicts,
                    # retries, artifacts recorded, external refs touched)
  inputs/           # immutable snapshots of run inputs (e.g. the issue body as claimed),
                    # content-digested
  artifacts/        # stage outputs, stored by digest, referenced by pointer
  spans/            # per-stage trace spans incl. within-stage harness events
```

Rules:

- **Append-only events; immutable snapshots.** Nothing in a journal is edited after
  the fact. Repairs happen by appending corrective events.
- **Content digests** on inputs and artifacts make runs comparable and tamper-evident.
- **Version pinning:** a run records the workflow definition version it started on and
  completes on it; definition changes affect only new runs (`WF-016`).
- **Redaction at the boundary:** credentials and secrets never enter the journal;
  stage runners scrub known secret material before events/spans are written.
- The journal is **human-readable first** (`cat`, `jq`, `grep` are legitimate debug
  tools at tier 1) and machine-projectable second (telemetry rollups, portal).

## 5. Stages and their contracts

A workflow is composed of stages of two kinds, plus gates:

- **Deterministic stages** — arbitrary commands (tests, linters, builders, CI pollers)
  run in the stage worktree with declared env, timeout, and retry policy.
- **Agentic stages** — an agent harness invoked in the stage worktree with an
  **invocation envelope** (goal, context pointers, capability grants); it must finish
  by producing a **result envelope** (status, outputs, artifact pointers). Harness
  choice is a *stage-level detail*: the first adapter is the **GitHub Copilot CLI**;
  other harnesses (e.g. Claude Code) are additional adapters behind the same
  invocation/result contract, not architectural changes.
- **Gates** — evaluate results and branch: automated checks, agentic reviewers, or
  human approval (`GATE-*`).

Contract rules:

- Stages exchange **artifact pointers** (path + digest inside the journal), never
  implicit shared state.
- Each stage runs in an **isolated git worktree** of the target repo's working copy;
  worktrees are disposable and cleaned up after the run.
- **Capability admission:** a stage may only touch capabilities its definition
  declares (e.g. `github:issues:write`, `repo:push`, `telemetry:read`). Undeclared
  use fails closed.
- Retries are a runner concern, driven by the stage's declared policy; a retried
  stage appears in the journal as a new attempt, never as overwritten history.

## 6. Instance anatomy (local runner)

```
<instance-root>/
  instance.yaml     # connections: target repo(s), provider (GitHub/ADO), token refs,
                    # telemetry settings
  config/           # the config repo/directory: gaggles, goobers, workflows, gates,
                    # instruction markdown  (the ONLY thing the Tutor may write to)
  runs/             # run journals (§4)
  telemetry.db      # local rollup store (§8)
  workcopies/       # managed working copies of target repos; per-run worktrees
                    # branch off these
```

`goobers init` scaffolds this; `goobers validate` checks it; `goobers up` runs the
daemon (scheduler + runner); `goobers status` / `goobers trace <run-id>` inspect it.
The two-repo split (`infra` vs `config`) from the vision maps onto tiers: at tiers
1–2 "infra" collapses into `instance.yaml` + the binary; at tier 3 it is a real
infra repo (Bicep + GitOps) again. The **permission boundary stays in all tiers**:
the Tutor's identity can write only `config/`.

## 7. Scheduling and triggers

- **V0:** cron-expression triggers only (`goobers up` evaluates them), plus run
  conditions: max parallel runs per workflow/instance, per-workflow run budgets.
- Backlog-item and external-signal triggers (`WF-010`) remain in the model and layer
  onto the same scheduler; at V0 backlog consumption is expressed as a cron-triggered
  workflow whose first stage queries the provider for eligible items and **claims**
  them (label/assignee marker) so concurrent runs never double-process (`WF-031`).
- At tier 3, cron triggers become Temporal Schedules and claiming coordinates across
  distributed workers — same declared semantics, different substrate.

## 8. Telemetry (two stores, unchanged doctrine)

The two-store separation from the vision is preserved at every tier:

| Store | Holds | V0 (local) | Tier-3 drop-in |
|---|---|---|---|
| **Goober-run telemetry** (ours) | Traces, per-stage success/duration, within-stage harness events, errors | Spans in the run journal + a **SQLite rollup** (`telemetry.db`) queryable via CLI | OTLP → **Azure Data Explorer** |
| **Project telemetry** (theirs, optional) | The target product's own observability | Any queryable source the nomination workflow is configured to read | ADX or whatever the team already runs |

Instrumentation is OpenTelemetry throughout (already in `internal/telemetry`); only
the exporter changes per tier. Work-nomination workflows read these stores; the Tutor
(V1+) mines the run store.

## 9. Security and auth ladder

| Tier | Identity/auth | Secrets | Isolation |
|---|---|---|---|
| 1 — Solo | None (local trust) | Env vars / token file, redacted from journals | Worktree + process isolation |
| 2 — Team | Optional OIDC on portal/daemon | Env/file or team secret store | + sandboxed stage execution, per-goober credential injection (V1) |
| 3 — Cloud | Entra ID (OIDC) | **Azure Key Vault** | Per-gaggle namespaces + identities, network policy (existing `SEC-*`) |

The protocol (OIDC) and the seam (an `Authenticator` + a secret-resolver interface)
are constant; tiers select implementations. The Tutor write-boundary (`SEC-021`) is
enforced at every tier — filesystem/repo permissions locally, repo+identity
permissions in the cloud.

## 10. Substrate drop-in map

Every Azure/cluster component from the original design remains — as the tier-3
implementation of a seam the local runner also implements. "This is where it goes":

| Seam | Tiers 1–2 (local) | Tier 3 (cloud drop-in) |
|---|---|---|
| Runner / durability | Local runner, file journal | **Temporal** (self-hosted, Postgres-backed), history → journal projection |
| Stage execution | Local process in worktree | **AKS** ephemeral agent pods |
| Config delivery | Local `config/` directory watched by daemon | **ArgoCD** sync → CRDs → **Goobers operator** |
| Run telemetry store | Journal spans + SQLite | **ADX** via OTLP |
| Secrets | Env/file | **Azure Key Vault** |
| AuthN | None / optional OIDC | **Entra ID** |
| Provisioning | `goobers init` | **Bicep** (`infra/`) + release pipeline |

## 11. Repo impact map

| Area | Disposition |
|---|---|
| `api/` types + JSON envelope schemas | **Keep** — the definition & envelope contracts; extended for DSL v0 |
| `internal/engine` compile/state machine | **Extract** the substrate-neutral core (compile, states, gates) for the local runner; the Temporal workflow function around it becomes the V2 adapter |
| `providers/` | **Keep & extend** — GitHub issues/PR operations are V0 workload |
| `internal/telemetry` | **Keep** — add journal/SQLite exporter |
| `internal/operator`, `cmd/operator`, `internal/configsync` (CRD apply path), `cmd/scheduler` | **Quarantine** — tier-3 components; status-bannered, kept compiling, revived in V2 |
| `infra/` (Bicep, ArgoCD, Temporal) | **Quarantine** — tier-3 provisioning, revived in V2 |
| `portal/` | **Keep** — retarget from mock client to reading run journals (V1) |
| `cmd/goober-runtime` | **Superseded** by the local runner's stage execution; folds into the `goobers` binary |

## 12. Roadmap

### V0 — “Works locally, begins to build itself”

A single machine runs a gaggle against a real GitHub repo (including this one):
install/init locally; separate managed working copy; local config directory using the
definitions-as-code DSL; read/modify GitHub issues; open/poll/close PRs;
deterministic stages (shell); agentic stages (Copilot CLI adapter); clean stage
contracts with artifact pointers and durability; cron triggers + max-parallel
conditions; rich per-stage/within-stage telemetry to the local store. Three shipped
workflows prove it: **backlog curation**, **work nomination**, **implementation**
(with optional reviewer gates, local deterministic gates, and a CI-poll/repass loop).
Definition of done: feed issues into the backlog and watch them get curated, scoped,
and implemented into PRs by the instance running on your own machine.

### V1 — Arbitrary repos, teams, hardening

Everything V0 does, deployable over arbitrary tier-1/tier-2 repos; **Azure DevOps**
provider (issues + PRs); packaging/install story; sandboxing + credential injection
for agentic stages; portal reads run journals; optional team auth (OIDC); **Tutor**
workflow if it needs more than the standard primitives.

### V2 — Cloud scale

The **Temporal runner** behind the same seam with journal projection and the
conformance harness; Kubernetes stage execution (agent pods); operator + ArgoCD/GitOps
config delivery revived; Azure substrate drop-ins (ADX exporter, Key Vault, Entra)
per §10.

## 13. Relationship to the requirement specs

The specs in `docs/requirements/` remain the requirement source of truth; their
stable IDs (`WF-*`, `GBO-*`, `DEP-*`, …) are referenced by build issues. Each spec
carries tier annotations aligned to this document; requirements that only exist at
tier 3 (e.g. `DEP-011` Temporal, `DEP-012` ArgoCD/operator) are marked as such rather
than deleted — they are the drop-in specs for V2.
