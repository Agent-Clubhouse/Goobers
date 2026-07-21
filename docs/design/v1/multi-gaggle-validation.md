# Design: V1 multi-gaggle + per-repo credential scoping

> Status: **Draft for review** · Area: `RUN` / `SEC` / `WF` · Milestone: **V1 — arbitrary
> repos, teams, hardening** (epic #34)
> Extends: [`34-arbitrary-repo-hardening.md`](34-arbitrary-repo-hardening.md)
> (runtime scoping), [`35-sandboxing-per-goober-creds.md`](35-sandboxing-per-goober-creds.md)
> (sandbox + per-goober creds). References the mixed-mode epic **#804**.
> Origin: a concrete multi-gaggle validation (one instance driving several gaggles across
> different GitHub owners and different stacks; shared agent-model token, per-repo-scoped gh
> PATs; otherwise isolated). The concrete repos/accounts live in the operator's private
> instance config — this doc is the generic design.

## Terminology (two orthogonal axes — do not conflate)

- **Multi-gaggle** = one instance running **multiple projects** (gaggles), each bound to its
  own repo(s), backlog, credentials, and runtime state. *This doc.*
- **Mixed-mode** = a **single repo with diverse authors** — goobers, humans, and outside
  agents all pushing. Makes `merge-review`/`pr-remediation` need to be *actor-aware*. That is
  epic **#804 / #369**, referenced here (§4 G4) but designed there. A gaggle can be
  goober-authored *or* mixed-mode independent of how many gaggles the instance runs.

## 1. Verdict

The config model and scheduler are already multi-gaggle: `GaggleSpec` binds a gaggle to a
`Project` repo (plus optional `AdditionalRepos`), and the daemon keys workflows by
`(gaggle, workflow)`, resolving each to that gaggle's repo (`runnerwiring.go:936`, tested
with a 2-gaggle fixture). **Scheduling multiple gaggles works today.** The shared agent
token also works — `agent:model` is a per-capability grant (#287), injected as a distinct
env var from the gh token.

A realistic validation nonetheless **breaks the V1 credential assumption** in #34 and
surfaces requirements the existing designs do not cover:

1. **Per-*repo* credential scoping is now mandatory, not deferrable.** #34 OQ-3 recommends
   "one instance token shared across gaggles in V1." That fails the moment two target repos
   sit under **different GitHub owners** (no single PAT reaches both), and least-privilege
   wants a distinct PAT per repo regardless. Yet `buildCredentials`
   (`cmd/goobers/runnerwiring.go`) is **instance-global** — *"the first repo's token backs
   every credentialed capability"* (`Repos[0]`). So per-repo gh PATs cannot be expressed or
   routed. **And this is per-repo, not merely per-gaggle:** a single gaggle may span two
   repos (server + client; source + wiki) via `AdditionalRepos`, each wanting its own scoped
   token — so the scope key is `(gaggle, repo, capability)`, with a shared agent token above.
2. **Per-gaggle CI/build command.** Different gaggles run different stacks (a Go `make ci`;
   an `npm`-built static site; a lint + typecheck + unit/component test suite for a TS app).
   `local-ci` hard-assumes the Go toolchain, so any foreign gaggle's PRs fail the gate. A
   gaggle must **declare its own CI command**.

Plus: the **`goobers/` branch namespace is hardcoded** in the mirror fetch (#965) and some
stage defaults, and any gaggle targeting a **mixed-mode** repo needs the actor-aware
handling from #804/#369 before it is turned on.

## 2. Scope key: `(gaggle, repo, capability) → token`, with a shared default

The design generalizes the credential model from one dimension (capability) to three, all
backwards-compatible:

- `agent:model` (and any capability with no repo/gaggle qualifier) → the **shared,
  instance-wide token** (the agent-model token every gaggle uses). Unchanged.
- A repo capability (`repo:push`, `github:issues:write`, `github:pr:write`, …) resolves to
  the token for **that run's gaggle and the repo the operation targets** — defaulting to that
  repo's own configured token, with an optional explicit grant override.
- **Per-repo capabilities differentiate read-only reference repos from the read-write
  sink.** A gaggle routinely spans repos it only *reads* (reference material, cross-repo
  context) and one (or few) it *writes*. The scope key carries the capability, so a repo
  entry can be granted a read-only token (Contents:read) while another carries a write token
  (Contents/PR/Issues:write). Two concrete cases the design must serve (§4.5):
  - **Site gaggle:** the site repo is read-write (curated content); the Goobers and Clubhouse
    repos are read-only reference sources.
  - **`docs-updater` (#472):** N read-only reference/source repos feed a docs-drift signal,
    and the output *sink* is a single per-repo choice — in-repo, a **separate docs repo**
    (#1019), a **GitHub wiki** (#1020), or an **ADO wiki** (#1021) — each a write target
    distinct from the reference reads. Phase 2 of docs-updater is a direct **consumer** of
    MGV-5 and gates on it.
- A one-gaggle, one-repo instance with no qualifiers behaves **exactly as today**.

## 3. What already exists vs. what this needs

| Capability | State | Needed |
|---|---|---|
| Multi-gaggle scheduling `(gaggle,workflow)` | ✅ works, tested | — |
| Per-gaggle runtime layout `gaggles/<g>/runs,workcopies` | designed #34/H2 (#159, approved) | land it (supervised) |
| Shared agent-model token | ✅ #287 | — |
| **Per-repo gh PAT routing `(gaggle,repo,cap)`** | ❌ instance-global (`Repos[0]`) | **G1 — load-bearing** |
| Multi-repo per gaggle (`AdditionalRepos`) | config field exists; no per-repo cred routing | folded into **G1** |
| **Per-gaggle CI command** | ❌ Go-hardcoded | **G2** |
| Per-gaggle branch namespace | ❌ `goobers/` hardcoded (#965) | **G3** |
| Sandbox / per-goober creds | designed #35 (S0 done) | orthogonal; not blocking locally |
| Mixed-mode / actor-aware (a repo with human authors) | ❌ epic #804 / #369 | **G4 — per mixed-mode gaggle only** |

## 4. Designs for the new gaps

### G1 — Per-repo credential scoping (load-bearing)

`CredentialGrant` is `{Capability, Token}` with no gaggle/repo dimension, and
`buildCredentials` grants `Repos[0]`'s token for every repo capability instance-wide. Make it
key on `(gaggle, repo, capability)`:

- **Config:** a `GaggleSpec` may carry its own `credentials:` block; each repo (Project or an
  `AdditionalRepos` entry) may name its own token; a top-level grant may take optional
  `gaggle:`/`repo:` selectors (OQ-1). Unqualified `agent:model` stays the shared token.
- **Resolution:** `buildCredentials` becomes gaggle/repo-aware — a stage's injected token is
  chosen by its run's gaggle and the repo the operation targets, defaulting to that repo's
  own configured token. Backwards-compatible for the single-repo case.
- **Enforcement-by-construction (V1 posture, per #685):** a gaggle's stages only ever hold
  tokens for that gaggle's repos. OS/namespace secrecy is V2 (#685); V1 gets *scoping*.

**High-blast-radius** (core credential resolution the instance itself runs on) → supervised.

### G2 — Per-gaggle CI command

Add a declared **CI command per gaggle** (on `GaggleSpec`, overridable per-workflow input,
OQ-2), which `local-ci` runs in the gaggle's worktree instead of the hardcoded Go path. A
non-zero exit fails the gate exactly as today. **Additive and gaggle-local** — a bad command
only fails that gaggle's own PRs, never the shared pipeline. **Approvable.**

### G3 — Per-gaggle branch namespace

Fix #965 (mirror-fetch's hardcoded `goobers/` exclusion) and make `headPrefix` defaults
**derive from the gaggle** (building on #982). Keeps the `goobers/<workflow>/<run>`
convention but stops the hardcode silently discarding a foreign gaggle's run branches.
Scoped, mostly mechanical. **Approvable.**

### G4 — Actor-aware handling for a mixed-mode gaggle

A gaggle whose repo has human (or non-goober agent) authors must make
`merge-review`/`pr-remediation` **actor-aware** — act only on the gaggle's own
goober-authored PRs, never a human's or an outside contributor's (the #797 case), honoring
the repo's own contribution contract. This is **epic #804 + #369**, unapproved and needing
PO review — **out of scope for an unsupervised run**; a mixed-mode gaggle must not be enabled
until it lands. A purely goober-authored gaggle does **not** need G4.

### G5 — Provable cross-gaggle isolation: conformance test + outstanding OS enforcement

The credential requirement is not just *route the right token* but **prove a gaggle's stages
can only ever hold their own gaggle/repo credentials, and cannot reach another gaggle's repos
or secrets.** V1's posture is **isolation by construction, proven by an automated conformance
test** — not yet OS-enforced. Both halves matter and must ship together.

**What V1 delivers (this sprint):**
- *Scoping by construction* — MGV-5 (#1012) `(gaggle,repo,cap)` routing, layered on
  per-goober credential injection (#35/S1) and stage-worktree filesystem confinement
  (#165/#35-S2). A stage is only ever *handed* its own gaggle's tokens.
- *A new isolation-conformance test* (**MGV-9**, below): a fixture instance with two gaggles
  (A, B) under different owners asserts, mechanically, that a stage in gaggle A: (a) has **no
  gaggle-B token** anywhere in its subprocess env; (b) cannot resolve a capability scoped to
  B (fail-closed `ErrNoCredentialForCapability`); (c) its worktree/git remotes reference
  **only** A's repos — no credentialed path to B's repos. This is the artifact that
  *provably shows* the isolation claim, runnable in CI on every change.

**Outstanding — explicitly tracked as security/sandboxing debt, NOT delivered here:**
V1 scoping means a stage is never *given* another gaggle's secret, but it does **not** yet
OS-enforce that a compromised or buggy stage cannot *reach* one (a shared HOME, an ambient
credential on disk, an unsandboxed egress). The hard-enforcement rungs remain future work and
must be surfaced as known posture, not silently assumed closed:
- **OS-native agentic sandbox** (Seatbelt/bubblewrap) confining the subprocess FS + the
  sandboxed-execution fail-closed path — designed in #35 (S2 #165 / S3 #166 / S4 #167), **not
  yet built**. Until it lands, filesystem confinement is by convention, not by the OS.
- **Per-gaggle workload identity + store-side secret ACLs** (#685, **V2**) — the only rung
  that denies cross-gaggle secret *resolution* by construction (gaggle A's identity has no
  read on B's refs). V1 file/env refs have no such store-side denial.
- **Egress / network-policy enforcement** (#167, SEC-Q5 → tier-3/V2) — a stage's outbound
  network is documented, not enforced, in V1.

`docs/design/v1/35-sandboxing-per-goober-creds.md` and `docs/requirements/security.md` own
the enforcement rungs; this section records that a multi-gaggle instance **operates on
scoping + conformance-test proof today, with OS/identity enforcement as required, tracked
future security work.** The conformance test (MGV-9) must assert the *scoping* invariant so a
regression is caught even before the OS rungs land.

## 5. Decomposition — dispatchable work items

| ID | Issue | Item | Risk | Status |
|---|---|---|---|---|
| MGV-1 | #1009 | G2 — per-gaggle CI command run by `local-ci` | Low (additive, gaggle-local) | **approved** |
| MGV-2 | #965 | G3a — fix mirror-fetch hardcoded `goobers/` | Low-Med | **approved** |
| MGV-3 | #1010 | G3b — gaggle-derived `headPrefix` (extend #982) | Low | **approved** |
| MGV-4 | #1011 | Foreign-gaggle `goobers validate` diagnostics | Low | **approved** |
| MGV-5 | #1012 | G1 — per-repo credential scoping `(gaggle,repo,cap)` | **High** (core creds) | filed, supervised |
| MGV-6 | #159 | #34-H2 per-gaggle runtime scoping | **High** (core runtime) | approved, **pulled from `ready`** |
| MGV-7 | #775/#161 | #34-H3/H4 multi-gaggle daemon loop / fairness | Med | #775 ready; rest supervised |
| MGV-8 | #804/#369 | G4 — actor-aware mixed-mode | High + PO | **hold — mixed-mode gaggle off until it lands** |
| MGV-9 | *(new)* | G5 — 2-gaggle isolation-conformance test (no cross-gaggle env creds / resolution / git reach) | Low (test-only) | **approvable — proves the isolation claim** |

> **Isolation debt (not work items in this sprint, tracked as outstanding security posture, §4.5 G5):** OS-native sandbox rungs #165/#166/#167 (#35), and per-gaggle workload identity + store secret ACLs #685 (V2). MGV-9 proves *scoping*; these enforce it.

## 6. Recommended sequencing

1. **Now (approvable, low-risk):** MGV-1/2/3/4 — make a **foreign goober-authored gaggle**
   runnable; individually safe. MGV-9 (isolation-conformance test) can land in parallel — it
   asserts the scoping invariant and hardens the sprint against regressions before MGV-5.
2. **Supervised, next:** MGV-5 (per-repo credentials) + MGV-6 (#159 runtime scoping) — the
   two load-bearing core changes. Land these and a goober-authored gaggle can run fully
   isolated with its own scoped PAT (including a repo under a different owner).
3. **Design + PO review, then build:** MGV-8 (mixed-mode) before any gaggle targeting a
   human-populated repo is enabled.

**First validation milestone:** a single goober-authored gaggle on a *different-owner*,
*non-Go* repo running green end-to-end — it exercises G1/G2/G3 without needing G4, and proves
Goobers can build and ship an autonomous project in a separate repo before taking on the
harder mixed-mode case.

## 7. Open questions (PO)

- **OQ-1 — credential config surface:** per-gaggle `credentials:` block on `GaggleSpec`,
  per-repo token on each repo entry, and/or a top-level grant with `gaggle:`/`repo:`
  selectors? *(Recommend: token on each repo entry + optional gaggle block; top-level stays
  the shared default — reads cleanly for "shared agent token + per-repo PAT.")*
- **OQ-2 — CI command surface:** on `GaggleSpec` or per-workflow input? *(Recommend:
  `GaggleSpec` default, overridable per workflow.)*
- **OQ-3 — multi-repo-per-gaggle CI:** when a gaggle spans repos (server+client), is CI one
  command over the checked-out set, or per-repo? *(Recommend: one gaggle command over the
  workspace; revisit if a real case needs per-repo.)*
- **OQ-4 — different-owner PAT trust:** confirm a fine-grained PAT scoped to one repo under a
  distinct owner is an acceptable trust boundary for an unattended daemon.
