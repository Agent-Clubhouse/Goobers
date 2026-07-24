# V2 — Cloud & Large Team

**Status:** Approved for backlog planning (PO directive, 2026-07-16). Items filed to milestone
"V2 — cloud scale" remain **future investments**: backlog-only, not `goobers:approved`, not
eligible for automated implementation until the PO promotes them.

**Audience:** V2 planning. This doc turns the placeholder V2 issues (#39, #40, #41, #155, #156)
plus the PO's 2026-07-16 cloud/large-team brief into a concrete design with an issue map.
docs/ARCHITECTURE.md remains the architecture of record; this doc details it, it does not
amend it.

---

## 1. What V2 means

ARCHITECTURE.md §10 already commits the macro shape: the **Temporal runner behind the same
runner seam** with history→journal projection, the **local↔Temporal conformance harness**,
**Kubernetes stage execution** (ephemeral agent pods, per-gaggle namespaces/identities), and
**GitOps config delivery** (operator + Argo-style sync). None of that is re-litigated here.

The PO's V2 brief (2026-07-16) adds the *large team* dimension, which the committed shape
implies but never detailed:

1. **Multiple humans with merge access to the workflow-config repo** — config CD must be
   multi-writer-safe and auditable, not single-operator.
2. **Very large repos** — clean mirror-clone + full worktree checkout per stage attempt does
   not survive a 10GB monorepo; we need cached starting points / baked images.
3. **Sandboxes** — teams need provisionable, isolated **test environments** (cloud) and the
   local analogy (containers / micro-VMs) for multiple parallel isolated test flows including
   e2e and UI automation. Credentials and provisioning shape vary by team, so this is a seam,
   not a product.
4. **Credential isolation** — different credentials for different stages (optional; same
   token remains the default), per-gaggle identities, cloud secret stores.
5. **Multiple gaggles** as a real runtime property, not just a config-model property.
6. **In-flight features carried to cloud** — the portal/daemon API (milestone #14), workflow
   CD (#15), and HITL (#16) all gain a cloud/multi-user form here rather than being redesigned.

### 1.1 Grounding: what exists today (2026-07-16 survey)

- **Runner seam is real and already has two implementations.** `internal/invoke` defines the
  neutral boundary (`Goober`, `Deterministic`, `Automated`); the local runner
  (`internal/runner`) and the quarantined Temporal engine (`internal/engine`) both walk the
  same compiled `workflow.Machine`. #156 is the authoritative drift ledger for the engine.
- **Working copies:** one `git clone --mirror` per repo URL under `workcopies/`, a fresh
  `git worktree add` per **stage attempt**, full teardown after (`internal/worktree`). No
  partial clone, no reference/alternates cache, no sparse checkout. Clone auth for private
  repos is **unwired** (`internal/credentials/git.go` has no production callers; the mirror
  path clones unauthenticated).
- **Credentials:** `internal/credentials` is multi-token, capability-scoped, fail-closed;
  per-capability sourcing shipped in V0.2 (`credentials:` block); the resolver interface
  is stabilized (#822) and per-goober scoping shipped (#823). Per-stage grants and
  resolvers beyond env/file remain planned but unbuilt.
- **Daemon** *(updated 2026-07-23)*: serves a **loopback-only** HTTP API
  (`internal/httpapi`: `/api/v1/*` reads, health, dashboard/event-stream endpoints;
  loopback bind validated) — still **no remote listener** of any kind. CLI↔daemon
  coordination remains file-based (flock singleton, delegated-trigger sweep). Auth is a
  seam (#38): the `Authorizer` interface exists (tier-1 `AllowAll`); no OIDC
  implementation yet.
- **State:** journals are files with **local flock** single-writer guards; the scheduler
  keeps trigger state in memory reconstructed by journal replay; crash-resume assumes sole
  ownership of `runs/`. None of this is multi-node-safe — by design, Temporal is the
  multi-node answer, not distributed flocks.
- **Multi-gaggle:** config model supports N gaggles; runtime layout is still flat
  (GAG-011 scoping is V1 epic #34); tier-3 per-gaggle namespace/identity (GAG-012) is V2.

### 1.2 Relationship to V1 epics

V1 epics #34 (arbitrary repos/multi-gaggle scoping), #35 (sandboxed execution + per-goober
creds), #38 (auth/OIDC + secret-resolver seam) each carry explicit "out of scope — V2"
lists. This milestone is where those deferrals land. Sequencing rule: **V2 items that extend
a V1 seam depend on that seam existing, not on the whole epic finishing** — each issue names
its concrete prerequisite.

---

## 2. Workstream A — Temporal runner, conformance, k8s execution

Detailed by the existing epics; this pass converts them from placeholders into staged,
~1-PR child issues.

### A1. Engine revival (epic #39)

Revive `internal/engine` against the drift ledger (#156), in this order:

1. **Invocation completeness** — `buildInvocation` must populate `Workspace`, `Limits`,
   `Capabilities`, `ContextPointers` per the closed invocation schema. Without this every
   capability-scoped credential fails closed the moment a real executor is wired.
2. **Retry semantics** — map `Task.Retry` onto an explicit Temporal `RetryPolicy`
   (never Temporal's unlimited default); tag attempts `policy` vs `infra` so projection
   emits the normative attempt classes (§3.3).
3. **Gate parity** — bounded repass/escalation identical to the local runner
   (`MaxRepasses`, escalation on exhaustion); stage-status semantics parity
   (failure→non-gate fails, blocked halts).
4. **Registry invariants** — enforce the shape invariants the JSON schema owns
   (agentic⇒goober, deterministic⇒run); fail closed on zero-value `DeterministicRun{}`;
   fix the version-number TOCTOU.
5. **History→journal projection** — emit the full normative event set (incl. `ref.touched`,
   attempt classes) into the standard `runs/<id>/` layout via `journal.ConformanceView`.
6. **Worker deployment shape** — a `goobers worker` entrypoint (task queues, graceful
   drain, versioned worker identity) so workers are a deployable unit.
7. **Temporal Schedules** (SCH-042) — cron triggers map to Temporal Schedules; claiming
   uses workflow-id exactly-once identity.

### A2. Conformance harness (epic #40)

The enforcement mechanism for "one system, three tiers": shared workflow fixtures run
through both runners; journals diffed over the conformance set (§3.3 — `seq`-ordered
orchestration events; timestamps/durations/infra-retries/`runner.*` excluded). Builds on
the walking-skeleton determinism assertion (#29) and `internal/journal/conformance.go`.
Ships as `make test-conformance`; CI wiring belongs to the Validation & CI milestone.

### A3. Kubernetes execution (epic #41)

Agentic stages as ephemeral pods; per-gaggle namespaces + identities (GAG-012/SEC tier-3);
operator + GitOps delivery revived (DEP-012). Infra prerequisites are documented, not
provisioned, per §7 (customer-managed cluster; docs/design/k8s-infra-shape.md).

### A4. Tier-3 DSL extensions (#155)

Unchanged: parallel branches + child workflows, tier-3-only, outside the conformance
surface, sequenced after A1 proves the sequential path.

---

## 3. Workstream B — Large-repo working copies

**Problem.** Today a stage attempt costs: (first ever) full mirror clone of the repo, plus
(every attempt) a full-tree `git worktree add` and teardown. On a 10GB / million-file
monorepo that is minutes per stage and unbounded disk churn. Tier 3 is worse: DEP-Q1 notes
each pod starts cold.

**Decision (recommended): a layered cache strategy, cheapest layer first, all measured
before/after by a benchmark harness.** No layer changes run semantics — the stage contract
(fresh, disposable, isolated working copy; TSK-040) is preserved at every layer.

- **B1. Partial-clone mirrors.** Mirrors become blobless (`--filter=blob:none`) with
  on-demand blob fetch; fetch refspec narrowed from `+refs/*:refs/*` to heads + goobers
  branches. Biggest win for clone time and mirror disk; transparent to worktrees.
- **B2. Sparse checkout.** A workflow/gaggle may declare a path cone
  (`project.checkout.sparse: [paths]`); worktrees materialize only the cone. Opt-in,
  validated (agentic stages get the cone documented in their invocation context so the
  agent knows the tree is partial).
- **B3. Reference/alternates cache.** A node-level object cache
  (`workcopies/_objects/<repo-key>`) shared via `--reference`/alternates by all gaggles on
  the node targeting the same repo, so N gaggles ≠ N full mirrors. Lifecycle: GC only when
  no dependent mirror exists (alternates make deletion unsafe otherwise — fail closed on GC).
- **B4. Worktree pooling.** Instead of add/remove per stage attempt, a bounded pool of
  pre-created worktrees per repo; "provision" becomes checkout+clean (`git clean -xdf` +
  `reset --hard`) of a pooled tree. Disposal semantics unchanged (pool reset IS the
  disposal); marker/reap logic extends to pooled trees.
- **B5. Baked workspace snapshots (tier 3).** The pod-form answer to DEP-Q1: a periodically
  rebaked **workspace image** (OCI image or PVC snapshot) containing the mirror + toolchain;
  pods start from the snapshot and `git fetch` the delta. Rebake is a scheduled deterministic
  workflow (it's just a workflow — same doctrine as producers). Warm pod pool (DEP-Q3)
  layers on top.
- **B0 (first, gating).** A **provisioning benchmark harness** + synthetic large-repo
  fixture generator, so B1–B5 land with measured numbers instead of vibes, and regressions
  are catchable. Shared with the Validation & CI milestone.

**Also in this workstream: B6, authenticated clone/fetch (#667 — shipped).** Private-repo
mirrors use the repo's configured token via the askpass helper: the composition root wires
`worktree.WithGitEnvironment` for GitHub repos with a token ref (ADO was already wired
through its own credential sources), covering the initial mirror clone and every refresh
fetch. Token-less (public) repos keep the unauthenticated environment byte for byte.

---

## 4. Workstream C — Sandboxes: provisionable test environments

**What the PO asked for:** some things are very slow or expensive to build/test locally;
teams should be able to provision an isolated **testing environment** — in the cloud
(shape and credentials vary by team) or locally (container / micro-VM) — and run multiple
isolated test flows in parallel, including e2e and UI automation.

**Naming.** This is distinct from *agent execution sandboxing* (SEC-044, V1 epic #35, which
confines the agent process). We call this feature the **test sandbox**: an environment a
*stage runs tests against*, not a cage the stage runs inside. Docs and DSL use `sandbox`
for this; #35 keeps "sandboxed execution."

**Decision (recommended): a provider seam + declarative profiles, BYO provisioner for
cloud.** Teams' cloud environments differ too much to productize provisioning; what we own
is the lifecycle contract, isolation guarantees, credential routing, and journal record.

- **C1. Sandbox contract + DSL surface.** Instance/gaggle config gains `sandboxProfiles:`
  (name → provider kind + provider config + credential refs). A task declares
  `requires.sandbox: <profile>`. Runner lifecycle: provision → inject connection context →
  run stage → teardown (always, crash-reaped like worktrees). Journal events
  `sandbox.provisioned` / `sandbox.released` with digested provisioning inputs; connection
  details are secrets (scrubbed). Fail closed: profile missing/unprovisionable ⇒ stage
  `blocked`, never "run without the sandbox."
- **C2. Local container provider.** Containers (Docker/Podman-compatible) as the tier-1/2
  provider: per-sandbox network+FS isolation, N parallel sandboxes per node, image +
  compose-style spec in the profile. This is what makes **multiple, isolated, parallel
  local e2e/UI-test flows** real.
- **C3. Local micro-VM provider (spike).** Evaluate micro-VMs (e.g. Lima/krunkit-class) for
  flows containers can't isolate (kernel features, nested daemons, GUI stacks). Spike
  first; only productize if C2 proves insufficient for a named flow.
- **C4. Cloud/BYO provider.** The provider invokes a **team-supplied provisioner** — a
  deterministic command or k8s Job/CRD the team owns — passing profile parameters and
  receiving a connection manifest back (schema-validated, closed). Team credentials come in
  as ordinary named token refs granted per-profile (fail-closed, never ambient). Goobers
  guarantees lifecycle, audit, and teardown-on-abandon; the team owns what a "sandbox"
  actually is (namespace, ephemeral env, stack deployment…).
- **C5. UI-test automation flow.** A shipped workflow shape proving the seam: build →
  deploy-to-sandbox → run browser-driver suite against sandbox URL → gate on results →
  teardown. Doubles as the portal's own e2e vehicle.
- **C6. Sandbox credentials.** Per-profile credential grants distinct from stage
  capability grants (a stage may talk to its sandbox without holding the creds that
  *provisioned* it). Extends `credentials:`/Grant model; fail-closed both directions.

---

## 5. Workstream D — Credential isolation at team scale

Continues the committed ladder (per-capability → per-goober (#35/S1) → per-stage →
cloud): 

- **D1. Per-stage grants.** `{stage → [capability → ref]}` overrides, optional — default
  remains inherit-from-goober/instance ("pass same creds" stays the zero-config path).
  Validation rejects a grant for a capability the stage doesn't declare (fail closed).
- **D2. Secret-resolver implementations.** Finalize the `Resolve(ctx, name)` seam (#38)
  and ship the first non-env/file resolvers: cloud secret stores (Key Vault per SEC-010;
  the seam keeps it vendor-neutral). Refs in config are unchanged — only resolution moves.
- **D3. Per-gaggle identity (tier 3).** Each gaggle's pods run as that gaggle's workload
  identity (GAG-012/SEC-001/002, SEC-Q1); resolver scopes secrets per gaggle so gaggle A
  cannot resolve gaggle B's refs even inside one cluster.
- **D4. Short-lived credentials seam.** Where the platform supports minting (e.g. GitHub
  App installation tokens), a resolver that mints per-run, TTL-bounded tokens instead of
  reading static PATs. Seam + one implementation; static PATs remain supported.

---

## 6. Workstream E — Multi-user config CD & team operations

Workflow CD (#15) is designed single-daemon/single-writer. V2's team reality: the config
repo has **multiple individuals with merge access**, and the daemon (or operator) follows
`main` of that repo.

- **E1. Config PR validation gate.** A reusable CI check for the config repo that runs
  `goobers validate` (plus feature-registry compat checks from milestone #12) against every
  PR. Branch protection on the config repo is the *authorization* mechanism for
  multi-writer — Goobers does not reinvent repo authz; it makes the merge gate able to
  reject invalid config before it ever reaches `main`.
- **E2. Multi-writer reconcile semantics.** Reconcile is snapshot-consistent per commit:
  the daemon/operator always renders one committed tree (a single SHA), advances
  monotonically, never blends two HEADs, retains last-known-good on invalid HEAD (already
  designed in workflow-cd.md — extended with: journal event per applied config change
  recording SHA, author, changed definitions). In-flight runs stay pinned (WF-016).
- **E3. Config provenance & rollback.** `git revert` on the config repo IS rollback;
  Goobers records apply/rollback as instance journal events so "who changed what, when,
  and what did the daemon do about it" is answerable from the journal alone.
- **E4. Multi-human HITL attribution.** Tier-2 HITL actions (#16) arrive over the
  authenticated API (see F below); every intervention journal event carries the
  authenticated principal, not just "recorded who" free-text. Approvals become
  authorization-checked per the #37/#172 seam with roles (view / operate / admin).

---

## 7. Workstream F — In-flight features at cloud: API, auth, portal

The dashboard milestone (#14) builds the loopback `/api/v1` read service; HITL (#16) adds
mutation endpoints behind the access-control seam. V2 takes that same surface off-box:

- **F1. Network exposure hardening.** The daemon/API serves beyond loopback: TLS,
  configurable bind, request limits, and the `Authenticator` seam **required non-null**
  when bind ≠ loopback (fail closed — no accidental open cloud API).
- **F2. OIDC/Entra as configured issuer + RBAC.** Per #38's ladder: generic OIDC at
  tier 2, Entra as *a configured issuer* at tier 3, with role mapping (view/operate/admin)
  feeding the Authorizer seam. Portal's existing MSAL scaffolding gets wired for real.
- **F3. Signal/webhook ingestion.** The write-capable API sink the scheduler already
  anticipates (`Scheduler.Signal`): authenticated webhook → external-signal trigger, and
  config-repo push hooks (E) fold into this same single inbound surface.
- **F4. Portal cloud deployment shape.** Portal as static assets served with/near the API,
  environment-configured API base, SSE through ingress (heartbeats/timeouts), documented in
  the k8s infra shape doc.

---

## 8. Workstream G — Multi-gaggle runtime at scale

- **G1.** Per-gaggle runtime scoping (GAG-011) is V1 epic #34/H2 and is a **prerequisite**;
  V2 assumes `gaggles/<gaggle>/…` layout.
- **G2. Gaggle-partitioned execution.** Tier 3 maps gaggles to Temporal task queues +
  worker deployments per gaggle (or weighted shares on shared workers) so one hot gaggle
  cannot starve others — the tier-3 form of #34/H4 fairness.
- **G3. Gaggle onboarding/offboarding runbook + automation.** Adding a gaggle = config PR
  (namespace, identity, profiles, budgets); removal reaps runtime state safely. At team
  scale gaggles churn; this must not be hand-surgery.

---

## 9. Kubernetes infra shape (kept separate, documentation-first)

Per PO directive: we **define the shape** of required infra on a **customer-managed
cluster**; Bicep/Terraform provisioning is explicitly out of scope. See
**docs/design/k8s-infra-shape.md** (companion doc in this PR) for the full statement:
control-plane namespace (operator, Temporal + Postgres, API/portal), per-gaggle namespaces,
storage classes (journal volume / blob), ingress + TLS, secrets integration, network
policy defaults, node-pool & sizing guidance, registry requirements. Issues in this
workstream (K1–K3) are documentation + reference-manifest work, not provisioning code.

---

## 10. Explicitly out of scope for this milestone

- Bicep/Terraform/cloud-account provisioning (customer-managed cluster assumed).
- Windows/Linux node support — separate milestone (docs/design/cross-platform-support.md).
- CI/validation-loop enrichment — separate milestone
  (docs/design/validation-and-ci-enrichment.md).
- Managed multi-tenant SaaS, cross-instance federation, marketplace/distribution.
- Replacing the local runner: tiers 1–2 remain first-class forever (one system, three tiers).

## 11. Sequencing & dependencies (summary)

1. **Foundations first:** B0 benchmark harness; #34/H2 gaggle scoping (V1); dashboard API
   (#14) and HITL seams (#16) as designed — V2 extends, never forks them.
2. **Engine track:** A1 drift fixes → A2 conformance → A3 k8s execution → A4 DSL
   extensions. Conformance gates everything after A1.
3. **Repo-scale track (B)** and **sandbox track (C)** are independent of the engine track
   and each other; both start with their contract/benchmark step.
4. **Cred isolation (D)** rides the #35/#38 seams; **team CD (E)** rides milestone #15;
   **cloud API (F)** rides #14/#16/#38.

## 12. Open questions (build-time, not product)

- OQ-1: worktree pool (B4) vs per-attempt add — is `clean+reset` provably equivalent to
  fresh-add for isolation (untracked state, hooks, submodules)? Benchmark + adversarial test
  decide.
- OQ-2: sandbox connection-manifest schema — one closed schema for all providers, or
  per-kind schemas under one envelope? (Lean: one envelope, per-kind payload, closed.)
- OQ-3: does the local daemon at tier 2 serve the network API directly, or does tier-2
  networked access require the tier-3 deployment? (Lean: tier 2 may bind non-loopback with
  auth required; document the risk posture.)
- OQ-4: baked-image rebake cadence & staleness bound (B5) — scheduled vs drift-triggered.
