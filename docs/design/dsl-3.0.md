# DSL 3.0

Status: approved — Goobernetes v1 design. Encodes the PO decision record in
[goobernetes-decisions.md](goobernetes-decisions.md) (2026-08-22).

DSL 3.0 is the workflow-language half of Goobernetes: the clean-break version that makes
distributed placement *authorable* — where a stage may run (`runsOn`), what the instance
offers (`runners:`), and where repo state crosses a runner boundary (declared handoff
edges). It ships with a migrator (`goobers fix --to 3.0`) and lands beside a frozen DSL 2.0
interpreter; 2.0 remains the only backward-compatibility contract and never learns
distributed features (PO-D0). Where this document and the decision record disagree, the
record wins.

**What this is not.** It is not the runner/scheduler architecture, the state/journal design,
the restrictions mechanism design, or the image contract — those are companion Goobernetes
documents keyed to PO-D3/D5/D7/D8. This document specifies only the language surface, its
validation, and its migration. Decisions here are numbered D1–D18; `PO-Dn` cites the
decision record.

Current-state citations are against `main` @ `54c42936`.

---

## 1. Decisions

| # | Decision | Why |
| --- | --- | --- |
| D1 | The scheduling surface is one new stage-level block, **`runsOn`**. Credential grants keep the name `capabilities:` unchanged — closed registry, security surface untouched (PO-D2) | "Capability" already means three things (`Task.Capabilities` credential grants, `Task.RequiredCapabilities` runner tokens, `WorkflowSpec.Requires` provider keys — `api/v1alpha1/workflow_types.go:197,221-232,603-614`) and CAP001 exists to police confusing the first two. Scoping the tag set inside `runsOn.capabilities` leaves exactly one bare `capabilities:` meaning at task level |
| D2 | `runsOn.os` is a **validated enum**: `linux` \| `windows` \| `macOS`. This formally supersedes the locked #659 ruling ("platform is a token, not a field") — see §7 | The drift hazard #659 guarded against — two platform vocabularies coexisting — is closed the opposite way: 3.0 **rejects** `os=*` tokens outright (D12), so the vocabularies structurally cannot coexist in one document (PO-D2) |
| D3 | **Explicit-complete semantics**: unspecified = no requirement. There is no `any` keyword to write | Authors state facts about the stage, never guesses about the fleet. An os-unspecified stage has *no* OS requirement; placement policy (ours to own, not the author's) prefers — and will wait, bounded, for — a Linux-class runner when the inventory has one; on a single-OS instance the stage runs where it can (PO-D2) |
| D4 | Quantities (`cpu`, `memory`, `disk`) use **Kubernetes quantity strings verbatim** (`2000m`, `4Gi`). Stage minimums become pod resource *requests*; *limits* come from the runner's declared ceiling, never from the stage. On local modes resource requirements are **advisory** (warnings, RNR004), never errors | Minimum-quantity (≥) satisfaction is a new comparison model the token vocabulary deliberately excludes ("never a range", `internal/runnercap/runnercap.go` package doc); this resolves the #1529 structured-model question with typed fields beside — not inside — the exact-match tag set (PO-D2) |
| D5 | `runsOn.capabilities` is today's open tag set **moved, not re-invented**: `internal/runnercap` grammar, exact-string set membership, no ranges, no installation | The mechanism shipped as RRQ-1/#1101 and is PO-confirmed (2026-07-20). Only the spelling and location change; matching semantics are byte-compatible |
| D6 | The **base runner contract** (goobers binary, stage-contract environment, network to provider endpoints, credential delivery) is implicit on every runner and **not a declarable tag** | Tier-1/2 authors write nothing; a declarable base would immediately be cargo-culted onto every stage (PO-D2) |
| D7 | **Derived requirements** keep annotation burden near zero: builtins derive their needs from stage identity via the providerstage manifest, which **gains DSL-version linkage as a funded prerequisite**; agentic stages derive `harness:<name>` from the goober's existing `harness:` field; `sh`/`make` stages derive `run:shell`. Credentials stay strictly capability-gated | The manifest is unversioned today (`internal/providerstage/manifest.go` — no SinceVersion anywhere) and shared by both interpreters, so a requirement change is a hard WF010 with no deprecation window (the ed11ae81 incident). The derive-or-declare precedent is CONF-6/#2079 (`WorkflowSpec.Requires`, `workflow_types.go:601-609`) |
| D8 | Instance config: **`runners:` (plural) supersedes `runner:` (singular)**. No `runners:` ⇒ the legacy singular block is the implicit `self` entry — zero-change upgrade. Instance config gains a **`schemaVersion`** field (PO-D3) | `runner:` today is exactly one runner's claims (`internal/instance/config.go:120-126,262-270`); mapping it to the `self` row preserves every existing install byte-identically. The surface is strictly loaded on both sides (`DisallowUnknownFields`, `additionalProperties:false`, `schema_parity_test.go`) and its live copy is a known drifted fork — this is its first version field |
| D9 | Inventory changes are **restart-only in v1**; updates follow **accept-and-pin**: in-flight runs finish against their pinned snapshot | `instance.yaml` is startup-only today (`cmd/goobers/configreload.go:147,204` digest covers only `config/`). Accept-and-pin answers #3449: no drain machinery is required for config updates (PO-D4) |
| D10 | **Runner claims are trusted in v1** (RRQ-1 model): a false claim degrades to a runtime error with a named diagnostic; probe-pod verification is a later honesty layer (PO-D3) | The spike shape already documents the lie mode ("claim os=windows on a Linux container"); v1 states the trust model instead of pretending to verify |
| D11 | **Declared repo-handoff edges**: a `workspace: repo` stage that consumes a predecessor's repo state names its producer(s) with **`repoFrom:`** — scalar or **list** (`repoFrom: [implement, remediate-ci]` = "the run branch head as of the most recent listed producer that executed"); the compiler rejects the undeclared chain (WF022) and enforces list *coverage* (§4). Generalizes #2861 from cross-OS to cross-runner (PO-D6; delivery decisions 001/002) | The run branch is the one implicit, untyped stage-to-stage channel (`docs/stage-contract.md` "a run's stages share one branch"); mode 3 makes the silent chain a silent *loss* (a node whose mirror lacks the commits provisions a pristine branch — `internal/worktree/worktree.go:50-72`). `inputsFrom` is the precedent for declared edges. The list form exists because CI-repass lanes create true fan-in (goobers/implementation's `local-ci ← {implement, remediate-ci}`) that a scalar cannot express |
| D12 | **Migration rules** (`goobers fix --to 3.0`): `requiredCapabilities` → `runsOn.capabilities`; `os=<goos>` tokens → `runsOn.os` (`darwin` → `macOS`); any remaining `os=*` token in a 3.0 document is **rejected** (CAP004) | One document, one vocabulary — the mechanical closure of the #659 drift hazard (PO-D2) |
| D13 | **DSL 1.4 is dropped** in the release that ships 3.0; the binary carries exactly two interpreters, 2.0 (frozen) and 3.0. A missing `dslVersion` becomes a **hard error** in the same release | 1.4 is already deprecated with `unsupportedAfter v0.5.0` (`internal/supportmatrix/supportmatrix.go:44-110`); the missing-pin default is 1.4 (`api/validate/validate.go:1101-1116`), so dropping 1.4 *is* the §8.3 cutover — defaulting to a version that no longer loads would be strictly worse than erroring |
| D14 | **2.0 lifecycle**: stays `supported` — the only backward-compat contract, local-runner-only, feature-frozen. Deprecation is **not scheduled now**; when it comes, the CI-enforced windows apply (≥1 released minor deprecated, ≥3 minor releases loadable, `supportpolicy.go:11-16`) and matrix transitions are staged across releases (append-only, tag-anchored) | PO-D0. The 1.4 window miscalculation (v0.2.0→v0.5.0) already reddened main once — state the clock rules up front |
| D15 | **Preview-feature fates**: the per-gaggle sandbox override (`featureGaggleSandbox`, the sole remaining 2.0 preview feature) does **not** carry into 3.0 as itself — it folds into the restrictions model (PO-D7); external call-out **promotes to 3.0 GA**. Both stay frozen preview in 2.0 | `internal/workflow/v_next/features.go:843-888`; external-call-out-stages.md D4 chose 2.0-preview precisely to avoid a minor bump — 3.0 is the bump |
| D16 | `run.network: none` (2.0, deterministic stages) migrates to `runsOn.restrictions: [network:none]` in 3.0 | One restrictions model, not two network surfaces (PO-D7 "restrictions name effects"). Enforcement on the `self` runner keeps today's executor mechanisms (`internal/executor/network_linux.go` et al.) |
| D17 | **Admission is three checkpoints** (PO-D4): apply/validate constraint solve (error iff `runners:` declared — the #3497 fix; warning otherwise), bounded fail-fast at dispatch, and boot that **never kills the daemon** (#2860 implemented). New codes join the CAP and (new) RNR families | §5. The static check and the dispatch check must stay mirrors — CAP003 exists because divergence produces configs that validate but never schedule |
| D18 | **Interpreter mechanics**: 3.0 is a new copy-forward interpreter package with its own feature registry and golden fixtures; supportmatrix gains a 3.0 entry staged across releases; `internal/dslmigrate` gains the 3.0 target; the agent-toolkit manifest `dslVersions` surface follows | The shipped multi-version machinery (`internal/workflow/compile.go:237-262` interpreterForVersion; `supportpolicy.go` append-only history) is designed for exactly this — §8 |

---

## 2. The stage surface: `runsOn`

```yaml
dslVersion: "3.0"
tasks:
  build:
    capabilities: [repo:push]            # credential grants — UNCHANGED (closed registry)
    runsOn:
      os: linux                          # validated enum: linux | windows | macOS
      cpu: 2000m                         # k8s quantity strings, verbatim
      memory: 4Gi
      disk: 20Gi
      capabilities: [go@1.26, make, gcc] # open tag set — today's requiredCapabilities, moved
      restrictions: [network:allowlist]  # closed effect list, vocabulary owned by PO-D7
    run:
      kind: make
      target: ci
```

Every field is optional, including the whole block (D3). Semantics per field:

- **`os`** — enum `linux | windows | macOS`. Unset = no OS requirement; placement prefers
  and will wait (bounded, §5 checkpoint 2) for a Linux-class runner when one is in the
  inventory. `macOS` on a cloud instance fails validation — Azure offers no macOS substrate;
  that is the system working as intended (PO-D1). `goobers fix` maps the `os=darwin` token
  to `macOS` (canonical spelling is the product name, not GOOS).
- **`cpu` / `memory` / `disk`** — Kubernetes quantity grammar, verbatim, no unit
  reinterpretation. These are *minimums*: in mode 3 they become the pod's resource
  requests; limits come from the matched runner's `provides:` ceiling, never from the
  stage. On modes 1/2 they are advisory: `goobers validate` warns (RNR004) when the `self`
  runner's declared ceiling cannot cover a stage minimum, and never errors.
- **`capabilities`** — the open toolchain tag set, exact-string set membership against the
  matched runner's `provides.capabilities`, unchanged grammar
  (`internal/runnercap/runnercap.go` tokenPattern). Not a place for `os=*` tokens: CAP004
  rejects them (D12). Exactly one token is product-interpreted — **`privilege=windows-admin`**
  (#3619, `runnercap.CapabilityWindowsAdmin`): the stage needs the Windows container's
  administrator identity. It is a claim about what the substrate *offers*, not an isolation
  effect, so it is a capability and the closed restriction list is unchanged. Rules: legal only
  when the stage's effective `os` is `windows` (declared on the stage or by the gaggle floor —
  refused, never defaulted, anywhere else); places only on a runner whose
  `provides.capabilities` claims it (ordinary exact-match solve); the dispatcher stamps
  `windowsOptions.runAsUserName: ContainerAdministrator` on that pod and only that pod. Every
  other Windows stage pod is stamped `ContainerUser` explicitly — a class that *provides* the
  privilege still runs a stage that did not *require* it as `ContainerUser`, and a stage that
  requires it on a class that lacks it is refused at dispatch. Spelled with `=` because the
  colon namespace is reserved for derived tags an author cannot declare.
- **`restrictions`** — a stage may *require* a restricted runner (PO-D7: v1 closed list
  `network:none`, `network:allowlist`, `fs:readonly-except-workspace`, `tmp:ephemeral`,
  `env:default-deny`). Unknown token = error with suggestion (CAP005, the CAP002 idiom).
  Effects, never mechanisms. The enforcement design is the restrictions companion doc.
  **OS-conditional (restrictions doc D4, #3619):** a stage whose effective `os` is `windows`
  may require only `tmp:ephemeral` and `env:default-deny` — the effects Windows can bind;
  `network:none`, `network:allowlist` and `fs:readonly-except-workspace` on a Windows-placed
  stage are refused at validate (CAP005), on a Windows runner entry at instance load, and at
  pod render, all through one predicate (`runnercap.DeclarableOnWindows`).

**Gaggle level.** `gaggle.spec.requiredCapabilities` migrates to a gaggle-level `runsOn`
carrying `os`, `capabilities`, and `restrictions` only (no quantities). It merges into
every stage as a floor: capabilities and restrictions union; an os conflict between gaggle
and stage is a compile error, never a silent override. Gaggles remain unversioned (#3297)
and resolve at `NewestSupported()`; the merge rule therefore activates only for gaggles
paired with 3.0 workflows (see open point 2).

**Base contract and derivation (D6, D7).** Authors of tier-1/2 workflows write no `runsOn`
at all and lose nothing:

- Every runner implicitly provides the base runner contract — goobers binary,
  stage-contract environment, network to provider endpoints, credential delivery. It is
  not spellable as a tag.
- Builtin stages (backlog-query, push-branch, merge-pr, …) derive their placement needs
  from stage identity via the providerstage manifest. **Prerequisite**: the manifest gains
  DSL-version linkage (per-entry since-version/support-level) before 3.0 ships — it is
  unversioned today and shared by both interpreters, so any Goobernetes-era requirement
  change would hard-break 2.0 workflows with no window (D7).
- Agentic stages derive `harness:<name>` from the goober's existing `harness:` field —
  declared once, never re-typed per stage. This derivation is what lets harness-less
  runner images exist (PO-D8's minimal `goobers-base`).
- `sh`/`make` stages derive `run:shell` (colon-namespaced like `harness:<name>` so the
  derived spelling lives outside the author token grammar — an author-declared plain
  `shell` capability is an ordinary token, never a derived tag; spelling owned by
  `internal/runnercap.DerivedShellTag`).

Derived requirements merge with declared ones by union; a declaration can add to a derived
set but never subtract from it. Credentials remain strictly capability-gated: nothing is
materialized without a declared credential capability, derived or not.

**Gates (Goobernetes-E2E-Core decision 001, #3798).** Placement is a *stage* property, and
an agentic gate is a stage: `gates[].runsOn` carries the identical block a task carries —
`os`, `cpu`/`memory`/`disk`, `capabilities`, `restrictions` — with the identical gaggle-floor
merge, and the reviewer derives `harness:<its goober's harness>` by the same rule an agentic
task does. Three rules are gate-specific:

- **Only agentic gates are placeable.** An automated gate is a pure function over its inputs
  and a human gate pauses for a portal decision; both evaluate in the daemon/control plane by
  definition, so `runsOn` on either is a compile error (WF023), never a silently ignored block.
- **A placed gate declares `cpu` and `memory`.** The gaggle floor deliberately carries no
  quantities, and an agentic review is the most expensive stage class in a lane; inheriting the
  floor's capabilities with no envelope would be a silent under-provision. A gate `runsOn`
  without both is a compile error (WF023), not a default — default-to-self was rejected because
  it makes "did my gate place?" invisible in the yaml.
- **An agentic gate without `runsOn` is unplaced.** It contributes no solver row, receives no
  pin, and evaluates in the control plane byte-for-byte as before the field existed; `goobers
  fix --to 3.0` never invents one. A placed gate inherits its subject's repo state (no
  `repoFrom` on gates in this ruling — a #3767 follow-up if a branching shape ever needs it),
  and there is no `goober.spec.runsOn`: a goober is shared by tasks and gates across
  workflows, so inheriting placement from it would make one goober place differently
  depending on who names it.

The solver input (`v30.StagePlacements`) lists every task in task order, then every placed
agentic gate in gate order; every consumer keys on the stage *name* (the run-start pin,
`bootstrap.PinStagePlacements`, looks each row up against the task and gate lists and never
by position — a gate is never ledger-touching). Parallels remain control-plane. The frozen
2.0 interpreter refuses `gates[].runsOn` through the router, exactly as it refuses the task
field. The engine/pod half — routing `evaluateGate` through the dispatch seam, a review mode
on the agentic kit, the surrendered verdict — is decision 001's rulings 7–8 and lands
separately.

**Until the engine half lands, a gate placement is declared but not honoured — and the
system says so.** `engine.evaluateGate` has no placement arm: an agentic gate always runs
`ActReviewGoober` in-process on the workflow's own queue, so a remote gate pin would be
manufactured and ignored and the reviewer would run with that host's OS, network and
envelope instead of the isolation the author declared. Three things hold the line:

- `goobers validate` emits **WF024** (warning) for every agentic gate that declares
  `runsOn`, naming the consequence.
- A gate placement **self cannot satisfy is refused at start, never run unrestricted.**
  For daemon-scheduled runs this is checkpoint 3 (§5) exactly as for a task: the workflow is
  marked refused (`workflow.refused`, `ReasonPlacementUnsatisfiable`) — deliberately so,
  because the daemon cannot honour the declared restrictions either. For `engine-start`,
  `bootstrap.PinStagePlacements` refuses a non-self gate pin with an error naming the runner
  and queue the gate would have pinned to.
- A gate placement **self satisfies pins self** (`LedgerTouching=false`) and evaluates
  in-process exactly as ruling 8's unpinned arm.

The CRD and JSON-schema descriptions of `gates[].runsOn` carry the same caveat. WF024 and
the `PinStagePlacements` refusal retire together when `evaluateGate` honours a non-self
gate pin.

---

## 3. The runner surface: `runners:` inventory

```yaml
# instance.yaml
schemaVersion: 2                  # new (D8); absent = 1 = pre-Goobernetes schema
runners:
  - name: self                    # the daemon host itself
    host: self
    provides:
      os: linux
      cpu: 8000m                  # ceiling → pod/process limit
      memory: 16Gi
      disk: 100Gi
      capabilities: [go@1.26, make, gcc, node@22]
  - name: ci-linux
    host: ghcr.io/example/goobers-ci:v0.7.0   # image ref (or a deployment name)
    provides:
      os: linux
      cpu: 4000m
      memory: 8Gi
      disk: 60Gi
      capabilities: [go@1.26, make, gcc]
    restrictions: [network:allowlist]          # what this runner ENFORCES (PO-D7)
  - name: win-ci
    host: ghcr.io/example/win-runner:v0.7.0
    provides:
      os: windows
      cpu: 4000m
      memory: 8Gi
      disk: 60Gi
      capabilities: [dotnet@8]
    restrictions: [tmp:ephemeral]   # Windows may declare only tmp:ephemeral / env:default-deny (restrictions doc D4)
  - name: win-admin
    host: ghcr.io/example/win-runner:v0.7.0
    provides:
      os: windows
      capabilities: [dotnet@8, privilege=windows-admin]   # stages REQUIRING it run as ContainerAdministrator (#3619)
    restrictions: [tmp:ephemeral]
engine:
  hostPort: temporal.goobers-system:7233       # optional connection config, unchanged
```

- **Supersession with implicit self (D8).** An instance with no `runners:` treats the
  legacy singular `runner:` block as the implicit `self` entry — capabilities carry over,
  `envPassthrough`/timeout settings keep their current homes. Zero-change upgrade for
  every local install; an uber-runner providing the superset is legal and is the expected
  simple configuration (PO-D3).
- **`host`** is `self`, an image reference, or a deployment name; the kind set is designed
  extensible (PO-D3). A runner with a non-self host and no `engine:` block is a validation
  error (RNR002) — mode is *inferred from inventory shape*, `engine:` stays optional
  connection config exactly as shipped (`internal/instance/config.go:88-92`).
- **`provides`** quantities are ceilings (→ limits); `provides.capabilities` is the claim
  set matched exactly. **Claims are trusted in v1** (D10): a false claim degrades to a
  runtime error with a named diagnostic, never a silent misroute. One claim is
  product-interpreted and load-validated: `privilege=windows-admin` (#3619) is accepted only on
  a `provides.os: windows` entry — it names the `ContainerAdministrator` identity the
  dispatcher stamps for a stage that requires it — and is refused on any other OS.
- **`provides.shell`/`provides.harnesses`** are how a non-self runner satisfies the
  derived requirements above (#3513): trusted claims (D10) in the same sense as
  `provides.capabilities`, but a separate, closed, typed surface — the derived
  `run:shell`/`harness:<name>` spellings stay outside the author token grammar
  regardless of host kind (`internal/runnercap.ValidToken` rejects the colon), so
  this is not a reopening of that grammar. A self runner satisfies both implicitly
  and never needs to declare them.
- **`restrictions`** on a runner declare what it enforces; a stage requiring a restriction
  matches only runners that enforce it. Instance-mandate composition and strengthen-only
  (SEC-021) semantics are specified in the restrictions companion doc (PO-D7).
- **Restart-only, accept-and-pin (D9).** Editing the inventory requires a daemon restart in
  v1; in-flight runs finish against their pinned snapshot. This is the recorded answer to
  #3449 for config updates: no drain machinery.

---

## 4. Declared repo-handoff edges

2.0's model: a run's repo-backed stages silently share the run branch
(`goobers/<workflow>/<run-id>`) through the node-local mirror, and #2861's rule rejects
only the *cross-OS* undeclared chain (push-branch required at the boundary,
`internal/workflow/checks.go:52`). In mode 3 any two stages may land on different runners,
so 3.0 makes the silent chain inexpressible everywhere (PO-D6).

**Before — 2.0, implicit chain** (abridged):

```yaml
dslVersion: "2.0"
tasks:
  implement:
    run: { kind: agentic, workspace: repo }
    requiredCapabilities: [go@1.26]
  local-ci:
    run: { kind: make, target: ci, workspace: repo }   # silently sees implement's commits
  push-branch:
    run: { kind: push-branch }                          # first and only publish to origin
  open-pr:
    run: { kind: open-pr }
```

**After — 3.0, declared edges:**

```yaml
dslVersion: "3.0"
tasks:
  implement:
    run: { kind: agentic, workspace: repo }
    runsOn:
      capabilities: [go@1.26]
    # first repo stage of the run: creates the branch, no incoming edge
  local-ci:
    run: { kind: make, target: ci, workspace: repo }
    repoFrom: implement                # declared handoff edge
  push-branch:
    run: { kind: push-branch }
    repoFrom: implement    # the last COMMITTING producer — local-ci mutates its
                           # worktree but never advances the branch (decision 002)
    capabilities: [repo:push]
  open-pr:
    run: { kind: open-pr }
```

**Compiler rule (WF022), generalizing #2861:** a `workspace: repo` stage that executes
after a *producer* of the same run must declare `repoFrom` covering its upstream; an
undeclared chain is a compile error regardless of OS. A repo stage with no `repoFrom` and a
reaching producer is refused, not defaulted — only committed work crosses a stage
boundary, and now only *declared* continuity does.

**Producers, coverage, and computation (delivery decisions 001/002 — binding):**

- **A producer ("definition") is a stage that advances the run-branch ref** as observed on
  the transport (origin in mode 3; the local mirror in modes 1/2): agentic
  non-`repo-readonly` stages and the ref-advancing builtins (`rebase-pr`,
  `update-behind-pr` — the latter advances the ref provider-side, which counts).
  `push-branch`/`push-remediated` *publish* existing commits and are consumers only.
  Deterministic `make`/`sh` stages are non-producers by default, with an explicit
  task-level opt-in for a genuinely committing script. Uncommitted worktree mutation is
  never a definition — the stage contract already guarantees it cannot cross a stage
  boundary in any mode, and branch transport physically carries commits only.
- **Coverage rule:** on every forward path reaching the consumer, the last producer before
  it must appear in the consumer's `repoFrom` list; an uncovered reaching producer, or a
  declared producer that can never immediately precede the consumer, is WF022.
- **Computation is reaching definitions** over the stage graph — gate-fail routing included
  as edges, fixed-point over cycles, a consumer's own prior attempts excluded (gate
  *repass* back-edges are attempt semantics, never repoFrom edges). It must **not** be
  implemented as DFS back-edge pruning: on the live tree that misclassifies
  `ci-gate --fail→ remediate-ci` (a loop re-entering through a *different* producer) as
  droppable, yields `[implement]` alone, and on every CI repass would fetch the head as of
  `implement` — silently discarding the remediation fix on exactly the path the lane
  exists to serve.
- **Enforcement, scoped by actor:** ref advances performed by the runner's own
  publish/recovery primitives are sanctioned by construction (push-branch's
  fetch+rebase-onto-remote race recovery per #3366; provider-side `UpdateBranch`). For
  authored stage work, the runtime records the branch head before/after every non-producer
  repo stage, and any advance is a fail-closed named error directing the author to declare
  producer-ness — never a silent drop, and never a spurious failure under push contention.

**Transport semantics.** The edge declares continuity; the runtime owns transport. At a
declared edge in mode 3, the runtime pushes the run branch to origin when the producing
stage completes and the consuming stage's fresh worktree fetches it
(`RequireExistingBranch`/`AcquireRemoteBranch` provisioning). Modes 1/2 are unchanged:
same declaration, no push, same-host mirror continuity — byte-identical behavior. A
cluster-internal git remote is a deferred optimization behind the same contract (PO-D6).
`push-branch` survives as the author-visible *semantic publish* (the branch a PR opens
from); edge pushes are transport to the run branch, not a publish. #2861's cross-OS rule is
subsumed: with every chain declared, the unsafe transition it rejected cannot be written.

The migrator computes `repoFrom` lists via the same reaching-definitions analysis and
inserts them automatically; it refuses only where coverage cannot be computed, with a named
diagnostic. On the live cloud tree this yields zero refusals; the canonical 14-workflow
fixture table (commit reading) and its continuity-reading contrast table are frozen as
migrator acceptance fixtures, with `goobers/implementation`'s
`local-ci → [implement, remediate-ci]` as the discriminator separating a correct
implementation from the naive one (delivery decisions 001/002).

---

## 5. Admission: three checkpoints (PO-D4)

1. **Apply/validate** — full per-stage constraint solve (stages × runners): os, quantities
   (≥ against ceilings), capabilities (set membership), restrictions (enforcement match).
   Severity: **error when a `runners:` inventory is declared; warning otherwise.** This
   fixes the #3497 severity trap for the declared case — today CAP003 is a warning
   (`api/validate/validate.go:96-106`) while the identical condition kills the daemon at
   boot, so exit-code-gated pipelines merge un-bootable configs.
2. **Dispatch** — a bounded fail-fast (schedule-to-start class) remains the reality
   backstop: apply-time truth rots (spot eviction, scaled-to-zero pools).
   Capability-unsatisfiable is an apply-time error; capacity-exhausted is a bounded, named
   runtime failure. The bounded wait is also where D3's "prefer and wait for a Linux-class
   runner" lives.
3. **Boot never kills the daemon** — #2860's decided-but-unimplemented ruling ("refusing
   one run is proportionate; refusing to start is not") is implemented as part of this
   work: an unsatisfiable workflow is a refused run with a diagnostic, not a dead instance.

The apply-time solver and the dispatch-time check must be one shared implementation (the
CAP003/scheduler mirror lesson: divergence = configs that validate but never schedule).

**Validation codes.** New codes are stable `WarningCode` IDs joining the existing const
blocks (`api/validate/validate.go:39-209`; severity strictly error|warning; the
`SEVERITY[ CODE] Scope: Message` prefix is pinned by #2025):

| Code | Severity | Condition |
| --- | --- | --- |
| RNR001 | Error iff `runners:` declared; else Warning | No declared runner satisfies a stage's `runsOn` (os / capabilities / restrictions) |
| RNR002 | Error | Runner has a non-self `host` but the instance declares no `engine:` |
| RNR003 | Error iff `runners:` declared; else Warning | A stage quantity minimum exceeds every runner's declared ceiling |
| RNR004 | Warning (always) | Local mode: resource minimums advisory — the `self` ceiling cannot cover a stage minimum |
| RNR005 | Warning (always) | A 3.0 stage's resolved eligible runner set excludes every self entry, but its command or built-in kind needs the daemon's instance root (decision 003 ruling 3) |
| CAP004 | Error | An `os=*` token appears anywhere in a 3.0 document (D12) |
| CAP005 | Error | Unknown restriction token, with did-you-mean suggestion; or a restriction a `windows`-placed stage cannot require (`network:none`, `network:allowlist`, `fs:readonly-except-workspace` — restrictions doc D4, #3619) |
| WF022 | Error | Undeclared repo-handoff chain (§4) |
| WF023 | Error | `runsOn` on a non-agentic gate, an agentic gate `runsOn` without `cpu` and `memory`, or one without an `agentic:` block naming its reviewer (§2 Gates, decision 001) |
| WF024 | Warning | An agentic gate declares `runsOn` while no execution path honours a gate placement (decision 001 rulings 7–8 unlanded); a placement self cannot satisfy is refused at start (§2 Gates) |

The structural `runsOn` problems (invalid os enum, malformed quantity, gaggle-vs-stage OS
conflict) report under WF010 like the other admission findings; so does the
`privilege=windows-admin` coherence rule (#3619) — a stage or floor requiring the token whose
effective `os` is not `windows`.

CAP003 keeps its shipped meaning for 2.0 documents on inventory-less instances (frozen
interpreter, frozen severity). `instance.yaml`'s own untyped fail-first errors
(`config_validation.go:19-26`) remain for malformed inventory; RNR codes cover the
cross-surface solve, which `goobers validate` already does in one pass.

---

## 6. Migration and lifecycle

**`goobers fix --to 3.0` rewrite rules** (in `internal/dslmigrate`, per-rule and
deterministic):

1. `dslVersion: "2.0"` → `"3.0"`.
2. Task and gaggle `requiredCapabilities` → `runsOn.capabilities` (grammar unchanged).
3. `os=<goos>` tokens are removed from the tag set and become `runsOn.os`
   (`os=linux`→`linux`, `os=windows`→`windows`, `os=darwin`→`macOS`). Two different
   `os=*` tokens on one stage: migrator refuses with a named diagnostic (was already
   unsatisfiable). After migration, any remaining `os=*` token is rejected (CAP004) —
   bare `os=*` cannot be carried into 3.0.
4. `run.network: none` → `runsOn.restrictions: [network:none]` (D16).
5. `repoFrom` edges computed as reaching definitions over the stage graph (§4); refusal
   only where coverage cannot be computed.
6. External call-out surface rewrites preview spelling to GA spelling if they differ
   (expected: none); the per-gaggle sandbox override is rewritten into the restrictions
   surface per the restrictions companion doc, or refused with a pointer when no
   equivalent exists yet.

The migrator's output must validate clean under 3.0 with zero manual edits for every
shipped and reference workflow (acceptance §9). Note `goobers fix --write` reformats YAML
wholesale; migration diffs will be larger than their semantic content.

**Version lifecycle:**

- **1.4 — dropped** (D13). The v_current interpreter package is removed; a 1.4 document
  gets DVL030 (unsupported), which names `goobers fix`. Missing `dslVersion` becomes a
  hard error in the same release — the §8.3 cutover, resolved.
- **2.0 — frozen, supported, local-only** (D14). Fully supported on the local runner
  indefinitely; never learns `runsOn`, `runners:` awareness, or handoff edges. 3.0 also
  fully supports local — 2.0 is compatibility, not the local dialect. Eventual EOL is
  explicitly not scheduled now; when scheduled it follows the CI-enforced windows
  (`MinimumDeprecatedMinorReleases=1`, `MinimumSupportWindowMinorReleases=3`) with matrix
  transitions staged across releases — the append-only history is validated against the
  latest reachable tag, so the 3.0 entry and any 2.0 lifecycle change cannot land in one
  PR.
- **Preview features** (D15): `featureGaggleSandbox` stays frozen preview in 2.0 and folds
  into the restrictions model in 3.0 — it does not appear in the 3.0 feature registry
  under its own name. External call-out promotes to GA in 3.0 (registry entry GA, no
  `allow-preview-features` gate) and stays preview in 2.0.

---

## 7. Supersession record

- **#659** (locked Lead ruling, 2026-07-25: "platform is the `os=<goos>` token inside
  `requiredCapabilities`, explicitly not a bespoke field; unlabeled ⇒ linux; fail-fast")
  is **formally superseded** by PO-D2 (2026-08-22), encoded here as D2/D3/D12:
  - *Token → field*: `runsOn.os` is a validated enum. The two-vocabularies drift hazard
    the ruling guarded against is closed by CAP004 — an `os=*` token cannot exist in a
    3.0 document, so the vocabularies cannot coexist.
  - *Unlabeled ⇒ linux → explicit-complete*: os-unspecified now means no requirement,
    with a Linux-preferring placement policy owned by the system (D3) — same practical
    outcome on mixed inventories, honest semantics on single-OS ones.
  - *Survives*: fail-fast at dispatch, bounded, naming the unmatched requirement
    (checkpoint 2). The ruling's routing mechanics (platform-suffixed queues) are an
    implementation detail of the runner-architecture doc, not DSL surface.
- **#2861** is **generalized, not reversed** (§4): the explicit-push contract's intent —
  unpushed worktree state never silently crosses hosts — now holds for *every* runner
  boundary via declared edges, and the runtime owns the push at declared edges in mode 3.
- Per PO-D12, related issues are folded/closed by the umbrella (#2889), not re-filed here:
  #1529 and #1087's capability-model questions are answered by D4/D5; #1494 stays rejected
  (per-task `run.image` never returns; runner-level images supersede — `host:` on the
  runner, §3); #2860 is implemented by checkpoint 3; #3497 by checkpoint 1.

---

## 8. Interpreter mechanics

Landing 3.0 touches the shipped multi-version machinery, all of it designed for this
(`docs/design/dsl-version-lifecycle.md` §3.4, epic #860):

- **A new copy-forward interpreter package** beside the frozen 2.0 one (today
  `internal/workflow/v_current` = 1.4, `v_next` = 2.0; 1.4's package is deleted in the
  same release — final naming is open point 1). The 2.0 package gains frozen-patch guards;
  `interpreterForVersion` (`internal/workflow/compile.go:237-262`) and the
  versionedInterpreter function tables gain the "3.0" arm.
- **A per-version feature registry** with golden fixtures: `runsOn`, `repoFrom`, and the
  restrictions surface register as 3.0 features; external call-out registers GA;
  `featureTaskRequiredCapabilities`/`featureGaggleRequiredCapabilities` do not exist in
  the 3.0 registry (the fields are gone).
- **Supportmatrix**: append the 3.0 entry; stage lifecycle transitions across releases
  under `ValidateSupportPolicy` (append-only, checked against the latest tagged release).
- **`internal/dslmigrate`**: the `--to 3.0` target (§6).
- **Schema ceremony**: closed JSON schemas for the new blocks, CRD regen (`make manifests`
  in the same PR — the manifests-diff gate), agent-toolkit manifest `dslVersions`
  (`api/schemas/agent-toolkit-manifest.schema.json:32-35`), docs/man/feature-matrix regen.
  CRD CEL cost scales with array cardinality (#3168): `runsOn.capabilities` and
  `restrictions` carry explicit `maxItems` (open point 7).
- **providerstage manifest versioning** (D7 prerequisite): per-entry version linkage so
  3.0-era requirement changes are invisible to the frozen 2.0 interpreter.

---

## 9. Acceptance criteria (falsifiable)

1. Every shipped and reference workflow, run through `goobers fix --to 3.0`, validates
   clean under 3.0 with zero manual edits; run on a single-runner instance, its journal
   conformance view diffs empty against the 2.0 run of the unmigrated source.
2. A byte-untouched 2.0 workflow compiles and runs identically before and after the 3.0
   release (frozen-interpreter guard stays green).
3. A 3.0 document containing `os=windows` in `runsOn.capabilities` is refused with CAP004;
   a 1.4 document is refused with DVL030 naming `goobers fix`; a document with no
   `dslVersion` is a hard error.
4. `goobers validate` exits 1 on a config tree whose declared `runners:` inventory cannot
   satisfy some stage (RNR001/RNR003), and exits 0-with-warning on the same workflow
   against an inventory-less instance — the #3497 class is closed for the declared case.
5. An instance with only the legacy singular `runner:` block loads as the implicit `self`
   entry and behaves byte-identically to the previous release; the same file plus
   `schemaVersion: 2` and a second runner entry loads on the new binary and hard-fails on
   the old one (strict loading, by design).
6. An os-unspecified stage on a mixed Linux+Windows inventory places on a Linux-class
   runner; on a Windows-only instance it runs there; a `runsOn.os: macOS` stage on a cloud
   instance fails validation.
7. The 2.0-style implicit repo chain written in a 3.0 document is refused with WF022; the
   declared chain in mode 3 executes `implement` and `local-ci` on different runners with
   the run branch arriving via origin (D11 exit is part of the PO-D11 smoke).
8. A runner entry with `host: <image>` and no `engine:` block is refused with RNR002.
9. A booting daemon whose config contains one unsatisfiable workflow starts, serves every
   other workflow, and refuses that workflow's runs with a named diagnostic (#2860).
10. The migrator reproduces the frozen canonical fixture table exactly (set-normalized);
    in particular `goobers/implementation` yields `local-ci: [implement, remediate-ci]` —
    a `[implement]`-only result (the back-edge-pruning failure) is an automatic fail.

---

## 10. Open implementation points

Decided questions above are not re-opened; these are the points this document
deliberately leaves to implementation:

1. Interpreter package naming/layout after 1.4's removal (rename `v_next`→`v_current`
   and add a new `v_next`, or move to version-literal package names) — pick once,
   mechanical either way.
2. Gaggle-level `runsOn` activation timing: gaggles are unversioned (#3297) and resolve at
   `NewestSupported()`, which flips to 3.0 the moment the entry lands — the exact rule for
   a 3.0-resolved gaggle paired with a 2.0-pinned workflow needs a compile-time statement.
3. The providerstage manifest version-linkage shape (per-entry since-version vs per-version
   manifest snapshots) — funded prerequisite, designed in its own change.
4. `schemaVersion` numbering and the instance-config migration diagnostic text —
   coordinate with the instance/runner companion doc; the live drifted fork must be ported
   deliberately at rollout.
5. ~~Migrator edge-inference beyond linear chains~~ — **closed** by delivery decisions
   001/002 (list-valued `repoFrom`, reaching-definitions computation, commit reading,
   actor-scoped enforcement); encoded in §4.
6. Quantity canonicalization: whether validation normalizes equivalent spellings
   (`2000m` vs `2`) in diagnostics and digests, and whether the k8s apimachinery quantity
   parser is vendored or reimplemented.
7. Concrete `maxItems` for `runsOn.capabilities`/`restrictions` under the #3168 CEL cost
   budget, verified by the envtest install check.
8. The shared home of the constraint solver so checkpoint 1 and checkpoint 2 cannot
   diverge, and what `goobers validate --source-tree` does when the tree carries no
   instance.yaml (today it substitutes `instance.yaml.example`).
9. The bounded-wait default for D3's Linux-preferring placement (relationship to the
   existing schedule-to-start timeout) and its named diagnostic on expiry.
10. Windows `network:none` escape hatch (#2034's host-global env override) versus
    per-runner restriction declarations — reconciled in the restrictions companion doc;
    the DSL surface here is unaffected.
