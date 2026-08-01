# Large-repo execution model (#2063)

Status: draft for review. Filed from the state-of-repo review 2026-07-30 + PO input
2026-07-31, hero scenario #3. First deliverable per #2063: this design doc. No children
are filed until it lands — PM (`#control`, 2026-08-01) files and dispatches from it.

## 0. Verdict up front

**There is no single "right" clone/checkout strategy for this hero scenario, and this doc
does not pick one.** The PO's grounding scenario — a 10+ GB, decades-old .NET
Framework/C++ repo, bespoke and possibly undocumented build tooling, almost certainly
Windows, hosted on an ADO instance under load from hundreds of concurrent agents — is
exactly the profile where the cheap, obvious optimizations (blobless partial clone, sparse
checkout) have a real, documented failure mode: **bespoke build systems that walk, hash, or
glob the full tree break or silently degrade under partial materialization.** Real-world
prior art (Microsoft's own VFS for Git → Scalar retreat, and git's own partial-clone docs)
says the same thing independently — see §4.

So the design is a **policy the runner can apply per repo, ordered cheapest-and-safest
first, escalating only on evidence** — not a default every repo gets. §5 is the decision
model; §6 answers #2063's six design questions against it. §7 is explicitly about not
foreclosing the cloud/remote-execution dovetail the PO flagged, without building it now.

## 1. Grounding: the hero scenario, restated

Per #2063 and PO input 2026-07-31 (elaborated by the PO mid-design, 2026-07-31):

- **10+ GB of legacy code.** Concretely: .NET Framework and/or C++, not a modern
  Go/TS-style repo. Deep, namespace-mirrored directory trees (C#) and deep template/header
  include graphs (C++) are exactly the shape that eats Windows' 260-char `MAX_PATH` budget
  (confirmed as a live risk in this codebase already — §3.4).
- **Bespoke, possibly undocumented build systems.** Custom scripts, not a single blessed
  `dotnet build`/`msbuild` invocation. This is the load-bearing constraint for this whole
  doc: we cannot assume the build only touches files under a knowable path cone, or that it
  tolerates on-demand blob fetch latency, or that it even declares its own inputs. **A
  strategy that "just works" for a clean modern repo can silently corrupt or fail a build
  here in a way that's expensive to diagnose**, because the failure surfaces as a build
  error deep inside third-party or generated code, not as a clear "git" error.
- **Windows, very likely.** Confirmed nothing in this codebase currently probes or enforces
  a path-length ceiling — see §3.4. .NET Framework/C++ toolchains are also exactly the
  toolchains most likely to write their own deep `obj`/`bin`/generated-header paths on top
  of whatever budget the checkout scheme leaves them.
- **Hundreds of concurrent users against the same ADO instance.** This compounds with
  #2061 (ADO hero epic): ADO's provider-side quota/rate-limit handling in this codebase is
  strictly weaker than GitHub's today (§3.5), so large-repo traffic patterns (fetch volume,
  blob backfill storms) are a direct load-multiplier on an already-thin mechanism.
- **The eventual answer is not "one machine, one disk, forever."** The PO explicitly wants
  this design to leave room to dovetail with remote/cloud execution (a routed Windows
  worker, or a provisioned cloud build/test sandbox) so load and size can be shed off the
  local runner over time. §7 addresses this as a non-goal-now / don't-foreclose-it-later
  seam, consistent with `docs/design/v2-cloud-scale.md`.

## 2. Relationship to `docs/design/v2-cloud-scale.md` §3 (Workstream B)

V2's cloud-scale design already staked out a layered large-repo cache strategy —
`docs/design/v2-cloud-scale.md:140-179` (B0–B6) — and B6 (authenticated clone) and an
opt-in form of B1 (blobless mirrors) are **already implemented** (§3.1). This doc does not
redo that layering; it answers #2063's questions against it and **corrects one assumption**:

- V2-cloud-scale.md's B1 write-up calls partial-clone mirrors "transparent to worktrees."
  For a well-behaved repo that's true. For the bespoke-build-system hero scenario this doc
  is scoped to, it is **not** — see §4. This doc narrows B1 from "the default layer
  everyone gets" to "a lever some repos can opt into once proven safe for that repo."
- B4 (worktree pooling) and B5 (baked workspace snapshots for tier-3 pods) are V2/tier-3
  concerns — they assume a scheduler and pod substrate this design doesn't have yet. This
  doc is scoped to what the **local runner** (V1, architecture-of-record: local runner
  first, Temporal at V2) can do now, and calls out in §7 what NOT to bake in so B4/B5 remain
  buildable later without a redesign.
- B3 (reference/alternates cache) is not yet implemented; this doc recommends it as the
  first concrete lever to build (§5, §6.2) because unlike B1/B2 it changes **nothing** about
  what a worktree materializes — it's strictly safe for a bespoke build system.
- B2 (sparse checkout) is schema-declared but inert today (#649); this doc's per-gaggle
  scoping answer (§6.3) is consistent with B2's shape but adds the build-validation gate
  V2's one-line mention didn't need to specify.

## 3. Current state (grounded in code)

### 3.1 Clone/mirror mechanics — already shared, already reused

There is **one mirror per distinct repo URL, reused across every run**, not a fresh clone
per run. `Manager.WorkingCopy` (`internal/worktree/manager.go:260-327`) clones once
(`git clone --mirror`, `manager.go:272-276`) into a content-addressed path keyed by a
16-hex SHA-256 prefix of the repo URL (`repoKey`, `manager.go:195-198`), then every
subsequent run does `git fetch --prune origin` against that same mirror
(`manager.go:307-317`). Concurrent runs against the same repo serialize on a per-repo-key
mutex (`manager.go:216-227`) rather than racing. `AdditionalRepos` (read-only reference
repos, MGV-11/#1286) reuse the identical mechanism keyed by their own URL
(`internal/runner/run.go:3042-3075`) — real, working prior art for "N repos, N mirrors,
shared across every gaggle/run that targets them," with no primary-vs-reference
special-casing in the sharing mechanism itself.

Blobless partial clone (`--filter=blob:none`) already exists as an **opt-in, new-mirror-only**
flag (`WithPartialClone`, `manager.go:127-148`): it narrows the fetch refspec and adds the
filter on first clone, but an existing full mirror is never migrated, and nothing in
production code turns it on today (no config surface calls `WithPartialClone`). No
`--depth`/shallow flag is used anywhere.

**Per-run worktrees are `git worktree add` against the shared mirror** — never an
independent clone (`internal/worktree/worktree.go:133-312`), and the granularity is one
worktree per **(run, stage)** attempt, not per run (`RunID: in.RunID + "-" + stageName`,
`internal/runner/run.go:3013-3016`). Teardown is eager and synchronous at the end of every
stage attempt (`worktree.go:535-586`, `run.go:2784`/`3148`), with a separate
crash-orphan reaper (`Manager.Reap`) that runs once at daemon startup only
(`internal/worktree/reap.go:75-100`, `cmd/goobers/up.go:443-466`) — **there is no periodic
sweep of live worktrees**, which matters for §3.3.

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
prerequisite for the acceptance gate (§8), not as something to re-scope here.** The
"disk-pressure graduates from hygiene to blocker at 10GB/checkout" framing in #2063 is
exactly why: at 10GB per checkout, a startup-only sweep with silently-disabled limits means
disk exhaustion is a matter of "how many failed runs accumulate before the next restart,"
which is not a story this design can accept.

Disk usage IS measured today, but only at lifecycle boundaries (worktree create/teardown/
housekeeping — `internal/worktree/usage.go:55-90`), emitted as OTel span events
(`internal/telemetry/workcopy.go:26-80`) when a telemetry client exists. There's no
continuous gauge — worth keeping in mind for whatever dashboard/alert eventually watches
the 10GB ceiling in §8's acceptance gate.

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
path-traversal safety, never length).

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

## 4. Why this has to be a policy, not a default — the tension, with evidence

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
- **General agentic-modernization practice reinforces the same caution rather than
  resolving it**: infrastructure — not the model — is repeatedly cited as the actual
  bottleneck for AI coding agents against large/legacy codebases, and tooling built for
  "big code" (Sourcegraph, Augment) treats large-repo support as a distinct, hard problem
  rather than something a single clone flag solves.
  ([Sourcegraph: Agentic Coding in 2026](https://sourcegraph.com/blog/agentic-coding),
  [awesome-agentic-software-modernization](https://github.com/feststelltaste/awesome-agentic-software-modernization))

None of this means "never optimize." It means **the runner must not assume optimization is
safe by default for an unproven repo**, and must give an operator a way to declare what's
safe once it's been verified — which is what §5 is.

## 5. Recommended model: per-repo policy, cheapest-and-safest first, escalate on evidence

A new `RepoRef`-level (or gaggle-level, for the primary `Project`) config field,
`checkout.strategy`, with an explicit, ordered set of tiers. Each tier is strictly opt-in —
an operator declares a repo has been verified to tolerate a tier; the runner never
self-promotes a repo to a riskier tier on its own.

| Tier | Mechanism | Changes what the build sees? | Default for an unverified repo | Prerequisite |
|---|---|---|---|---|
| **0 — full mirror** (today's default) | `git clone --mirror` + `git fetch --prune`, full worktree | No — full tree, always | **Yes** | none |
| **1 — reference/alternates cache** (V2 B3, pulled forward) | Per-node object cache shared via `--reference`/alternates across gaggles targeting the same repo | No — still a full worktree, just fewer duplicate mirrors | Safe to default on once built (§6.1) | GC must fail closed while any dependent mirror exists (V2-cloud-scale.md:161) |
| **2 — sparse checkout (cone mode)** | Per-gaggle `checkout.sparse` path cones, materialized via `git sparse-checkout` (cone mode, not the slower non-cone form) | **Yes** — only declared paths exist on disk | Opt-in only, gated (§6.3) | A build-validation gate: the declared cone must be proven to build/test green before being trusted in production runs |
| **3 — blobless partial clone** | `--filter=blob:none`, on-demand blob fetch (already implemented, unused) | **Yes** — file *content* fetch is lazy, full tree/path structure still present | Opt-in only, and **not recommended as the default lever for this hero scenario** (§4) | Only for repos whose build is known not to walk/hash broad swaths of the tree in one pass |

An operator escalates a specific repo through tiers only after evidence (a green run of the
scale-suite / acceptance-gate fixture at that tier — ties to #2060/§8). The **explicit
escape hatch** is tier 0 forever: nothing about this design requires any repo to ever leave
full-mirror mode, and that must remain true — a repo whose build system genuinely can't be
verified safe under 1/2/3 keeps working exactly as it does today, just with the path-length
and retention fixes from §6.4/§6.5 that don't touch what the build sees.

Tiers compose except where noted: tier 1 is safe to combine with 0/2/3 (it doesn't change
worktree contents). Tier 2 and tier 3 are independent axes (path scoping vs. blob laziness)
and can combine, but each needs its own verification — don't assume proving tier 2 safe
also proves tier 3 safe, or vice versa.

## 6. Design questions from #2063, answered

### 6.1 Clone strategy

Tier 0 (full mirror) stays the default, for every repo, until an operator opts a specific
repo into tier 2/3 with evidence. This is **not a regression from "no strategy"** — the
shared mirror + fetch model (§3.1) already avoids the worst cost (repeated full clones);
what's missing is tier 1 (§6.2) to stop paying it once *per node* instead of once *ever*.
Blobless (tier 3) stays available (it's already built) for repos an operator has verified
tolerate it, but is explicitly **not** the recommended default for legacy/bespoke-build
repos, contra the "biggest win, transparent to worktrees" framing in v2-cloud-scale.md's B1
(§2) — that framing holds for well-behaved repos, not this hero scenario's grounding case.

### 6.2 Shared object store

Recommend building V2's **B3 (reference/alternates cache)** now, pulled forward into this
V1 design, rather than treating it as V2-only: a node-level object cache
(`workcopies/_objects/<repo-key>`) that every gaggle-on-that-node's mirror references via
git alternates, so N gaggles targeting the same repo stop paying N full mirror-clone costs.
This is tier 1 — strictly safe (§5), directly addresses the "repeated clones are painful at
10GB" pain point named in #2063, and needs no build-system trust to ship. Lifecycle: GC only
when no dependent mirror exists (fail closed on GC, per v2-cloud-scale.md's own note,
`v2-cloud-scale.md:161`) since alternates make premature deletion unsafe.

### 6.3 Sparse checkout scoped per gaggle

The schema already models this (`RepoRef.Checkout.Sparse`, §3.2) — the gap is a runner
implementation (#649) plus, new in this doc, **a build-validation gate before trusting it in
production**: promoting a repo to tier 2 should require a green run of that repo's actual
build/test path under the declared cone (ideally as part of the acceptance-gate fixture,
§8), not just an operator's assertion that "this gaggle only touches `services/web`."
`AdditionalRepos` checkouts (`internal/runner/run.go:3042-3075`) should honor
`Checkout.Sparse` too once implemented — today they ignore it identically to the primary
repo. Use git's **cone mode**, not the older path-list mode — this is the specific,
externally-validated lesson from Microsoft's Scalar work (§4): cone mode is what let them
match VFS-for-Git-class performance without a virtualization layer.

### 6.4 Explicit path-length budget

Two concrete, low-risk changes, independent of the tiering policy above (they help every
tier, including tier 0):

1. **Shorten the RunID+stage path segment.** Hash `RunID + "-" + stageName` (and the
   `-ref-<name>` reference-repo variant) to a fixed short token for the *directory name*
   only — the full RunID stays in the marker file and journal for traceability, only the
   filesystem path shortens. This is the single largest Goobers-controlled contributor to
   the ~131-char fixed prefix (§3.4) and reclaims meaningful budget with no behavior change
   visible to a stage.
2. **A loud preflight, not a silent failure deep in a build.** Before provisioning a
   worktree, compute the worst-case path length the repo's checkout could reach (needs a
   configured or measured ceiling per repo — see §8's benchmark-harness tie-in) and refuse
   the stage with a clear error if it can't fit, rather than letting an obscure build-tool
   error three layers deep be the first signal. `core.longpaths=true` (already set,
   §3.4) stays as defense-in-depth for git itself, not as the answer.

Both are safe to build now, independent of PM's #2052 sequencing and independent of tier
2/3 adoption.

### 6.5 Disk-pressure/retention story

Not re-scoped here — #2052 (Dev-7, in flight) is the fix, and this design treats its
acceptance criteria (periodic sweep ticker, sane non-zero defaults instead of a silent
no-op, journal-less-orphan resolution) as a **hard prerequisite** for #2063's own acceptance
gate (§8): a 10GB-repo steady-state disk ceiling is meaningless if retention only runs once
at startup. No new scope proposed here beyond flagging the dependency explicitly so PM
sequences #2063's children after (or alongside, not before) #2052 lands.

### 6.6 Provider-side load: ADO quota coordination with #2061

This design doesn't build ADO quota coordination — that's #2061's scope — but flags two
concrete inputs for whichever shared quota mechanism #2061 designs:

1. **Git transport traffic, not just REST calls, needs to be a first-class consumer of the
   shared quota ledger.** ADO throttles/rate-limits git protocol traffic under
   IP/identity limits, distinct from its REST API limits; a large-repo hero scenario's
   mirror-fetch and (if tier 3 is ever adopted for an ADO repo) blob-backfill traffic is
   exactly the load pattern most likely to trip that, and it's currently invisible to
   ADO's per-call-only backoff (§3.5).
2. **The tier-0/tier-1 default (§5, §6.1) partially self-mitigates this independent of
   #2061 landing** — by keeping fetch traffic to incremental `git fetch --prune` against an
   already-shared mirror rather than repeated full clones or blob-backfill storms, this
   design avoids adding a new load source on top of the gap #2061 already identified. That's
   a reason to prefer tier 0/1 for ADO-hosted large repos specifically, on top of the
   build-safety reasoning in §4.

## 7. Forward dovetail: don't foreclose remote/cloud execution

The PO wants this to eventually connect to "provision cloud build or test to help the load
or size" — that capacity already has a name and an owner in the backlog, and this design
doesn't build it, but must not make it harder later:

- **#1087** (capability-routed stage execution — curated 2026-07-25: per-stage platform
  label, unlabeled ⇒ local, fail-fast when no match) is the mechanism. Its near-term,
  buildable-now form is an **external-target executor**: ship source to a single
  statically-configured remote host (e.g. a Windows box) over SSH/WinRM, run the stage
  there, stream results back — no distributed scheduler required. Its full form generalizes
  #659's Windows-node-pool routing to arbitrary capabilities.
- **v2-cloud-scale.md's B5** (baked workspace snapshots for tier-3 pods —
  `v2-cloud-scale.md:166-170`) is the eventual answer to "don't re-pay a 10GB clone on
  ephemeral cloud compute": a periodically rebaked OCI image/PVC snapshot containing the
  mirror, with pods fetching only the delta on top.
- **What this design keeps clean for that future, without building it now:** the mirror
  cache root is already a config-relative path (`Layout.WorkcopiesDir()`,
  `internal/instance/instance.go:76-78`), not hardcoded to assume co-location with the
  daemon process — so a remote Windows node executing a routed #1087 stage can run the
  identical `worktree.Manager` machinery against its own local disk, with no redesign of
  the clone/mirror/worktree contract itself. The tiering policy in §5 is also
  substrate-agnostic by construction: "which tier is this repo verified safe at" is a fact
  about the *repo*, not about which machine executes the checkout, so it travels cleanly to
  a remote worker or a future cloud sandbox (`v2-cloud-scale.md` Workstream C) without
  re-deriving.
- Explicitly **not** proposed here: any scheduler, provisioning automation, or Temporal
  wiring. That's #1087/#659/Workstream C's scope, gated on demonstrated need per their own
  acceptance criteria.

## 8. Acceptance gate ("shine" definition)

Refining #2063's stub gate against §5's tiering and #2060 (scale suite has no repo-size/
user-count/tenant dimension):

- [ ] **B0-equivalent first: a provisioning benchmark harness + synthetic large-repo
      fixture generator** (shared with #2060/the Validation & CI milestone, per
      v2-cloud-scale.md's own B0 sequencing) — every claim below needs to be a measured
      number, not an estimate, before it's trusted for the real hero-scenario repo.
- [ ] A synthetic ≥10GB repo fixture, ideally shaped like the grounding scenario (deep
      C#/C++-style directory nesting, not a flat synthetic tree) — init→first run under a
      stated time budget, steady-state disk under a stated ceiling, path depth ≥ a stated
      floor, at **tier 0** (the default every repo gets) as the primary pinned gate.
- [ ] A second fixture run proving tier 1 (reference/alternates) reduces per-node clone
      cost with byte-identical worktree contents to tier 0 — the "no behavior change"
      claim from §5 needs to be asserted, not assumed.
- [ ] Tier 2/3 promotion criteria (the build-validation gate from §6.3) defined precisely
      enough to be a real CI check before any repo is promoted off tier 0/1 in production.
- [ ] **Blocked on #2052 landing** for the disk-pressure half of the gate (§6.5).

## 9. Scope boundary

**In scope for this doc:** the six #2063 design questions (§6), the tiering policy they
answer against (§5), and the path-length fixes (§6.4) — all V1, local-runner, buildable now.

**Explicitly out of scope, cross-referenced not designed here:**
- #2052 implementation (Dev-7, in flight) — treated as a dependency (§6.5, §8).
- #2061 implementation (ADO quota mechanism) — this doc names inputs for it (§6.6), doesn't
  design it.
- #1087/#659 (remote/routed execution) and v2-cloud-scale.md Workstream C (test sandboxes)
  and B4/B5 (worktree pooling, baked snapshots) — V2/tier-3, addressed only as "don't
  foreclose" in §7.
- #649 (sparse-checkout runner implementation itself) — this doc specifies the gate it
  needs (§6.3) but the implementation is a child issue, not this doc.

## 10. Proposed work breakdown (for PM to file/sequence, not filed here)

Per PM's direction, no children are filed until this design lands. Proposed decomposition,
each intended as an isolated, single-PR-sized deliverable:

1. **Reference/alternates cache (tier 1, §6.2)** — node-level object cache + fail-closed GC.
   No dependency on #2052/#2061; safe to land first.
2. **RunID/stage path-segment hashing (§6.4.1)** — shorten the worktree directory name.
   Independent, low-risk, lands any time.
3. **Path-length preflight check (§6.4.2)** — depends on #1 having established where the
   benchmark numbers live, but is otherwise independent.
4. **Sparse-checkout runner implementation (#649) + build-validation gate (tier 2, §6.3)** —
   the largest item; depends on the benchmark harness (§8) existing to define the gate.
5. **Benchmark harness + synthetic large-repo fixture (§8)** — shared with #2060; should
   probably be sequenced *before* #4, since #4's gate depends on it.
6. **ADO quota inputs handoff to #2061** — not an implementation issue, a cross-link/comment
   on #2061 pointing at §6.6 so #2061's design accounts for git-transport load.

## 11. Open questions for PM/PO

- Does the PO have a specific candidate repo (or repo shape) in mind for the synthetic
  fixture in §8, so the benchmark harness is validated against something representative
  rather than a generic large-repo generator?
- Should tier 2/3 promotion be a per-repo config an operator sets by hand (this doc's
  assumption), or should the runner itself attempt an automated "try tier 3, fall back to
  tier 0 on build failure" probe? This doc recommends operator-declared (fail loud, never
  silently retry at a lower tier mid-run — a partially-built worktree is worse than a slow
  one), but flags it as a real design choice worth a PO ruling if there's a strong
  preference either way.
- Priority/sequencing of #6 (remote/cloud dovetail groundwork) relative to the rest of the
  breakdown — this doc treats it as "keep the seam clean," not a scheduled deliverable;
  confirm that's the right level of investment for now.
