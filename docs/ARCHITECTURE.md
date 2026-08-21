# Goobers — Target Architecture

> Status: **Approved — architecture of record.** Supersedes the implicit "Temporal-first"
> architecture assumed by earlier specs and code. Where an older spec or code path
> contradicts this document, this document wins and the spec/code carries a status
> banner pointing here.
> Last updated: 2026-08-07 · Descriptive/prescriptive status annotated 2026-07-23:
> §4–§7 (as amended) describe shipped, verified behavior of the local runner,
> except the capability namespace rule in §5, which is prescriptive pending its
> atomic migration; §3.2, §10, the remaining V1 work identified in §12, and V2
> are prescriptive roadmap — mandated, not yet built.

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
   deterministic **step-machine** (the doc set also says "state machine"; the terms are
   equivalent) of stages (tasks) and gates. All side effects happen inside stages; the
   machine itself reads no wall clock and takes no hidden inputs (`WF-002`, `WF-020`).
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
- Executes repo-backed stages as local processes in isolated git worktrees;
  deterministic stages may instead request an empty disposable scratch workspace.
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
- Brings durable long waits (multi-day human gates), schedules at scale, and
  per-gaggle worker isolation. **Child workflows** remain a **tier-3 DSL extension**: a
  definition that uses them is tier-3-only until the local runner implements them
  (`CFG-022`, `GAG-010`), and they are not part of the cross-runner conformance
  surface. **Static parallel branches are not** — they are core DSL, implemented by the
  local runner first, and inside the conformance surface
  ([`design/static-fan-out-fan-in.md`](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/design/static-fan-out-fan-in.md) §4). Dynamic
  (data-driven) branch width remains future work.

### 3.3 Conformance property

Because workflow compilation and stage semantics live *above* the seam, the same
workflow definition run with **fixed stage effects** must produce **equivalent run
journals** on either runner. "Equivalent" is a defined relation, not a vibe:

- **The conformance set** is the ordered sequence of orchestration events: run
  started/finished, stage started/finished (policy-retry attempts included), gate
  verdicts, artifacts recorded (with digests), external refs touched, parallel and
  branch lifecycle (`parallel.started`, `branch.started`, `branch.finished`,
  `parallel.finished`) including the branch completeness record. Events are compared in
  `seq` order (§4); parallel-branch events order by `(branch, seq)` **at every tier**.
- **Excluded** from comparison: timestamps and durations; infrastructure-retry
  attempts (attempt events are tagged `policy` vs `infra`, and only `policy`
  attempts are normative); `spans/` contents (telemetry, not conformance);
  `state.json` (a derived checkpoint); the **absolute value of `seq` across distinct
  non-zero branches** (branch interleaving is a scheduling artefact, so `seq` is
  compared *within* a branch — it stays fully normative for the root branch and for
  every run that never forks); and runner-specific annotations, which MUST live under a
  namespaced `runner.*` field — that namespace is the *only* sanctioned runner-specific
  divergence.
  Notification request and delivery-receipt events are also excluded: they are
  deployment output side effects rather than workflow-machine transitions. Their
  provider-neutral contract is documented in
  [`design/notification-output.md`](design/notification-output.md).
- **Fixed stage effects** means: deterministic stages with pinned commands over
  fixture inputs, provider reads mocked or replayed from journaled responses, and
  agentic stages driven by the fixture harness. For **live agentic runs** the
  guarantee is structural — same machine, same branching for the same verdicts, a
  well-formed journal — never payload equality; LLM output varies run to run on any
  runner.

The event schema (issue #8) marks each field normative or excluded, the V0 e2e
walking skeleton asserts journal determinism on the local runner (the conformance
seed), and the V2 conformance harness runs shared fixtures through both runners and
diffs the conformance set. This property is what makes "one system, three tiers"
enforceable rather than aspirational.

## 4. The run journal (provenance contract)

Every run — local or cloud — produces:

```
gaggles/<gaggle>/runs/<run-id>/
  schema.json       # journal schema version + minimum compatible binary
  run.yaml          # pinned identity: workflow name+version, gaggle, trigger, inputs
  state.json        # current machine state; atomically replaced checkpoint
  events.jsonl      # append-only event journal (stage started/finished, gate verdicts,
                    # retries, artifacts recorded, external refs touched); every event
                    # carries a monotonic seq (branch id reserved for tier-3 branches)
  inputs/           # immutable snapshots of run inputs (e.g. the issue body as claimed),
                    # content-digested
  artifacts/        # stage outputs, stored by digest, referenced by pointer
  spans/            # per-stage trace spans incl. within-stage harness events
```

Rules:

- **Publish initialized runs atomically.** Local journal creation builds the
  identity, input snapshots, initial event, checkpoint, lock, and reserved
  directories in a hidden sibling staging area, then atomically renames that
  directory to `<run-id>`. Span export only appends to a directory whose
  `run.yaml` already exists. Before this ordering was enforced, `journal.Create`
  exposed the final directory and created `spans/` and `.lock` before writing
  `run.yaml`; a crash or initialization error could therefore leave a directory
  that looked like half a run, and the span exporter could keep it alive.
  Existing directories from that historical failure mode and half-initialized
  crash residue in the dedicated `.runs.creating` staging area are handled only
  by the operator-invoked `goobers telemetry prune-orphans` command. It reports
  by default, requires `--delete` to remove anything, and never selects a
  directory with `run.yaml`, a recently modified directory, or a staging
  directory whose creation lock is still held. The inactivity window has a
  non-reducible 24-hour minimum.
- **Append-only events; immutable snapshots.** Nothing in a journal is edited after
  the fact. Repairs happen by appending corrective events. The one sanctioned
  exception is secret remediation: `goobers journal redact` replaces a leaked blob
  and appends a redaction event recording the old→new digests, so even the exception
  leaves a trace.
- **Content digests** on inputs and artifacts make runs comparable and make those
  files tamper-evident (the event log itself is trusted-at-rest at tiers 1–2; hash
  chaining is a tier-2+ option, not a baseline claim).
- A stage may additionally mirror its durable outbox files to a configured local
  filesystem root. Stage, workflow, then gaggle configuration wins in that order.
  The mirror is arranged beneath `<root>/<run-id>/`; the journal remains the
  source of truth, and every source and destination path is containment-checked.
- **Version pinning:** a run records the workflow definition version it started on and
  completes on it; definition changes affect only new runs (`WF-016`).
- **Redaction at the boundary:** raw secrets MUST NOT land at rest anywhere under
  `gaggles/*/runs/` or `telemetry.db` (`SEC-041`). The journal package scrubs every event,
  span, snapshot, **and artifact** before write — registry-based (all
  resolver-issued credentials) plus pattern-based scanning for secret-shaped
  material — and scrubbing happens **before digesting**, so digests commit to the
  scrubbed bytes. Misses are remediated via `goobers journal redact` (above).
- The journal is **human-readable first** (`cat`, `jq`, `grep` are legitimate debug
  tools at tier 1) and machine-projectable second (telemetry rollups, portal).
- **Instance-level events have a journal too:** scheduler decisions (trigger fired,
  run started, tick skipped with reason) and claim-ledger transitions append to
  `scheduler/events.jsonl` in the instance root (§6), under the same envelope and
  append-only rules — so the portal, telemetry, and Tutor read scheduling history
  the same way they read runs.

## 5. Stages and their contracts

A workflow is composed of stages of two kinds, plus gates. ("Stage" is this
document's term for what the Task spec calls a task; the terms are equivalent across
the doc set.)

- **Deterministic stages** — arbitrary commands (tests, linters, builders, CI pollers)
  run with declared env, timeout, and retry policy. They use a repository worktree
  by default, or an empty disposable scratch workspace when they do not need a repo;
  commands that require no connectivity may declare `run.network: none`. At tiers 1–2
  these commands are native host subprocesses, not containers; containerized stage
  execution is tracked in [#1494](https://github.com/Agent-Clubhouse/Goobers/issues/1494).
- **Agentic stages** — an agent harness invoked in the stage worktree with an
  **invocation envelope** (goal, context pointers, capability grants); it must finish
  by producing a **result envelope** (status, outputs, artifact pointers). Harness
  choice is a *stage-level detail*: the first adapter is the **GitHub Copilot CLI**;
  other harnesses (e.g. Claude Code) are additional adapters behind the same
  invocation/result contract, not architectural changes.
- **Gates** — evaluate results and branch: automated checks, agentic reviewers, or
  human approval (`GT-*`). A gate is a **machine state, not a stage**: its automated
  and agentic evaluators *run with stage-execution semantics* (declared env, timeout,
  retry, worktree where applicable), but gates carry no stage contract of their own —
  a human gate executes nothing.

Contract rules:

- Stages exchange **artifact pointers** (path + digest inside the journal), never
  implicit shared state.
- Each stage normally runs in a **fresh, isolated, disposable workspace**. Repo-backed
  stages receive a working copy of the target repo: at tiers 1–2 that is a git
  worktree branched off the managed working copy (§6); at tier 3 it is the
  workspace of an ephemeral pod (fresh clone or sparse checkout). Deterministic
  stages may declare `run.workspace: scratch` to receive an empty workspace with
  no repository resolution. The tier-neutral contract is isolation + disposal
  after the run; the worktree is the tiers-1–2 repo-backed mechanism, not the
  contract.
- A repository may instead opt into a node-local **pinned workspace** at
  `workcopies/<repo-key>/pin`. Pinned mode and per-stage worktrees are mutually
  exclusive. One FIFO lease covers the entire run across all gaggles targeting
  that repository, so their stages cannot interleave. The pinned directory is
  outside the per-run `runs/` namespace and is structurally excluded from
  worktree retention.
- Pinned mode is deliberately non-hermetic. With its default `none` clean
  policy, ignored and untracked build state persists between runs;
  `ignored-safe` removes untracked non-ignored files and `full` also removes
  ignored files. Operators opting in accept that the target repository's
  `.gitignore` hygiene is load-bearing for clean run-branch diffs.
- **Capability admission:** a stage may only touch capabilities its definition
  declares, from the canonical registry (`internal/capability`, issue #74) —
  e.g. `github:issues:write`, `repo:push`, `telemetry:read`. Undeclared use, and
  a capability string outside the registry, both fail closed — enforced at
  tiers 1–2 by declaration validation at compile time plus **capability-scoped
  credential non-injection** (an undeclared capability's credentials are
  simply never materialized), and by sandbox policy from V1 (`SEC-042`,
  `SEC-044`). A task whose command, policy, persona, or verdict vocabulary can
  prescribe an external mutation also declares that closed vocabulary in
  `policyActions`. Goober definitions make persona prescriptions
  machine-readable in `policyActions`; capability-gated persona behavior lives
  in `conditionalPolicyActions` and is disabled unless a task explicitly opts
  into both the action and its capability. The compiler rejects unknown
  actions, omitted command/persona actions, and actions whose canonical
  capability is not declared by both the task and its goober.
- **Capability namespaces follow the operation surface:** a stage that dispatches
  an operation through the configured repository provider declares
  `provider:<resource>:<verb>`; a stage that uses provider-specific behavior
  declares `<provider>:<resource>:<verb>`. The two spellings are not aliases,
  and a stage may not declare both spellings of the same operation. Migrating a
  built-in to provider-neutral behavior updates its contract and bundled
  definitions atomically; workflow-version pinning preserves already-started
  runs. See [ADR 0002](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/adr/0002-provider-neutral-capability-namespaces.md).
- **Input-integrity admission:** provider reads, immutable snapshots, artifacts,
  and invocation context pointers carry `trusted`, `maintainer`, `unapproved`, or
  `derived` provenance. A task may declare `minimumIntegrity`; the compiler rejects
  unknown grades and every runner refuses lower-graded input before dispatch with
  the normative journal error code `input_integrity_below_minimum`. Grades persist
  in both invocation envelopes and journal records, so this decision is part of
  the cross-runner conformance surface rather than a `runner.*` annotation.
  Tasks that need a strict minimum may declare `contextFrom` to route only named
  producer tasks or gates into their invocation; omitted sources remain honestly
  graded in the journal and available to other consumers.
- Retries are a runner concern, driven by the stage's declared policy; a retried
  stage appears in the journal as a new attempt, never as overwritten history.
- **Run-control inheritance is explicit:** `runConditions` supplies instance
  defaults, `Gaggle.spec.runControls` overrides them for one workforce, and
  `Workflow.spec.runControls` overrides them for one definition. The resolved
  `maxRepasses`, `stalledRunTimeout`, and optional `maxRunDuration` are pinned
  in `run.yaml` when a run starts, so config reloads cannot retune a run in
  flight. `maxRunDuration` bounds total wall-clock age independently of journal
  activity and is disabled when omitted. An automated or
  agentic gate may override `maxRepasses`. The value bounds cumulative
  re-entries to a branch's target stage across all gates that route back to
  that stage; a pass at one gate does not reset that target's live budget.
  Separate target stages can therefore have independent budgets. Stall
  detection does not have a task-level override: task/gate `timeoutSeconds`
  and retry policies already own per-attempt execution bounds, while the stall
  watchdog protects the run journal as a whole.
- Retry attempt counts and backoff remain declared on each task or executable
  gate. They are intentionally not inherited run controls: they classify and
  repeat one stage attempt, whereas repass and stall budgets bound orchestration
  across stage attempts.

## 6. Instance anatomy (local runner)

```
<instance-root>/
  instance.yaml     # connections: target repo(s), provider (GitHub/ADO), token refs,
                    # telemetry settings
  config/           # the config repo/directory: gaggles, goobers, workflows, gates,
                    # instruction markdown  (the ONLY thing the Tutor may write to)
  gaggles/<gaggle>/
    runs/           # run journals (§4)
    workcopies/     # managed working copies and per-run worktrees
  scheduler/        # instance journal: scheduler decisions + claim ledger (§4)
  telemetry.db      # local rollup store (§8)
```

`goobers init` scaffolds this; `goobers validate` checks it; `goobers up` runs the
daemon (scheduler + runner); `goobers run <workflow>` triggers one manually (still
honoring run conditions); `goobers status` / `goobers trace <run-id>` inspect it.
These are the anchor commands of a wider registry-sourced CLI documented in the
guarded [generated CLI reference](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/cli/README.md),
including the built-in stage kinds workflows invoke as subcommands (`backlog-query`,
`open-pr`, `merge-pr`, `elect-lander`, `apply-verdict`, …). The daemon also serves a
**loopback-only HTTP API**
(`internal/httpapi`: `/api/v1/*` reads, health, event stream) backing an embedded
dashboard (`goobers dashboard`); long-lived daemons run under platform supervision
(systemd/launchd/Windows service — `docs/guides/supervision.md`).
The two-repo split (`infra` vs `config`) from the vision maps onto tiers: at tiers
1–2 "infra" collapses into `instance.yaml` + the binary; at tier 3 it is a real
infra repo (Bicep + GitOps) again. The **Tutor write boundary holds at every tier**:
the Tutor's stages receive a write grant scoped to `config/` and nothing else.
Backing `config/` with its own git remote + required review upgrades that from
runtime enforcement to a hard permission boundary — required at tier 3, recommended
at tiers 1–2 (`SEC-021`, `TUT-006`).

## 7. Scheduling and triggers

- **Shipped triggers — five types, all with live runtime consumers:** `manual`
  (`goobers run`), `schedule` (cron / time-since-last-run, with a catch-up policy
  for missed ticks), `backlog-item`, `signal` (workflow outputs admitted as
  triggers through the scheduler; also `goobers signal`), and `webhook` (signed
  GitHub webhooks, delivered through the signal path —
  `docs/guides/github-webhooks.md`).
- **Backlog-item triggers fan out on eligible work:** the scheduler polls the
  provider's eligible-item count (#344) and dispatches runs accordingly; the run's
  first stage — a built-in deterministic **`backlog-query`** stage kind — still
  performs the actual query and **claims** items (label/assignee marker + claim
  ledger) so concurrent runs never double-process (`WF-031`). A trigger
  `selector`'s KEYS remain required labels (values are ignored because GitHub
  labels are flat strings). A direct backlog-item trigger declares its SEC-047
  approval source separately as `trustLabel`; selectors remain routing criteria
  and never confer maintainer integrity. Gaggle backlogs, backlog-item triggers, and
  `backlog-query` tasks may additionally declare a `labelPredicate`: restricted
  CEL string-membership checks against `labels`, composed with `&&`, `||`, and
  `!`. The CEL predicate is ANDed with legacy label inputs and evaluated exactly
  after any provider-side label optimization. The same surfaces accept a
  `fieldPredicate` over `fields["name"]`, with scalar string/number/bool
  comparisons composed by the same boolean operators. GitHub projects `id`,
  `number`, `state`, `locked`, `comments`, `user.login`, `assignee.login`,
  `created_at`, `updated_at`, milestone number/title, and native dependency
  count (`issue_dependencies_summary.total_blocked_by`); Azure DevOps projects
  every scalar work-item field by its reference name plus `System.Id` and
  `System.Rev`. Gaggle and workflow-trigger field predicates are ANDed.
  Optional or unsupported fields are errors rather than false matches.
  `backlog-query` also accepts `fieldOrder` as comma-separated
  `field[:asc|desc]` terms, applied across the complete candidate set after
  label priority and before FIFO. With none of these additive inputs configured,
  label selection and FIFO remain unchanged. On public repos, eligibility
  requires a maintainer-applied trust label: backlog content is untrusted input
  (`SEC-047`).
- **A claim's lifetime is the ledger's, and the marker's lifetime is the
  claim's.** `scheduler/claims.json` is the only source of truth for
  exactly-once processing (`BL-005`); the provider-visible `goobers:claimed`
  label and its claim breadcrumb are a projection of it, never an input to
  eligibility. The projection is retired at the same moment the lease is —
  a stage does it on the paths that have one (`issue-close-out`,
  `backlog-query --release`), and the instance's terminal cleanup does it for
  every run that reaches a terminal phase still holding a lease. The `no-work`
  outcome is the case that makes the second path necessary rather than
  defensive: it short-circuits to `completed` from whatever stage reported it,
  so no close-out stage runs. Backlog curation's reconciliation of markers with
  no backing lease remains the backstop for a projection that could not be
  written (a forge outage, a credential-less instance, a non-GitHub provider),
  not the primary mechanism — so the window in which the ledger and the forge
  disagree is bounded by one provider call, not by one curation interval.
- **Readiness conditions** enforced before any run starts: max parallel runs per
  workflow and per instance, `maxRunsPerHour` / `maxRunsPerDay` run budgets,
  chain-depth bounding (`maxChainDepth`), open-PR caps (`maxOpenPRs`, #353), and
  provider-quota / rate-limit-reset gating.
- **Still prescriptive (V1):** true k8s-style selector value semantics (the
  shipped boolean label surface is CEL rather than k8s selectors), routing one
  item across multiple candidate workflows with a
  priority single-winner election (`SCH-010` full form, `SCH-011`), dead-letter /
  unrouted-item surfacing (`SCH-012`), and an item priority field (`SCH-030`).
  None of these have runtime consumers.
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
| 1 — Solo | None (local trust) | Env vars / token file, redacted from journals | Worktree + process isolation, capability-scoped credential injection |
| 2 — Team | Optional OIDC on portal/daemon | Env/file or team secret store | + per-goober credential scoping (shipped, #823); sandboxed stage execution (V1, mechanism per ADR 0001) |
| 3 — Cloud | Entra ID (OIDC) | **Azure Key Vault** | Per-gaggle namespaces + identities, network policy (existing `SEC-*`) |

The protocol (OIDC) and the seam (an `Authenticator` + a secret-resolver interface)
are constant; tiers select implementations. The Tutor write-boundary (`SEC-021`) is
enforced at every tier — capability-scoped write grants locally (hardened to a true
permission boundary when `config/` is backed by its own reviewed git remote),
repo+identity permissions in the cloud.

## 10. Substrate drop-in map

Every Azure/cluster component from the original design remains — as the tier-3
implementation of a seam the local runner also implements. "This is where it goes":

| Seam | Tiers 1–2 (local) | Tier 3 (cloud drop-in) |
|---|---|---|
| Runner / durability | Local runner, file journal | **Temporal** (self-hosted, Postgres-backed), history → journal projection |
| Journal & artifact store | Plain files under `gaggles/<gaggle>/runs/` + `scheduler/` | Journal projection on a single-writer RWO instance volume; fleet-wide content-addressed artifacts on RWX/blob storage |
| Stage execution | Local process in worktree | **AKS** ephemeral agent pods |
| Scheduling / triggers | Embedded scheduler (cron eval in `goobers up`) | **Temporal Schedules** |
| Config delivery | Startup-loaded local `config/`; opt-in `--watch-config` for direct edits; or continuous Git `workflowSource` reconciliation via polling, local-ref/webhook wakeups, and last-known-good retention | **ArgoCD** sync → CRDs → **Goobers operator** |
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

**Status: V0 acceptance passed** (`docs/V0-ACCEPTANCE.md`). The V0.5/V0.6+ waves
then closed and expanded the PR loop: the `reference-workflows/` reference config
now defines **ten** workflows: backlog curation, docs updater, implementation,
merge review, PR remediation, quality sprint, self update, test-suite quality,
Tutor, and work nomination. Together they provide the canonical patterns for curating and
implementing work, reviewing, remediating, and **merging PRs autonomously**, and
maintaining the product and its workforce — a ratified product direction (G2 in
`docs/design/v0/pr-lifecycle-loop.md`; sibling sequencing in
`docs/design/sibling-pr-sequencing.md`). `reference-workflows/` is the canonical, tested
reference configuration these capabilities are built against. It is not a live
instance or a synced mirror of any deployment's running config; operators maintain
deployed config separately, and it can drift from the checked-in reference.

### V1 — Teams and remaining hardening

Arbitrary tier-1/tier-2 repositories are current scope, not a future V1
prerequisite. Repository-neutral GitHub onboarding and multi-gaggle configuration
are shipped, alongside the Azure DevOps provider, packaged-install machinery, the
journal-backed portal, capability-scoped and per-goober credential injection,
optional OIDC, and a narrow Tutor workflow. The remaining V1 roadmap is team and
hardening work: sandboxed stage execution and expansion of the packaged-install,
authentication, and Tutor surfaces beyond their current slices.

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
