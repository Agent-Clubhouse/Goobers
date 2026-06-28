# Goobers — Product Vision (Draft v0.2)

> Status: **Working draft for red-lining.** Authored by Lead from the PO's vision.
> Supersedes v0.1 (which was built on a misread of what the product is).
> Last updated: 2026-06-28

## 1. One-liner

**Goobers is an open, self-hosted platform that adds a virtual AI coding workforce to
a team's existing engineering stack — defined and managed entirely as code, learning
and improving itself over time.**

## 2. Who it's for & the moment of pain

An EM and their PM counterpart face a backlog growing faster than headcount.
Requirements from leadership keep getting bigger; human resources are fixed. They need
throughput they can't hire for.

Goobers is the answer: a coding workforce they stand up themselves, on infrastructure
and subscriptions they already own.

## 3. Product principles

- **Open platform, not a managed service.** Users bring their own Azure, agent harness
  (GitHub Copilot or Claude Code), repo (GitHub or ADO), and backlog. They deploy and
  own their instance.
- **Built from simple, understandable primitives** chained into something bigger than
  the parts. The repo should read as approachable, not magic.
- **Config-as-code, not a UI.** All setup, configuration, and management is done in
  code — markdown, folders, a YAML manifest, and a deploy. The portal is a window, not
  a control panel.
- **Systems of record live outside the instance.** Runtime state/execution is managed
  in-system, but durable truth lives in the user's own git repos and backlog.
- **Self-improving.** The workforce learns from its own telemetry and folds lessons
  back into itself via PRs.
- **Process creates consistency.** Workflows make outcomes and data collection
  predictable regardless of how a work item originated.

## 4. The experience (narrative spec)

1. **Discover & deploy.** Team finds the repo, reads the README, sees it builds on
   stuff they already have. They configure a release pipeline with their connection
   info and hit deploy.
2. **First boot.** A simple web portal appears: *"I'm alive — your goober gaggle is
   ready"* — but empty; nothing is configured yet.
3. **Define a goober in code.** A few markdown files + folders + a YAML manifest +
   deploy → the dashboard shows a new team member, ready to work. Their first: a
   **coder**.
4. **Work flows.** PO drops an item in the connected backlog. The coder picks it up,
   reads it, makes a plan, makes fixes. **Every work item is a trace** through the
   system: start/stop telemetry, detailed per-step logs, what the agent did, script
   outputs.
5. **Goobers generate their own work.** PO adds autonomous goobers — a perf-bug hunter
   that crawls the codebase and files backlog items; one that mines telemetry for
   errors; one that finds test-coverage gaps; custom domain-specific ones. They write
   into the **same backlog** the humans and coders use.
6. **Scale by a number.** The coder is single-threaded; work piles up. Instead of
   copying it, the PO changes a **scale factor** and redeploys → 2, 3, 5 coders.
7. **Impose process via Workflows.** Churn rises, quality dips. The PO defines
   workflows: take an item → research & implement → lint + unit tests → **reviewer
   goobers** with defined scope must sign off. Now every item — however it originated —
   follows the same process and meets the same quality bar, and data collection
   follows the same shape.
8. **The Tutor closes the loop.** A specialized goober continuously analyzes the
   gaggle's output — prompts, gates, results. It finds patterns: the same tests keep
   failing, a reviewer keeps repeating itself, a coverage policy keeps getting missed.
   It **trains the goobers** by opening a **PR that changes their definitions** (skills,
   instructions, tools).

## 5. Architecture (how it works)

- **Instance = simple infra:** an AKS cluster, storage + logging, some disk.
- **A goober = a standard agent harness** (GitHub Copilot agent harness, or Claude
  Code) running standard tools: MCPs, skills, instruction markdown.
- **Ephemeral runs.** A run is a pod instance prepped with auth, the agent harness,
  signed in, with a fresh copy of the repo. An **invocation hook** hands the agent a
  block of context/data + the task definition. When done, the agent calls a specific
  **completion tool/method** so the workflow/system can track completion.
- **Automatic data collection.** Management tools injected via MCP or hooks collect
  data about what the agent is doing; machine logs are collected too.
- **Workflow engine: off-the-shelf, likely Temporal.** We need a deterministic state
  machine that creates process/output consistency and supports discrete steps for data
  collection, retries, and recovery.
- **Definitions as code (Helm-like).** The count and characteristics of goobers live in
  code alongside all definitions. When the **Tutor** "trains," it opens a PR against
  these definitions.

### Standard setup (repos/sources)
1. **Goober infra codebase** — Bicep + the goober codebase + agent definitions;
   controls the actual goober deployment. *(Singleton.)*
2. **Project codebase** — the work being targeted.
3. **The backlog.** *(Singleton.)*
4. **Optional:** project/product telemetry in Azure Data Explorer (the *project's*
   observability — see telemetry note below).

Less standard setups may have multiple repos or telemetry sources, but **the backlog
and goober infra are always singletons.**

### Two separate telemetry stores (do not conflate)
The same separation that keeps **goober infra ≠ project repo** applies to telemetry —
there are two distinct stores that are never the same instance/database:

| Store | Holds | Consumed by | Whose world |
|---|---|---|---|
| **Project telemetry** (ADX, *optional*) | The product's runtime errors/perf — the user's existing observability | **Producer goobers** read *from* it to file backlog items (error miner, perf hunter) | The project's |
| **Goober-run telemetry** (we own it) | The gaggle's own operational data — traces, step logs, prompts, gate results | The **Tutor** mines it to train goobers | The goober instance's |

The goober-run store is provisioned/owned by the instance. **ADX is the likely/right
choice**, but the store tech is flexible (a separate ADX vs. in-cluster) — what is
*not* flexible is that it is **separate from the project's ADX**.

## 6. Primitives

| # | Primitive | Definition |
|---|---|---|
| 1 | **Instance / Tenant** | A deployed Goobers installation. |
| 2 | **Gaggle** | A siloed workforce of goobers. A tenant has many. |
| 3 | **Goober** | An agent instance within a gaggle. |
| 4 | **Workflow** | A defined process modeled as a state machine, within a gaggle. |
| 5 | **Task** | A state in a workflow. Deterministic/code-driven or agentic. Has defined input states, work to do, and a goal. |
| 6 | **Gate** | A validation state in a workflow. Used for conditional branching — a check: did the task complete? have conditions been met? |

## 7. The execution model (confirmed)

**The system is the orchestrator. Workflows invoke goobers; goobers never *directly*
invoke or orchestrate workflows.** A goober's *outputs* — a filed backlog item, an
emitted signal — can still become triggers the system evaluates, so a goober's action
*can indirectly* cause a workflow to start. The distinction: orchestration **always
routes through the system scheduler**; a goober never calls a workflow itself.

- A **deterministic system scheduler** decides when to start workflow runs. A workflow
  is eligible for a new run **IFF** its **trigger** fires *and* its **readiness
  conditions** are met. Canonical example: *idle worker capacity exists → this workflow
  is unblocked → dequeue the next backlog item → invoke the run.*
  - **Triggers:** a backlog item becoming available, a schedule / time-since-last-run,
    or an external signal.
  - **Readiness conditions:** worker/resource capacity, concurrency limits, other
    resource constraints — a run starts only when these are satisfied.
- A **Workflow** is the unit the scheduler invokes. Its **Tasks** are the steps —
  agentic tasks are executed by **goobers**, deterministic tasks by code. **Gates** are
  validation states that branch the flow. So "a goober doing work" = **a workflow
  invoking a goober** to execute an agentic task.
- A **Goober** is fundamentally a **definition** (role, instructions, skills, tools,
  scale factor) in the infra repo. At runtime it materializes as ephemeral pod(s). The
  dashboard "team member" is the definition; the pod is transient.
- **The default starter is just a length-1 workflow** — a single-stage, implement-only
  process. It ships so a gaggle works immediately. It is an ordinary workflow, not a
  special "implicit" mechanism. Authoring richer workflows is how you add research,
  tests, reviewers, and gates.
- **Producers are not a separate beast — they are just workflows.** Perf hunter, error
  miner, coverage finder, **Tutor**, researchers, implementers — all the *same*
  taxonomy. They differ only by **trigger** (schedule/signal vs. backlog item) and
  **stages**. They can chain runs and carry gates like any other workflow.
- **There is no "outside a workflow."** Every goober runs *within* a workflow; the
  floor is just a simple single-stage workflow. A "standalone" goober is simply one in
  its own minimal workflow.
- **Scaling** a goober/workflow = N replicas drawing from shared work → requires a
  **work-claiming mechanism** so two replicas never grab the same item. *(Open — §8.)*
- **Routing** — with many workflows/goobers, something maps backlog items → the right
  workflow. *(Open — §8.)*

## 8. Decisions & open questions

### Decided
| Decision | Choice | Notes |
|---|---|---|
| v1 harness | **GitHub Copilot agent harness only** | Claude Code deferred; no harness abstraction required for v1. |
| v1 providers | **Both GitHub + ADO, via a provider abstraction** | Abstract repo + backlog from the start. |
| Tutor self-modification gate | **Configurable, no default stance** | Instance decides whether Tutor PRs need human approval. |
| Goober-run telemetry store | **Separate store, ADX likely** | Provisioned/owned by the instance; **never** the project's ADX. Store tech flexible. |
| Execution model | **System scheduler invokes workflows; workflows invoke goobers** | §7 confirmed. Goobers may *indirectly* trigger workflows via outputs, routed through the scheduler. |
| Gate model | **One Gate primitive, pluggable evaluator** | Evaluator kind = automated / agentic / human. See `requirements/gate.md`. |
| Routing | **Labels + selectors (k8s-style)** | Items labeled; workflows declare selectors. See `requirements/scheduler.md`. |
| Work-claiming | **Lease-based atomic claim** | Visibility timeout; auto-release on failure/crash. See `requirements/scheduler.md`. |
| Portal scope | **Observability-first; minimal runtime ops only** | Config-time = code only; runtime ops (gate approvals, retry/intervene) = minimal portal. See `requirements/portal.md`. |
| Tutor change scope | **Goobers + workflows/gates (not platform code)** | Bounded blast radius; approval configurable per instance. See `requirements/tutor.md`. |
| Gaggle isolation | **Namespace + identity per gaggle** | k8s namespace + per-gaggle Azure identity; Key Vault secret refs. See `requirements/security.md`. |

### Still open
All 14 area specs are drafted (`requirements/`). Remaining open items are now **within
each spec** as `*-Qn` questions rather than vision-level gaps. The highest-leverage
cross-cutting ones to resolve next:
- **Build-vs-buy boundary** — what Temporal/GitOps tooling provides vs. what we build
  (`SCH-Q4`, `DEP-Q2`, `DEP-Q5`).
- **Tutor safety guardrail** — can it weaken/remove a required gate? (`TUT-Q2`.)
- **Common schemas** — context/data block, task result, trace/span shape (`TSK-Q1/Q2`,
  `TEL-Q3`).
- **Claim/lease location** given an external backlog (`SCH-Q5`, `BL-Q1`).

## 9. Strawman phasing (not committed)

- **Phase 0 — Spec.** This doc → requirements → reference instance/goober/workflow
  definitions.
- **Phase 1 — Walking skeleton.** Deploy instance + portal; one coder goober; pull one
  backlog item through a default workflow; emit a trace. (Narrative steps 1–4.)
- **Phase 2 — Workforce.** Autonomous producer goobers; scaling + work-claiming;
  routing. (Steps 5–6.)
- **Phase 3 — Process.** User-defined workflows, gates, reviewer goobers. (Step 7.)
- **Phase 4 — Learning loop.** Telemetry store + Tutor + training PRs. (Step 8.)
