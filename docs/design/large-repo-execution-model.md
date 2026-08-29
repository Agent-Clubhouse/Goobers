# Large-repo execution model (#2063)

Status: draft — for review, rev 2. Filed from the state-of-repo review 2026-07-30 + PO input
2026-07-31, hero scenario #3. Rev 2 incorporates a second round of PO rulings (2026-07-31,
recorded in §11): the headline recommendation is now **large-repo mode** — a pinned
persistent workspace with fully-serial execution — with the clone-strategy tiers retained
as secondary, optional levers. First deliverable per #2063: this design doc. No children
are filed until it lands — PM (`#control`, 2026-08-01) files and dispatches from it.

## 0. Verdict up front

**The dominant cost in the hero scenario is not clone strategy — it is the per-(run, stage)
worktree materialization model itself.** Every stage attempt today materializes a full
working tree via `git worktree add` and eagerly destroys it afterwards (§3.1). At 10+ GB of
working tree on Windows/NTFS, that is minutes-to-hours of file churn per stage — and it
also discards all incremental build state, forcing a cold build of a monolith that is
already hard and slow to build. No amount of clone-strategy tuning fixes that, and the
cheap, obvious clone optimizations (blobless partial clone, sparse checkout) have a real,
documented failure mode for exactly this repo profile: **bespoke build systems that walk,
hash, or glob the full tree break or silently degrade under partial materialization** (§4).

So this design has two parts:

1. **Large-repo mode (§5) — the primary recommendation, PO-ruled required.** A per-repo
   preset that swaps the execution model: one pinned, persistent workspace per repo per
   node; fully serial (a whole-run lease on the workspace, no concurrency in any stage
   touching the repo); **no worktrees at all** while the mode is on; build state persists
   between runs as an explicit, opt-in contract; raised timeouts; loud operator-visible
   recovery. The preset flips every toggle at once "in exchange for getting it to work";
   each toggle is individually relaxable afterwards by operators whose repos tolerate more.
2. **A per-repo clone/checkout tier policy (§6) — secondary, optional levers**, ordered
   cheapest-and-safest first, escalated only on evidence per repo, never self-promoted by
   the runner. This is where sparse checkout and blobless clone live, for the repos that
   can be *proven* to tolerate them.

§7 answers #2063's six design questions against both. §9 covers the Windows operational
realities (antivirus, file locking, environment-mangling build systems) that any execution
model for this repo class must survive. §8 is explicitly about not foreclosing the
cloud/remote-execution dovetail the PO flagged, without building it now.

## 1. Grounding: the hero scenario, restated

Per #2063 and PO input 2026-07-31 (elaborated by the PO across two rounds, 2026-07-31):

- **10+ GB of legacy code — and that is *working tree* size on disk** (PO-confirmed:
  file-explorer "get info" measurement), not history size. History for a decades-old repo
  can be a large multiple of that, which matters for mirror strategy (§5.2): the two must
  not be conflated. Concretely: .NET Framework and/or C++, not a modern Go/TS-style repo.
  Deep, namespace-mirrored directory trees (C#) and deep template/header include graphs
  (C++) are exactly the shape that eats Windows' 260-char `MAX_PATH` budget (confirmed as a
  live risk in this codebase already — §3.4).
- **Git-hosted, and git only.** PO ruling: no other source providers will be supported —
  TFVC (common in legacy .NET shops on ADO) is explicitly out of scope for Goobers, not a
  future epic. This doc assumes git throughout, deliberately.
- **Bespoke, possibly undocumented build systems that "get pretty gross."** Custom scripts,
  not a single blessed `dotnet build`/`msbuild` invocation — and per PO, they mutate the
  environment and machine state in bad ways (PATH surgery, global temp dirs, potentially
  registry/GAC/COM). This is the load-bearing constraint for this whole doc: we cannot
  assume the build only touches files under a knowable path cone, or that it tolerates
  on-demand blob fetch latency, or that it even declares its own inputs, or that two builds
  can safely share a machine. §5.1 and §9.3 state exactly what Goobers isolates and what it
  does not.
- **Hard to build and test locally, i.e. slowly.** Cold builds of the monolith are long;
  stage/run timeout defaults sized for modern repos will not survive contact (§5.4). This
  is also why discarding incremental build state every stage (today's model) is
  independently disqualifying, even if checkout were free.
- **Windows, very likely.** Nothing in this codebase currently probes or enforces a
  path-length ceiling — see §3.4. .NET Framework/C++ toolchains are also exactly the
  toolchains most likely to write their own deep `obj`/`bin`/generated-header paths on top
  of whatever budget the checkout scheme leaves them — and to hold file locks that break
  eager teardown (§9.2).
- **Hundreds of concurrent users against the same ADO instance.** This compounds with
  #2061 (ADO hero epic): ADO's provider-side quota/rate-limit handling in this codebase is
  strictly weaker than GitHub's today (§3.5), so large-repo traffic patterns (fetch volume,
  blob backfill storms) are a direct load-multiplier on an already-thin mechanism.
- **The eventual answer is not "one machine, one disk, forever."** The PO explicitly wants
  this design to leave room to dovetail with remote/cloud execution — and named a
  **dedicated host** per mega-repo as the intended deployment shape — so load and size can
  be shed off the local runner over time. Per PO ruling this is carried later, not scoped
  here: it needs its own generic design (it could be almost any shape, possibly custom Go
  code). §8 addresses it as a non-goal-now / don't-foreclose-it-later seam, consistent with
  `docs/design/v2-cloud-scale.md`.

## 2. Relationship to `docs/design/v2-cloud-scale.md` §3 (Workstream B)

V2's cloud-scale design already staked out a layered large-repo cache strategy —
`docs/design/v2-cloud-scale.md:140-179` (B0–B6) — and B6 (authenticated clone) and an
opt-in form of B1 (blobless mirrors) are **already implemented** (§3.1). This doc does not
redo that layering; it answers #2063's questions against it and **corrects two
assumptions**:

- V2-cloud-scale.md's B1 write-up calls partial-clone mirrors "transparent to worktrees."
  For a well-behaved repo that's true. For the bespoke-build-system hero scenario this doc
  is scoped to, it is **not** — see §4. This doc narrows B1 from "the default layer
  everyone gets" to "a lever some repos can opt into once proven safe for that repo."
- B4 (worktree pooling) assumed the fix for materialization cost is *pooling* ephemeral
  worktrees. Large-repo mode (§5) is a stronger and simpler statement of the same idea for
  the local runner: a pool of exactly one, held forever, serialized by lease — no pool
  manager needed. B4 proper and B5 (baked workspace snapshots for tier-3 pods) remain
  V2/tier-3 concerns — they assume a scheduler and pod substrate this design doesn't have
  yet. §8 calls out what NOT to bake in so B4/B5 remain buildable later without a redesign.
- B3 (reference/alternates cache) is not yet implemented; this doc keeps it as a real but
  **demoted** lever (§7.2): under large-repo mode there is one pinned workspace and one
  mirror per repo per node, so there is nothing for alternates to dedupe on the hero path.
  It retains value for the many-normal-gaggles-per-node case.
- B2 (sparse checkout) is schema-declared but inert today (#649); this doc's per-gaggle
  scoping answer (§7.3) is consistent with B2's shape but adds the build-validation gate
  V2's one-line mention didn't need to specify.

## 3. Current state (grounded in code)

### 3.1 Clone/mirror mechanics — shared mirror, but full materialization per stage

There is **one mirror per distinct repo URL, reused across every run**, not a fresh clone
per run. `Manager.WorkingCopy` (`internal/worktree/manager.go:260-327`) clones once
(`git clone --mirror`, `manager.go:272-276`) into a content-addressed path keyed by a
16-hex SHA-256 prefix of the repo URL (`repoKey`, `manager.go:195-198`), then every
subsequent run does `git fetch --prune origin` against that same mirror
(`manager.go:307-317`). Concurrent runs against the same repo serialize on a per-repo-key
mutex (`manager.go:216-227`) rather than racing. `AdditionalRepos` (read-only reference
repos, MGV-11/#1286) reuse the identical mechanism keyed by their own URL
(`internal/runner/run.go:3042-3075`).

Note what `--mirror` implies for a decades-old repo: it fetches **all refs** — on ADO that
includes PR merge refs, which balloon over decades. With working-tree size PO-confirmed at
10+ GB, total mirror size is an unknown multiple of that and must be measured per repo,
not assumed (§5.2 narrows the refspec for large-repo mode).

Blobless partial clone (`--filter=blob:none`) already exists as an **opt-in, new-mirror-only**
flag (`WithPartialClone`, `manager.go:127-148`): it narrows the fetch refspec and adds the
filter on first clone, but an existing full mirror is never migrated, and nothing in
production code turns it on today (no config surface calls `WithPartialClone`). No
`--depth`/shallow flag is used anywhere.

**Per-run worktrees are `git worktree add` against the shared mirror** — never an
independent clone (`internal/worktree/worktree.go:133-312`) — but `git worktree add`
shares *objects*, not *materialization*: it writes the *entire working tree* to disk. The
granularity is one worktree per **(run, stage)** attempt, not per run
(`RunID: in.RunID + "-" + stageName`, `internal/runner/run.go:3013-3016`). Teardown is
eager and synchronous at the end of every stage attempt (`worktree.go:535-586`,
`run.go:2784`/`3148`), with a separate crash-orphan reaper (`Manager.Reap`) that runs once
at daemon startup only (`internal/worktree/reap.go:75-100`, `cmd/goobers/up.go:443-466`) —
**there is no periodic sweep of live worktrees**, which matters for §3.3.

At hero scale this model pays, per stage attempt: a full 10+ GB tree write on NTFS
(amplified by antivirus scanning of every file, §9.1), then a full tree delete (which can
*fail* under Windows file locks, §9.2), and — because the tree is destroyed — a **cold
build** next stage: all `obj`/`bin`, package caches, and generated code are gone. This is
the cost §5 removes.

### 3.2 Gaggle model — repo-scoped today, path-scoped only on paper

A gaggle (`api/v1alpha1/gaggle_types.go:9-69`) targets exactly one `Project` repo plus
optional whole `AdditionalRepos` — there is no runtime notion of a gaggle owning a
*subdirectory* of a repo today. The schema already anticipated this:
`RepoRef.Checkout.Sparse []string` (repo-relative path cones,
`api/v1alpha1/common.go:62-69`) exists end-to-end in the API type and round-trips, but its
own doc comment says it plainly: **"Accepted but not honored by the local runner yet:
declaring it is inert and surfaces a VER003 compatibility warning at validate time"**
(`common.go:54-58`, enforced `api/validate/validate.go:754-777`). It's stripped from the
task envelope before dispatch (`internal/engine/engine_test.go:341-354`). Tracked as #649.
No `git sparse-checkout` invocation exists anywhere in the codebase (verified by grep).

### 3.3 Disk-pressure/retention — real bug, real prerequisite, not this doc's scope

#2052 ("worktree retention is startup-only and 'enabled: true' with default limits is a
silent no-op") is confirmed accurate against current code: `pruneConfiguredRetention` runs
once at startup only (`cmd/goobers/up.go:466`, no ticker — contrast telemetry retention's
`up.go:663-675`), and the age/byte-cap prune passes both silently skip when their limit is
the Go zero value (`internal/worktree/retention.go:141`, `152`), which is `RetentionConfig`'s
default (`internal/instance/config.go:575-580`). A retained worktree whose owning run
journal was already deleted by independent telemetry retention becomes permanently
unprunable (`cmd/goobers/retention.go:227-231`).

**This is Dev-7's assigned fix (`largerepo/2052-retention-periodic-sweep`), landing
independently of this design doc per PM's direction — this doc treats it as a hard
prerequisite for the acceptance gate (§10), not as something to re-scope here.** One new
requirement lands on it from this doc: **a pinned large-repo workspace must be exempt from
retention sweeps by construction** (§5.5) — it is long-lived state, not a leaked worktree,
and a sweeper that deletes it has destroyed hours of build-state warm-up.

Disk usage IS measured today, but only at lifecycle boundaries (worktree create/teardown/
housekeeping — `internal/worktree/usage.go:55-90`), emitted as OTel span events
(`internal/telemetry/workcopy.go:26-80`) when a telemetry client exists. There's no
continuous gauge — worth keeping in mind for whatever dashboard/alert eventually watches
the disk ceiling in §10's acceptance gate.

### 3.4 Path-length budget — a real, currently-unenforced risk

No `MAX_PATH` preflight exists anywhere; the only mitigation is `core.longpaths=true` set
on every mirror (`manager.go:371-377`), which only helps *git itself*, not build tooling,
harness scratch writes, or anything else touching the tree. Goobers' own fixed path
overhead before a single file of the target repo is counted comes to roughly **131
characters** on a realistic Windows layout (`C:\Users\<user>\goobers` root + gaggle name +
`\workcopies\<16-hex key>\runs\<32-hex RunID>-<stage name>`), leaving on the order of
**~129 characters** of the 260-char `MAX_PATH` budget for the target repo's own relative
paths — before a single directory of a deep C++ template-header tree or a namespace-mirrored
.NET Framework tree is counted. `AdditionalRepos` checkouts eat further into this with a
longer `-ref-<name>` suffix (`run.go:3065-3068`).

The one length lever available and unused: `opts.RunID` (32-hex trace ID + `-` + stage name,
up to ~50 chars) is the largest Goobers-controlled contributor to that fixed prefix, and
nothing hashes or truncates it (`internal/worktree/worktree.go:118-127` only validates
path-traversal safety, never length). Large-repo mode side-steps this entirely for the hero
path — a pinned workspace has a *stable, short* directory name with no per-run component
(§5.1) — but the hashing fix (§7.4) still pays for every normal repo.

### 3.5 ADO provider quota state — weaker than GitHub's, and this scenario stresses it

ADO's rate-limit handling is a bare per-HTTP-call 429/Retry-After backoff loop with **zero
memory across calls**, let alone across processes (`providers/ado.go:759-812`, local
variables scoped to one `send()`). GitHub, by contrast, has an in-process shared quota
ledger (`internal/localscheduler/providerquota.go`) instantiated once per daemon process
and threaded through every scheduler call site, plus a disk-backed cross-process ETag cache
(`cmd/goobers/apireadcache.go`) — neither of which ADO has any equivalent of. This confirms
#2061's stated "per-process in-memory" framing, and for ADO the gap is worse: it doesn't
even have the single-process ledger GitHub has. Every deterministic stage runs as its own
OS subprocess (`internal/executor/shell.go:366`), so two stages of the *same run* on the
*same host* don't share rate-limit state either.

## 4. Why partial materialization has to be opt-in — the tension, with evidence

The obvious instinct — "blobless partial clone + sparse checkout, make repos small and
fast" — is real V2 prior art (§2) and is the right call for a well-behaved modern repo. It
is a documented risk for exactly this hero scenario:

- **Partial clone's own docs call it "a tool for a very narrow case."** Commands that touch
  many blobs at once — `git blame`, full-tree diffs, and (critically) anything a bespoke
  build script does that resembles those patterns (hashing every file for a build
  manifest, walking the tree for a custom dependency resolver, a resource compiler globbing
  everything) — degrade badly under on-demand blob fetch: each missing blob is a
  single-object network round-trip with no delta compression, and shallow/blobless fetches
  push *more* computational load onto the server, not less. ([Git Partial and Shallow
  Clone](https://gist.github.com/stormwild/b7ae53f401ecf9f51f097a43d01be23b), [GitHub: Get
  up to speed with partial and shallow
  clone](https://github.blog/open-source/git/get-up-to-speed-with-partial-clone-and-shallow-clone/))
  Against an ADO instance already carrying hundreds of concurrent agents (§3.5), a blob
  backfill storm on first checkout of a 10GB tree is a real, self-inflicted load spike on
  the exact mechanism #2061 already flags as fragile.
- **Microsoft's own retreat from this problem is directly on point.** Microsoft built VFS
  for Git specifically for their largest Windows/.NET monorepos, then moved *away* from
  full filesystem virtualization toward Scalar + git's own **cone-mode sparse-checkout**,
  because virtualization has platform costs (it depended on kernel features Apple later
  deprecated) and, per their own writeup, VFS-for-Git-grade virtualization is warranted for
  only "a very small number of extremely large repos" (the Windows OS repo itself is the
  named example that still needs it). ([The Story of
  Scalar](https://github.blog/open-source/git/the-story-of-scalar/),
  [microsoft/scalar](https://github.com/microsoft/scalar)) The lesson that transfers here:
  **sparse checkout (path-scoped, not blob-lazy) is the lever that scales safely when
  ownership genuinely maps to a subtree — but it presupposes the repo's build doesn't reach
  outside the declared cone.** A bespoke, decades-old .NET Framework/C++ build is exactly
  the kind of repo where that presupposition is unverified and possibly false (project
  references crossing directories, relative `..\..\` paths in custom MSBuild targets,
  generated code checked in outside the "owning" area).
- **The prior art for this exact repo class is classic CI practice, not git-internals
  cleverness.** Azure DevOps' own self-hosted agents — what these customers run today —
  keep a **persistent working directory per pipeline across builds** (`clean: false`),
  doing incremental fetch + incremental build, precisely because re-materializing and
  cold-building a legacy monolith per run is untenable; Microsoft-hosted (ephemeral)
  agents are documented as losing exactly this benefit. ([Azure Pipelines
  agents](https://learn.microsoft.com/en-us/azure/devops/pipelines/agents/agents?view=azure-devops),
  [Azure Pipelines jobs &
  workspace](https://learn.microsoft.com/en-us/azure/devops/pipelines/process/phases?view=azure-devops))
  Large-repo mode (§5) is that model, adopted deliberately.

None of this means "never optimize." It means **the runner must not assume optimization is
safe by default for an unproven repo**, and must give an operator a way to declare what's
safe once it's been verified — which is what §5 and §6 are.

## 5. Large-repo mode: pinned workspace, fully serial (primary recommendation)

PO-ruled (2026-07-31): this is a required solution, not one option among peers. It is a
per-repo **preset** — one declaration ("large-repo mode") flips every toggle below at once,
making all the concurrency/workspace/hermeticity trade-offs "in exchange for getting it to
work." Every toggle remains an independent config knob, so operators can then **relax**
what their scenario tolerates (or tighten further); the preset just sets defaults that are
known-safe for the hero profile.

### 5.1 The pinned workspace

One long-lived working copy per repo per node, at a stable short path
(`workcopies/<repo-key>/pin` — no per-run path component, which also reclaims the entire
per-run share of the `MAX_PATH` budget, §3.4). Lifecycle per run:

- `git fetch` against the shared mirror (already incremental, §3.1), with the **narrowed
  refspec** of §5.2;
- `git checkout`/`git reset --hard` to the run's base ref;
- a **configurable clean policy** between runs — the knob, not a fixed answer:
  `none` (reset tracked files only; all ignored/untracked build state survives — the
  large-repo default), `ignored-safe` (`git clean -ffd`, untracked-but-not-ignored only), or
  `full` (`git clean -ffdx`, hermetic, what today's model effectively gives);
- stages — **including agentic stages** — run directly in the pinned workspace;
- run branches are created, committed, and pushed from it exactly as from a worktree today;
  no change to the PR/review flow.

**While the mode is on, no worktrees are created for this repo, at all** (PO ruling). Not
for agent stages, not for QA stages, not for `AdditionalRepos`-style convenience. The
pinned workspace *is* the execution surface. Config-level consequence: pinned mode and
per-stage worktrees are mutually exclusive for a repo, and validation should reject
contradictory combinations loudly (a VER-class error, not a silent preference).

Two consequences to state honestly:

- **Agents edit in a tree that contains build grime** (`obj/`, `bin/`, package caches,
  generated code). The customer repo's `.gitignore` hygiene becomes load-bearing: `git
  status`/diff surfaces are how run branches get built, and un-ignored build output would
  pollute them. An onboarding check ("is the tree `git status`-clean after a build?") is
  part of the mode's runbook (§10).
- **This is the serial model.** There is exactly one workspace; there is no concurrent
  execution against this repo on this node, by construction (§5.3). PO-accepted trade.

### 5.2 Mirror refspec: heads and tags only

Working-tree size is 10+ GB (PO-confirmed); history size is unknown and potentially a
large multiple. Large-repo mode narrows the mirror fetch refspec to `refs/heads/*` +
`refs/tags/*` (dropping ADO PR-merge refs and other ref noise that `--mirror` hoovers up,
§3.1), and treats **measured** mirror size as a per-repo onboarding datum, not an
assumption. Full history stays (agents benefit from `git log`/`blame`); shallow clone
remains off the recommended path — its server-side cost profile on repeated fetch is the
wrong trade against an already-loaded ADO instance (§4) — but is not banned as a
per-repo, evidence-gated override where history is truly pathological.

### 5.3 Serialization: a whole-run lease

Concurrency control is a **lease on the workspace held for the entire run** — not
per-stage. Interleaving stages of two runs through one dirty workspace would mix state
between runs in unobservable ways; the lease makes the serialization unit the run.
Queued runs wait (visibly — queue position surfaces in run status/portal, not as a silent
hang). The lease is shared across *gaggles*: N gaggles targeting the same large repo on one
node share the one pinned workspace and serialize behind the same lease — one 10 GB copy
per node, not N. (A per-gaggle pinned workspace is a valid future relaxation for operators
who prefer isolation over disk; not the default.) The existing per-repo-key mutex
(§3.1, `manager.go:216-227`) is the in-process seed of this; the lease generalizes it
run-long and crash-safe (a lease-holder that dies must not brick the repo — stale-lease
recovery goes through §5.6's reset path, never silent auto-steal).

Serial-per-repo is also the honest mitigation for build systems that mutate
**machine-global** state (§9.3): if only one build runs at a time on the node, PATH/env/
registry grossness can't collide with a sibling. A dedicated host per mega-repo (§8) is
the stronger version of the same statement.

### 5.4 Timeouts and the preset surface

Cold builds of the monolith are hours, not minutes. The large-repo preset raises stage/run
timeout defaults to monolith-scale values (and any watchdog/heartbeat tuned to "modern
repo" expectations moves with them). Per PO ruling the preset's shape is: **one switch
that defaults everything safe for this profile — pinned workspace on, whole-run lease
serial-only, monolith-scale timeouts, retention exemption (§5.5), path-length preflight
(§7.4) on, narrowed refspec (§5.2)** — with every one of those individually overridable
afterwards. Users can tighten (e.g. lower a timeout once incremental builds are warm) or
relax (e.g. re-enable concurrency for a repo that proves tolerant) without leaving the
mode.

### 5.5 Retention interplay

The pinned workspace is **exempt from retention/pruning sweeps by construction** — it is
deliberate long-lived state whose warm build caches are the point. #2052's periodic sweep
(prerequisite, §3.3) must recognize and skip it structurally (it lives outside the
per-run `runs/` namespace, which also makes the exemption mechanical rather than
policy-based). Its disk footprint is instead watched by the §10 gate's steady-state
ceiling: mirror + one workspace + build state, measured.

### 5.6 Recovery: explicit, loud, never automatic mid-run

Gross build state will eventually corrupt the workspace in ways `reset --hard` can't fix
(locked files, poisoned caches, tooling half-installs). The escape is an **operator
command** — `goobers workspace reset <repo>` — that tears down and re-materializes the
pinned workspace (full re-checkout, cold build accepted). After N consecutive run failures
on a pinned workspace, the runner *suggests* a reset loudly (run verdict/portal
annotation); it never resets automatically mid-run or between runs on its own — a
partially-rebuilt workspace silently swapped under an operator's feet is worse than a
failed run. This is the same fail-loud stance as tier promotion (§6): the runner never
takes materialization decisions on its own evidence.

## 6. Secondary levers: per-repo checkout tiers, escalate on evidence

For repos *not* in large-repo mode — or large-repo-mode repos whose operators later want to
relax toward concurrency — the per-repo tier policy stands. Each tier is strictly opt-in;
an operator declares a repo has been verified to tolerate a tier; the runner never
self-promotes a repo to a riskier tier on its own (PO-confirmed: operator-declared, no
automated probe-and-fallback — a partially-built worktree is worse than a slow one).

| Tier | Mechanism | Changes what the build sees? | Default for an unverified repo | Prerequisite |
|---|---|---|---|---|
| **P — pinned workspace** (§5, large-repo preset) | One persistent workspace, whole-run lease, no worktrees | **Yes — deliberately**: build state persists (non-hermetic by contract) | Only via the large-repo preset | Operator opts in; accepts serial + non-hermetic contract |
| **0 — full mirror + ephemeral worktrees** (today's default) | `git clone --mirror` + `git fetch --prune`, full worktree per (run, stage) | No — full clean tree, always | **Yes** | none |
| **1 — reference/alternates cache** (V2 B3) | Per-node object cache shared via `--reference`/alternates across gaggles targeting the same repo | No — still a full worktree, just fewer duplicate mirrors | Safe to default on once built (§7.2) | GC must fail closed while any dependent mirror exists (V2-cloud-scale.md:161) |
| **2 — sparse checkout (cone mode)** | Per-gaggle `checkout.sparse` path cones, materialized via `git sparse-checkout` (cone mode, not the slower non-cone form) | **Yes** — only declared paths exist on disk | Opt-in only, gated (§7.3) | A build-validation gate: the declared cone must be proven to build/test green before being trusted in production runs |
| **3 — blobless partial clone** | `--filter=blob:none`, on-demand blob fetch (already implemented, unused) | **Yes** — file *content* fetch is lazy, full tree/path structure still present | Opt-in only, and **not recommended for this hero scenario** (§4) | Only for repos whose build is known not to walk/hash broad swaths of the tree in one pass |

An operator escalates a specific repo through tiers only after evidence (a green run of the
scale-suite / acceptance-gate fixture at that tier — ties to #2060/§10). The **explicit
escape hatches** are tier P and tier 0 forever: nothing about this design requires any repo
to leave either. Tiers compose except where noted: tier 1 is safe to combine with 0/2/3 (it
doesn't change worktree contents); tier P excludes worktree-based tiers by definition
(§5.1). Tier 2 and tier 3 are independent axes (path scoping vs. blob laziness) and can
combine, but each needs its own verification — don't assume proving tier 2 safe also
proves tier 3 safe, or vice versa.

## 7. Design questions from #2063, answered

### 7.1 Clone strategy

For the hero profile: **large-repo mode** (§5) — pinned workspace over a full (refspec-
narrowed) mirror; no partial materialization. For everyone else: tier 0 (full mirror +
ephemeral worktrees) stays the default until an operator opts a specific repo into tier
2/3 with evidence. Blobless (tier 3) stays available (it's already built) for repos an
operator has verified tolerate it, but is explicitly **not** the recommended lever for
legacy/bespoke-build repos, contra the "biggest win, transparent to worktrees" framing in
v2-cloud-scale.md's B1 (§2) — that framing holds for well-behaved repos, not this hero
scenario's grounding case.

### 7.2 Shared object store

V2's **B3 (reference/alternates cache)** remains worth building for the
many-gaggles-per-node case — a node-level object cache (`workcopies/_objects/<repo-key>`)
that every gaggle-on-that-node's mirror references via git alternates, GC failing closed
while any dependent mirror exists (`v2-cloud-scale.md:161`). But it is **demoted from
"build first" to "build when the duplication is observed"**: under large-repo mode the hero
repo has one mirror and one workspace per node — there is nothing to dedupe on the path
this epic exists for. Rev 1 of this doc had it as the first lever; that ordering was wrong
for the hero scenario and is corrected in §13's work breakdown.

### 7.3 Sparse checkout scoped per gaggle

The schema already models this (`RepoRef.Checkout.Sparse`, §3.2) — the gap is a runner
implementation (#649) plus, new in this doc, **a build-validation gate before trusting it in
production**: promoting a repo to tier 2 should require a green run of that repo's actual
build/test path under the declared cone (ideally as part of the acceptance-gate fixture,
§10), not just an operator's assertion that "this gaggle only touches `services/web`."
`AdditionalRepos` checkouts (`internal/runner/run.go:3042-3075`) should honor
`Checkout.Sparse` too once implemented — today they ignore it identically to the primary
repo. Use git's **cone mode**, not the older path-list mode — this is the specific,
externally-validated lesson from Microsoft's Scalar work (§4): cone mode is what let them
match VFS-for-Git-class performance without a virtualization layer.

### 7.4 Explicit path-length budget

Large-repo mode largely dissolves the problem for the hero path — a pinned workspace at
`workcopies/<repo-key>/pin` has no per-run path component, reclaiming the entire ~50-char
RunID+stage segment (§3.4). Two changes still pay for every repo and tier, including
tier 0:

1. **Shorten the RunID+stage path segment.** Hash `RunID + "-" + stageName` (and the
   `-ref-<name>` reference-repo variant) to a fixed short token for the *directory name*
   only — the full RunID stays in the marker file and journal for traceability, only the
   filesystem path shortens. This is the single largest Goobers-controlled contributor to
   the ~131-char fixed prefix (§3.4). The implemented `wt-` plus 96-bit SHA-256 token is
   always 27 characters, replacing the roughly 50-character trace-ID-plus-stage segment
   and reclaiming about 23 characters with no behavior change visible to a stage.
2. **A loud preflight, not a silent failure deep in a build.** Before provisioning a
   workspace or worktree, compute the worst-case path length the repo's checkout could
   reach (needs a configured or measured ceiling per repo — see §10's benchmark-harness
   tie-in) and refuse the stage with a clear error if it can't fit, rather than letting an
   obscure build-tool error three layers deep be the first signal. `core.longpaths=true`
   (already set, §3.4) stays as defense-in-depth for git itself, not as the answer — it
   does nothing for the build tooling that actually trips `MAX_PATH` (§9).

Both are safe to build now, independent of PM's #2052 sequencing and independent of tier
adoption.

### 7.5 Disk-pressure/retention story

Not re-scoped here — #2052 (Dev-7, in flight) is the fix, and this design treats its
acceptance criteria (periodic sweep ticker, sane non-zero defaults instead of a silent
no-op, journal-less-orphan resolution) as a **hard prerequisite** for #2063's own
acceptance gate (§10): a 10GB-repo steady-state disk ceiling is meaningless if retention
only runs once at startup. One addition from this doc: the sweep must structurally exempt
pinned workspaces (§5.5). No other new scope proposed beyond flagging the dependency
explicitly so PM sequences #2063's children after (or alongside, not before) #2052 lands.

### 7.6 Provider-side load: ADO quota coordination with #2061

This design doesn't build ADO quota coordination — that's #2061's scope — but flags two
concrete inputs for whichever shared quota mechanism #2061 designs:

1. **Git transport traffic, not just REST calls, needs to be a first-class consumer of the
   shared quota ledger.** ADO throttles/rate-limits git protocol traffic under
   IP/identity limits, distinct from its REST API limits; a large-repo hero scenario's
   mirror-fetch and (if tier 3 is ever adopted for an ADO repo) blob-backfill traffic is
   exactly the load pattern most likely to trip that, and it's currently invisible to
   ADO's per-call-only backoff (§3.5).
2. **Large-repo mode is itself the biggest ADO-load mitigation this design controls** — one
   pinned workspace doing incremental `git fetch` with a narrowed refspec (§5.2) is the
   minimum-traffic shape possible: no per-stage re-materialization traffic, no blob
   backfill, no PR-ref hoovering. That's a further reason it's the recommended mode for
   ADO-hosted large repos specifically, on top of the build-safety reasoning in §4.

## 8. Forward dovetail: don't foreclose remote/cloud execution

The PO wants this to eventually connect to "provision cloud build or test to help the load
or size," and named a **dedicated host per mega-repo as the intended deployment shape**
for the hero customer — but ruled (2026-07-31) that it is carried later, not scoped here:
it needs its own generic design pass, since the target could be almost any shape (a
statically-addressed Windows box, a provisioned VM, possibly custom Go integration code).
What this design does is keep that path cheap:

- **#1087** (capability-routed stage execution — curated 2026-07-25: per-stage platform
  label, unlabeled ⇒ local, fail-fast when no match) is the mechanism. Its near-term,
  buildable-now form is an **external-target executor**: ship source to a single
  statically-configured remote host over SSH/WinRM, run the stage there, stream results
  back — no distributed scheduler required. Its full form generalizes #659's
  Windows-node-pool routing to arbitrary capabilities.
- **v2-cloud-scale.md's B5** (baked workspace snapshots for tier-3 pods —
  `v2-cloud-scale.md:166-170`) is the eventual answer to "don't re-pay a 10GB clone on
  ephemeral cloud compute": a periodically rebaked OCI image/PVC snapshot containing the
  mirror, with pods fetching only the delta on top.
- **What this design keeps clean for that future, without building it now:** the mirror
  cache root is already a config-relative path (`Layout.WorkcopiesDir()`,
  `internal/instance/instance.go:76-78`), not hardcoded to assume co-location with the
  daemon process — so a dedicated Windows host executing a routed #1087 stage can run the
  identical mirror + pinned-workspace machinery against its own local disk, with no
  redesign of the contract. Large-repo mode is substrate-agnostic by construction: "this
  repo runs pinned-serial with these timeouts" is a fact about the *repo*, not about which
  machine executes it, so the preset travels unchanged to a dedicated host — where
  serial-per-repo also stops costing anyone else anything, since the host does nothing
  else. The per-repo tier declarations (§6) travel identically.
- Explicitly **not** proposed here: any scheduler, provisioning automation, or Temporal
  wiring. That's #1087/#659/Workstream C's scope, gated on demonstrated need per their own
  acceptance criteria.

## 9. Windows operational realities

Any execution model for this repo class lives or dies on three Windows facts that no git
strategy fixes. Large-repo mode reduces exposure to all three (fewer materializations, fewer
deletions, serial execution) but each needs explicit handling or documentation. Operational
setup and recovery steps are in the
[Windows large-repo runbook](../guides/windows-large-repo-runbook.md).

### 9.1 Antivirus dominates small-file I/O

Defender real-time scanning makes bulk small-file writes — exactly what a 10 GB checkout
of a C#/C++ tree is — dramatically slower, and is a documented first-order cost for git
operations and builds on Windows ([microsoft/Windows-Dev-Performance
#27](https://github.com/microsoft/Windows-Dev-Performance/issues/27)). The runbook for
large-repo nodes must cover: Defender exclusions for the workcopies root (or the instance
root), and/or placing workcopies on a **Dev Drive** (ReFS + deferred scanning, Microsoft's
own answer for dev workloads). Goobers should *document and detect* (a preflight warning
when the workcopies root is neither excluded nor on Dev Drive is cheap), not silently
reconfigure the host's security posture — changing AV settings is an operator decision.

### 9.2 File locks break teardown — and builds hold locks by design

MSBuild's node-reuse feature deliberately keeps `MSBuild.exe` worker processes alive after
a build to speed the next one, holding loaded-assembly locks — a well-documented cause of
"access denied" on directory cleanup in CI ([dotnet/msbuild
#3141](https://github.com/dotnet/msbuild/issues/3141), [dotnet/msbuild #3140 — "MSBuild
should allow CI tools to isolate
invocations"](https://github.com/dotnet/msbuild/issues/3140)). Today's eager per-stage
worktree deletion (§3.1) is maximally exposed to this; the pinned workspace rarely deletes
anything, which is a real robustness win. What remains: stage process-tree cleanup (the
`internal/platform/proc` seam, #623/#1103) must expect orphaned build daemons
(MSBuild nodes, Roslyn compiler server `VBCSCompiler.exe`) between runs on a pinned
workspace, and the large-repo stage environment should set `MSBUILDDISABLENODEREUSE=1` by
default ([background](https://awakecoding.com/posts/disabling-msbuild-node-reuse-to-avoid-file-locking-issues/))
— overridable, since node reuse is also a legitimate warm-build speedup an operator may
prefer once cleanup is proven reliable. `goobers workspace reset` (§5.6) must kill
lingering build processes holding locks under the workspace before deleting, or it will
fail on exactly the workspaces that most need resetting.

### 9.3 The environment-isolation contract, stated explicitly

Legacy build systems mutate PATH and the environment aggressively (`vcvarsall.bat`-style
shells, custom wrapper scripts) and sometimes machine state (registry, GAC, COM
registration, global temp/cache dirs). The contract Goobers offers, stated plainly so
operators can reason about it:

- **Isolated per stage:** working directory; environment variables (every deterministic
  stage is its own OS subprocess, `internal/executor/shell.go:366` — env mutations die
  with the process); the process tree (via the proc seam, with §9.2's caveats).
- **Supported, per stage:** environment *bootstrap* — a stage's command can already invoke
  its own setup shell (`vcvarsall.bat && build.cmd`); the large-repo preset documentation
  should carry the pattern rather than inventing new config surface until a real repo
  proves the need.
- **Explicitly NOT isolated:** machine-global state — registry, GAC/COM, global caches,
  services. No checkout strategy isolates these. The honest mitigations are large-repo
  mode's serial-per-repo execution (one gross build at a time per node, §5.3) and,
  stronger, the dedicated-host deployment shape (§8). Repos whose builds are
  machine-mutating should not share a node with unrelated gaggles — a deployment-guidance
  statement, not an enforcement mechanism, at this stage.

## 10. Acceptance gate ("shine" definition)

Refining #2063's stub gate against §5/§6 and #2060 (scale suite has no repo-size/
user-count/tenant dimension):

- [ ] **Primary gate — large-repo mode on a large fixture:** a ≥10 GB *working tree* fixture
      (ideally shaped like the grounding scenario: deep C#/C++-style directory nesting,
      not a flat synthetic tree). Measured: (a) init→first run under a stated time budget;
      (b) **second run dramatically cheaper than the first** — no re-materialization,
      warm build state — this delta is the entire point of the mode and must be pinned as
      a number, not a claim; (c) steady-state disk under a stated ceiling (mirror +
      one workspace + build state); (d) path depth ≥ a stated floor passing the §7.4
      preflight.
- [ ] **B0-equivalent benchmark harness + synthetic large-repo fixture generator**
      (shared with #2060/the Validation & CI milestone, per v2-cloud-scale.md's own B0
      sequencing) — every claim above needs to be a measured number, not an estimate,
      before it's trusted for the real hero-scenario repo. The harness is required for the
      *gate*; it is deliberately **not** a prerequisite for building §5 itself, which is
      behaviorally simple and can be validated against a real repo first.
- [ ] Tier 1 (alternates), when built: byte-identical worktree contents to tier 0 — the
      "no behavior change" claim from §6 needs to be asserted, not assumed.
- [ ] Tier 2/3 promotion criteria (the build-validation gate from §7.3) defined precisely
      enough to be a real CI check before any repo is promoted off tier P/0/1 in
      production.
- [ ] **Blocked on #2052 landing** for the disk-pressure half of the gate (§7.5),
      including its structural exemption of pinned workspaces (§5.5).

## 11. PO rulings recorded (2026-07-31, second round)

For traceability, the rulings this rev incorporates — these are decided, not open:

1. **Pinned workspace is required**, not optional-among-peers; other tiers may also be
   supported, but large-repo mode is the real solution for the hero profile.
2. **10+ GB is working-tree size on disk** (file-explorer measurement); history size is a
   separate, per-repo unknown.
3. **Git only.** No TFVC or other source providers — out of scope permanently, not
   deferred.
4. **Build-state persistence accepted** — required, in fact — as part of the opt-in for
   the shared/pinned workspace pattern (the non-hermetic contract, §5.1).
5. **Serial-per-repo stays as an offering** even as build systems "get pretty gross";
   dedicated host is the stronger future shape (§8) and is **carried later** — it needs
   its own generic scoping (could be almost any shape, possibly custom Go code).
6. **No worktrees at all when pinned mode is on** (§5.1).
7. **Large-repo mode is a preset**: flips all toggles (concurrency, workspace, timeouts,
   etc.) to the known-safe-for-monoliths settings in exchange for working at all; each
   toggle individually configurable afterwards so operators can relax (or tighten) what
   their scenario tolerates (§5.4).
8. **Timeouts raise by default under the preset**; users can tighten (§5.4).
9. **Operator-declared tier promotion, no automated probe-and-fallback** (§6 — carried
   from rev 1's open question, now confirmed).

## 12. Scope boundary

**In scope for this doc:** large-repo mode (§5), the tier policy (§6), the six #2063 design
questions (§7), the path-length fixes (§7.4), and the Windows operational contract (§9) —
all V1, local-runner, buildable now. **Git-hosted repos only** (§1).

**Explicitly out of scope, cross-referenced not designed here:**
- #2052 implementation (Dev-7, in flight) — treated as a dependency (§7.5, §10), with one
  new requirement (pinned-workspace exemption, §5.5).
- #2061 implementation (ADO quota mechanism) — this doc names inputs for it (§7.6),
  doesn't design it.
- #1087/#659 (remote/routed execution), the dedicated-host deployment shape, and
  v2-cloud-scale.md Workstream C (test sandboxes) and B4/B5 (worktree pooling, baked
  snapshots) — addressed only as "don't foreclose" in §8, per PO ruling.
- #649 (sparse-checkout runner implementation itself) — this doc specifies the gate it
  needs (§7.3) but the implementation is a child issue, not this doc.
- TFVC or any non-git source provider — permanently out (§1).

## 13. Proposed work breakdown (for PM to file/sequence, not filed here)

Per PM's direction, no children are filed until this design lands. Proposed decomposition,
each intended as an isolated, single-PR-sized deliverable, in recommended order:

1. **Large-repo mode: pinned workspace + whole-run lease (§5.1–5.3)** — the headline
   deliverable: workspace provisioning at the stable path, fetch/reset/clean-policy
   lifecycle, run-long lease shared across gaggles, mutual exclusion with worktree mode,
   narrowed refspec. Largest item; no dependency on #2052 (the workspace is exempt from
   retention anyway) or the benchmark harness (validate on a real repo first, §10).
2. **Large-repo preset surface (§5.4) + timeout raises** — the one-switch config that
   defaults everything, each knob independently overridable. Depends on #1 for the
   toggles to exist.
3. **`goobers workspace reset` + failure-streak suggestion (§5.6)** — includes the §9.2
   lock-holding-process kill on reset. Depends on #1.
4. **RunID/stage path-segment hashing (§7.4.1)** — independent, low-risk, lands any time;
   benefits every non-pinned repo.
5. **Path-length preflight check (§7.4.2)** — needs a configured/measured per-repo
   ceiling; otherwise independent.
6. **Windows large-repo runbook + preflight detection (§9.1, §9.3)** — Defender/Dev Drive
   guidance, `MSBUILDDISABLENODEREUSE` default in the large-repo stage env, the
   env-isolation contract documented. Mostly docs + one preflight warning; cheap, high
   leverage.
7. **Benchmark harness + synthetic large-repo fixture (§10)** — shared with #2060;
   required to *pin* the gate numbers, sequenced after #1 can produce numbers worth
   pinning.
8. **Sparse-checkout runner implementation (#649) + build-validation gate (tier 2, §7.3)**
   — depends on #7 for the gate definition. Later.
9. **Reference/alternates cache (tier 1, §7.2)** — demoted from rev 1's "build first":
   build when many-gaggles-per-node mirror duplication is actually observed. No hero-path
   dependency.
10. **ADO quota inputs handoff to #2061** — not an implementation issue, a
    cross-link/comment on #2061 pointing at §7.6 so #2061's design accounts for
    git-transport load.

## 14. Remaining open questions for PM/PO

Most of rev 1's open questions are now PO-ruled (§11). What remains:

- Does the PO have a specific candidate repo (or repo shape) in mind for the synthetic
  fixture in §10, so the benchmark harness is validated against something representative
  rather than a generic large-repo generator? (Carried from rev 1 — still open. Candidate
  customer repos cannot be named in this doc; a sanitized shape description — file count,
  depth histogram, language mix — would be enough.)
- The default clean policy between runs (§5.1) is proposed as `none` (maximum build-state
  preservation). If early real-repo experience shows stale-state build failures dominate,
  `ignored-safe` may be the better default — flagged as a knob to revisit with data, not a
  blocking decision.
