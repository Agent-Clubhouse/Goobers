# Goobers — Product Vision (Draft v0.3)

> Status: **Working draft for red-lining.** Authored by Lead from the PO's vision.
> Supersedes v0.2. v0.3 re-anchors the architecture on **one system that scales
> across three deployment tiers** — local-first, cloud as a drop-in — per
> [`ARCHITECTURE.md`](ARCHITECTURE.md), which is the architecture of record.
> Last updated: 2026-07-12

## 1. One-liner

**Goobers is an open, self-hosted platform that adds a virtual AI coding workforce to
a team's existing engineering stack — defined and managed entirely as code, learning
and improving itself over time. It starts as a single binary on one machine and
scales, without changing a definition, to clustered orchestration over a large
monorepo.**

## 2. Who it's for & the moment of pain

Three users, one system:

1. **The solo builder** — one person, a laptop or headless desktop, a gaggle working
   their hobby or side projects while they sleep.
2. **The small team** — an EM/PM pair with a moderate repo and one or two gaggles,
   running on a workstation or a small cloud box.
3. **The platform org** — a team on a very large monorepo, several gaggles each with
   its own area of the codebase, its own backlog, its own prompts and priorities.

All three face the same pain: a backlog growing faster than the hands available.
Goobers is the answer at every scale: a coding workforce they stand up themselves, on
infrastructure and subscriptions they already own — where "infrastructure" may be
nothing more than the machine in front of them.

## 3. Product principles

- **One system, three tiers.** The tier you run at (solo / team / cloud-scale) is a
  deployment choice, never a product fork. Definitions, provenance, and the portal
  are identical everywhere; only the substrate underneath changes.
- **Radically simple at the floor.** Tier 1 is a single binary: no database, no
  service cluster, durable state as plain inspectable files. Every cloud component
  (AKS, ADX, Entra, Key Vault, ArgoCD, Temporal) is a **drop-in** behind a named seam
  — see `ARCHITECTURE.md §10` for exactly where each one goes.
- **Open platform, not a managed service.** Users bring their own machine or cloud,
  agent harness (GitHub Copilot first; others as adapters), repo (GitHub or ADO), and
  backlog. They deploy and own their instance.
- **Built from simple, understandable primitives** chained into something bigger than
  the parts. The repo should read as approachable, not magic.
- **Config-as-code, not a UI.** All setup, configuration, and management is done in
  code — markdown, folders, a YAML manifest, and a deploy. The portal is a window,
  not a control panel.
- **Systems of record live outside the instance.** Runtime state/execution is managed
  in-system, but durable truth lives in the user's own git repos and backlog.
- **Inspectable by design.** Every run leaves an append-only, content-digested
  **run journal** a human can read with standard tools. Orchestration internals are
  never the product surface.
- **Self-improving.** The workforce learns from its own telemetry and folds lessons
  back into itself via PRs.
- **Process creates consistency.** Workflows make outcomes and data collection
  predictable regardless of how a work item originated — and regardless of which
  tier the workflow runs on.

## 4. The experience (narrative spec)

1. **Discover & install.** A solo builder finds the repo, installs one binary, runs
   `goobers init` against a project they already have. A cloud-scale team instead
   configures a release pipeline with their connection info and deploys to their
   cluster. Same product, different door.
2. **First boot.** The instance reports *"I'm alive — your goober gaggle is ready"* —
   but empty; nothing is configured yet.
3. **Define a goober in code.** A few markdown files + folders + a YAML manifest →
   the roster shows a new team member, ready to work. Their first: a **coder**.
4. **Work flows.** PO drops an item in the connected backlog. On its next trigger the
   coder picks it up, reads it, makes a plan, makes fixes, opens a PR. **Every work
   item is a trace** through the system: a run journal with start/stop telemetry,
   detailed per-stage logs, what the agent did, script outputs.
5. **Goobers generate their own work.** PO adds autonomous goobers — a backlog
   curator that dedupes, tags, and splits oversized items; a perf-bug hunter that
   crawls the codebase and files backlog items; one that mines telemetry for errors;
   one that finds test-coverage gaps. They write into the **same backlog** the humans
   and coders use.
6. **Scale by a number.** The coder is single-threaded; work piles up. Instead of
   copying it, the PO changes a **scale factor** and redeploys → 2, 3, 5 coders.
7. **Impose process via Workflows.** Churn rises, quality dips. The PO defines
   workflows: take an item → research & implement → lint + unit tests → **reviewer
   goobers** with defined scope must sign off — optionally polling remote CI and
   bouncing failures back to the implementer. Now every item — however it originated —
   follows the same process and meets the same quality bar, and data collection
   follows the same shape.
8. **The Tutor closes the loop.** A specialized goober continuously analyzes the
   gaggle's output — prompts, gates, results. It finds patterns: the same tests keep
   failing, a reviewer keeps repeating itself, a coverage policy keeps getting missed.
   It **trains the goobers** by opening a **PR that changes their definitions**
   (skills, instructions, tools).
9. **Outgrow the box.** The monorepo org runs the *same definitions* on the cloud
   tier: clustered orchestration, distributed workers, per-gaggle isolation, durable
   multi-day approval gates. Nothing about the workforce was rewritten to get there.

## 5. Architecture (how it works)

> The authoritative treatment — runner seam, run-journal contract, stage contracts,
> substrate map, roadmap — is [`ARCHITECTURE.md`](ARCHITECTURE.md). This section is
> the summary.

- **One workflow contract.** Every workflow compiles to a **deterministic
  step-machine**; all side effects live in stages; every run produces an append-only,
  content-digested **run journal**. The journal — not any engine's internals — is the
  product's history: the portal, telemetry, and Tutor read it at every tier.
- **Two runners behind one seam.** Tiers 1–2 use the **local runner**: one Go binary,
  files as durable state, crash-recovery by journal replay, stages as local processes.
  Tier 3 uses the **Temporal runner**: the same compiled machine hosted on self-hosted
  Temporal, stages dispatched to distributed workers, with history projected down into
  the same journal format. Same definitions + inputs ⇒ equivalent journals on either
  runner (the conformance property).
- **A goober = a standard agent harness** (GitHub Copilot CLI first; Claude Code and
  others as adapters) running standard tools: MCPs, skills, instruction markdown.
  Harness choice is a stage-level detail behind one invocation/result contract.
- **Ephemeral runs.** A run executes in a disposable, isolated environment prepped
  with auth and a fresh working copy of the target repo — a **git worktree + local
  process** at tiers 1–2, an **ephemeral pod** at tier 3. An **invocation envelope**
  hands the agent context + task; the agent finishes via a **completion/result
  envelope** so the workflow can track completion.
- **Automatic data collection.** Management tools injected via MCP or hooks collect
  what the agent did; per-stage spans and machine logs land in the run journal and
  the run-telemetry store.
- **Definitions as code (Helm-like).** The count and characteristics of goobers live
  in code alongside all definitions. When the **Tutor** "trains," it opens a PR
  against these definitions.

### Standard setup (repos/sources)

The Goobers side of an instance is split by change cadence and blast radius:

1. **Instance provisioning** — *(singleton)* at tiers 1–2 this collapses to the
   binary + `instance.yaml` written by `goobers init`; at tier 3 it is a real
   **`infra` repo** (Bicep, cluster bootstrap, connection wiring) with strict review.
2. **`config` repo/directory** — the workforce as code: manifests + goober /
   workflow / gate definitions + agent markdown. *(Singleton.)* The living config;
   changes constantly and is **the only place the Tutor writes** (see §7 /
   `requirements/tutor.md`).

Plus the user's existing sources:

3. **Project codebase(s)** — the work being targeted.
4. **The backlog.** *(Singleton per gaggle's scope.)*
5. **Optional:** project/product telemetry (e.g. ADX) — the *project's* observability,
   read by nomination workflows (see telemetry note below).

The **platform/engine code itself is upstream** — consumed as a released binary /
images / charts, not a per-instance repo (forkable, since Goobers is open).

> **Why split provisioning from config:** it turns Tutor containment from a *policy*
> into a *permission boundary* — the Tutor's identity gets write access to `config`
> only. It also matches change cadence: provisioning happens once; config drives
> continuous desired state. The boundary holds at every tier — filesystem permissions
> locally, repo + identity permissions in the cloud.

### Two separate telemetry stores (do not conflate)

| Store | Holds | Consumed by | Whose world |
|---|---|---|---|
| **Project telemetry** (*optional*; ADX or whatever the team runs) | The product's runtime errors/perf — the user's existing observability | **Producer goobers** read *from* it to file backlog items (error miner, perf hunter) | The project's |
| **Goober-run telemetry** (we own it) | The gaggle's own operational data — run journals, traces, stage logs, prompts, gate results | The **Tutor** mines it to train goobers; the PO reads it in the portal | The goober instance's |

The goober-run store is provisioned/owned by the instance. At tiers 1–2 it is the
run journals plus a local rollup store; at tier 3 it drops into **ADX**. What is
*not* flexible is that it is **separate from the project's telemetry**.

## 6. Primitives

| # | Primitive | Definition |
|---|---|---|
| 1 | **Instance / Tenant** | A deployed Goobers installation (any tier). |
| 2 | **Gaggle** | A siloed workforce of goobers. A tenant has many. |
| 3 | **Goober** | An agent instance within a gaggle. |
| 4 | **Workflow** | A defined process modeled as a deterministic state machine, within a gaggle. |
| 5 | **Task** | A state in a workflow. Deterministic/code-driven or agentic. Has defined input states, work to do, and a goal. |
| 6 | **Gate** | A validation state in a workflow. Used for conditional branching — a check: did the task complete? have conditions been met? |
| 7 | **Run** | One execution of a workflow: pinned inputs, a run journal, a trace. |

## 7. The execution model (confirmed)

**The system is the orchestrator. Workflows invoke goobers; goobers never *directly*
invoke or orchestrate workflows.** A goober's *outputs* — a filed backlog item, an
emitted signal — can still become triggers the system evaluates, so a goober's action
*can indirectly* cause a workflow to start. The distinction: orchestration **always
routes through the system scheduler**; a goober never calls a workflow itself.

- A **deterministic system scheduler** decides when to start workflow runs. A workflow
  is eligible for a new run **IFF** its **trigger** fires *and* its **readiness
  conditions** are met.
  - **Triggers:** a schedule (cron / time-since-last-run) — first to ship; a backlog
    item becoming available; or an external signal.
  - **Readiness conditions:** worker/resource capacity, concurrency limits (e.g. max
    parallel runs), run budgets — a run starts only when these are satisfied.
- A **Workflow** is the unit the scheduler invokes. Its **Tasks** are the steps —
  agentic tasks are executed by **goobers**, deterministic tasks by code. **Gates** are
  validation states that branch the flow. So "a goober doing work" = **a workflow
  invoking a goober** to execute an agentic task.
- A **Goober** is fundamentally a **definition** (role, instructions, skills, tools,
  scale factor) in config. At runtime it materializes as ephemeral run environments —
  worktree processes at tiers 1–2, pods at tier 3. The roster "team member" is the
  definition; the run environment is transient.
- **The default starter is just a length-1 workflow** — a single-stage, implement-only
  process. It ships so a gaggle works immediately. It is an ordinary workflow, not a
  special "implicit" mechanism. Authoring richer workflows is how you add research,
  tests, reviewers, and gates.
- **Producers are not a separate beast — they are just workflows.** Backlog curator,
  perf hunter, error miner, coverage finder, **Tutor**, researchers, implementers —
  all the *same* taxonomy. They differ only by **trigger** + **stages**. They can
  chain runs and carry gates like any other workflow.
- **There is no "outside a workflow."** Every goober runs *within* a workflow; the
  floor is just a simple single-stage workflow.
- **Scaling** a goober/workflow = N replicas drawing from shared work → requires a
  **work-claiming mechanism** so two replicas never grab the same item.
- **Routing** — labels + selectors map backlog items → the right workflow (see
  `requirements/scheduler.md`).

## 8. Decisions & open questions

### Decided

| Decision | Choice | Notes |
|---|---|---|
| Architecture of record | **One system, three deployment tiers; two runners behind one seam** | See `ARCHITECTURE.md`. Local runner ships first (V0); Temporal runner is the tier-3 drop-in (V2). |
| Workflow engine | **Deterministic step-machine over an append-only run journal** | Tiers 1–2: **local runner** — single binary, files as durable state. Tier 3: **Temporal self-hosted in-cluster** (OSS/MIT, Postgres persistence), history projected into the same journal format. Buy the tier-3 engine, own the contract. |
| Provenance | **Run journal is the product's history** | Append-only events, immutable input snapshots, content digests, secrets redacted at the boundary. Portal/Tutor/telemetry read the journal, never runner internals. |
| First harness | **GitHub Copilot CLI only** | Claude Code and others deferred — but harness choice is a **stage-level adapter detail** behind one invocation/result contract, not an architectural commitment. |
| v1 providers | **GitHub first, ADO next, via a provider abstraction** | Abstract repo + backlog from the start. GitHub issues/PRs are the V0 workload; ADO lands in V1. |
| Tutor self-modification gate | **Governed by `config` PR controls** | Tutor authors freely within `config`; humans hold the quality bar via branch protection / required review / CODEOWNERS. No bespoke in-product restriction. See `requirements/tutor.md`. |
| Goober-run telemetry store | **Journal spans + local rollup (tiers 1–2); ADX drop-in (tier 3)** | Provisioned/owned by the instance; **never** the project's store. OTel instrumentation throughout; only the exporter changes per tier. |
| Execution model | **System scheduler invokes workflows; workflows invoke goobers** | §7 confirmed. Goobers may *indirectly* trigger workflows via outputs, routed through the scheduler. |
| Gate model | **One Gate primitive, pluggable evaluator** | Evaluator kind = automated / agentic / human. See `requirements/gate.md`. |
| Routing | **Labels + selectors (k8s-style)** | Items labeled; workflows declare selectors. See `requirements/scheduler.md`. |
| Work-claiming | **Lease-based atomic claim, owned by the runner** | Tiers 1–2: claim ledger in instance state + provider-visible marker. Tier 3: Temporal workflow-id identity (one workflow per item id = exactly-once). Backlog item mirrors status for humans — not the claim source of truth. |
| Portal scope | **Observability-first; minimal runtime ops only** | Config-time = code only; runtime ops (gate approvals, retry/intervene) = minimal portal. Reads run journals. See `requirements/portal.md`. |
| Tutor change scope | **Goobers + workflows/gates (not platform code)** | Bounded blast radius; approval configurable per instance. See `requirements/tutor.md`. |
| Isolation | **Ladder by tier** | Tiers 1–2: worktree + process isolation, capability admission, credential redaction; V1 adds sandboxing + per-goober credential injection. Tier 3: namespace + identity per gaggle, Key Vault refs, network policy. See `requirements/security.md`. |
| Repo topology | **Provisioning split from `config`** | At tiers 1–2 provisioning collapses to binary + `instance.yaml`; at tier 3 it is the `infra` repo (Bicep + GitOps). Either way the Tutor write-boundary is a *permission boundary*. Platform code is upstream. See §5. |
| Config delivery | **Local config directory watched by the daemon (tiers 1–2); ArgoCD + Goobers operator via CRDs (tier 3)** | Same validated definitions; different delivery. OSS throughout. See `requirements/deployment.md`. |
| Common schemas | **JSON envelopes + OpenTelemetry traces** | Standard invocation-context and result envelopes; run=trace / stage·gate·scheduler=span in OTel. See `requirements/telemetry.md`, `requirements/task.md`. |
| Identity / auth | **Ladder by tier: none → optional OIDC → Entra ID** | One `Authenticator` seam; the protocol (OIDC) is constant, the issuer changes. See `requirements/security.md`, `requirements/portal.md`. |
| Telemetry data policy | **Redact at ingest + configurable retention** | Secrets/PII redacted as collected; instance sets retention window. See `requirements/telemetry.md`. |
| Environments | **Separate instances per env** | dev/prod are separate Goobers instances (separate provisioning + config). See `requirements/instance.md`. |

### Asserted cloud-tier (tier-3) defaults — Azure-native; revisit at build

These are the **drop-in points** (`ARCHITECTURE.md §10`): each names where an Azure
component slots in when an instance scales to tier 3. None are load-bearing below it.

- **Gaggle identity:** AKS **workload identity** (managed-identity federation) per gaggle.
- **Secret/auth injection:** Key Vault via CSI driver; harness + git tokens short-lived,
  injected per run, never in images/repo.
- **Tool/egress containment:** per-goober tool **allowlist** in its definition; restricted
  pod egress via network policy (allowlist provider/telemetry endpoints).
- **Pod latency:** sparse-checkout/cache for fresh repo copies + optional warm pod pool.
- **AKS scaling:** cluster autoscaler + HPA sized to gaggle load.
- **Telemetry partitioning:** goober-run store partitioned per gaggle (aligns isolation).
- **Goober↔workflow:** a goober definition MAY be referenced by multiple workflows; the
  per-task invocation envelope differentiates behavior.
- **Parallelism:** expressed at the **workflow** level (a task = one goober run), not
  within a single task.

### Still open

Remaining `*-Qn` items in the specs are **build-time design** — exact manifest field
schemas, folder layouts, gate branch-expression syntax, per-provider auth and
rate-limit handling, definition composition mechanics. These are intentionally
deferred to implementation, not open product decisions.

## 9. Roadmap

Committed milestones live in `ARCHITECTURE.md §12` and as GitHub milestones on the
repo:

- **V0 — "Works locally, begins to build itself."** The local runner end-to-end on
  one machine, running backlog-curation, work-nomination, and implementation
  workflows against a real GitHub repo — including this one.
- **V1 — Arbitrary repos, teams, hardening.** ADO provider, packaging, sandboxing +
  credential injection, portal on journals, optional team auth, Tutor.
- **V2 — Cloud scale.** Temporal runner + conformance harness, Kubernetes execution,
  GitOps config delivery, Azure substrate drop-ins.
