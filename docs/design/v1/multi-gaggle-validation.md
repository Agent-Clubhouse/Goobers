# Design: V1 multi-gaggle + per-repo credential scoping

> Status: **draft — for review** · Area: `RUN` / `SEC` / `WF` · Milestone: **V1 — arbitrary
> repos, teams, hardening** (epic #34)
> Extends: [`34-arbitrary-repo-hardening.md`](34-arbitrary-repo-hardening.md)
> (runtime scoping), [`35-sandboxing-per-goober-creds.md`](35-sandboxing-per-goober-creds.md)
> (sandbox + per-goober creds). References the mixed-mode epic **#804**.
> Origin: a concrete multi-gaggle validation (one instance driving several gaggles across
> different GitHub owners and different stacks; shared agent-model token, per-repo-scoped gh
> PATs; otherwise isolated). The concrete repos/accounts live in the operator's private
> instance config — this doc is the generic design.
>
> **2026-07-27 addendum:** a live grounding walkthrough (the `clubhouse-site` gaggle reading
> `Clubhouse`/`Goobers` via `additionalRepos`) found that MGV-10 (#1285, shipped) never
> actually resolved OQ-1 below — it built read-only *capability routing* on top of the
> existing single-token-per-repo model instead of giving OQ-1's DSL surface a real answer.
> §4 G6 and the resolved OQ-1 below are that answer. See §5 for the new MGV-13..19 work
> items this generalization decomposes into.

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

### G6 — Credential decoupling: tokens as first-class objects, not repo-owned (resolves OQ-1)

**The gap.** MGV-10 (#1285) gave `additionalRepos` consumers a *read-only capability*, not a
*read-only credential*. Concretely: `clubhouse-site`'s `additionalRepos: [Clubhouse, Goobers]`
resolves its checkout token by `(owner, name)` lookup against those repos' **own** `RepoRef`
entries — the exact same `clubhouse_pat` / `dev5-github-token` their owning gaggles use to
push and open PRs. `AdditionalReadGrants` (`internal/credentials/scoping.go`) only ever
*emits* a `contents:read` capability grant for that consumer, never a write grant — but the
`Ref` it hands out points at the same fully-write-scoped secret. Read-only is enforced
**by construction in the routing code**, not because the credential material itself is
narrower. A leaked env var, log line, or workspace file from the site gaggle is a live,
fully write-capable token to Clubhouse or Goobers, not a read-only one. This is a real
defense-in-depth gap, not a hypothetical: it's the same class of blast-radius problem TBH-1
(`docs/design/trust-boundary-hardening.md`) exists to close for daemon-initiated mutations,
just on the credential-provisioning side instead.

**Why this wasn't caught by MGV-5/MGV-10 already.** `RepoRef` (`internal/instance/config.go`)
is strictly 1:1 with its credential — one `Token` or one `Auth` per repo entry — and
`GaggleSpec.AdditionalRepos` (`api/v1alpha1/gaggle_types.go`) carries no credential-override
field at all. There is currently no DSL surface to say "grant this gaggle repo X, but with
token Y" — only "grant this gaggle repo X" (implicitly: repo X's own token, capability-gated
after the fact). Capability scoping throughout `internal/credentials` is call-site-procedural
(a `Grant{Capability, Ref}` computed per resolution), not a property the credential object
itself declares — there is no `Credential.Scopes` concept anywhere in the codebase today.

**The fix: invert the ownership.** A credential should be a first-class object that declares
its own repo binding(s) and its own capability scope, independent of which `RepoRef` entry
"owns" it — not the other way around. This isn't a novel idea for this codebase:
`internal/githubapp/source.go`'s `Config.Repositories []string` already does exactly this for
GitHub App installation tokens (down-scoped **by GitHub's own API at mint time**, built for
MGV-5/#1012 for the identical reason — "a token leaked from one gaggle's stage cannot reach a
sibling gaggle's repo"). G6 generalizes that shape from App tokens to the general credential
model, PATs included:

- **Schema.** A top-level `credentials:` list (building on the existing per-capability
  `Credentials []CredentialGrant`, `internal/instance/config.go:75` — this is not starting
  from zero, that list already isn't repo-owned). Each entry: `name`, `repos: [{owner,name},
  ...]` (one or many — mirrors the App `Repositories` precedent directly), a declared
  capability/scope, and a source (PAT ref, App auth, future OIDC). A `RepoRef`'s existing
  single `Token`/`Auth` becomes sugar for an implicit single-repo credential entry —
  fully backward compatible for the one-repo-one-token case that works today.
- **`additionalRepos` gets an explicit, required `credential:` reference.** No implicit
  fallback to the referenced repo's own token. If a gaggle's `additionalRepos` entry doesn't
  name a credential explicitly scoped for that repo, `goobers validate` **rejects the config**
  — a schema-time failure, not a runtime capability-derivation nicety. This is what actually
  delivers "only use provided": the daemon should not be *able* to silently reuse a
  write-scoped token for a read-only reference the way it does today.
- **Keep `AdditionalReadGrants`'s existing in-code narrowing as a backstop, not a
  replacement.** Even a correctly-scoped read-only credential should never be routed a write
  capability by the runner — belt-and-suspenders, matching this codebase's established
  practice elsewhere (e.g. the merge-queue sibling-collision re-verify). A future bug widening
  the routing logic is still caught even if the credential itself is already narrow.
- **Out of scope for G6:** OS/store-side secret ACLs that deny cross-gaggle *resolution*
  outright — that's #685 (V2), tracked separately in G5's isolation-debt list above. G6 is
  entirely a V1, in-process schema and routing change.

**Relationship to other in-flight credential work.** UNOP-7 (#1295 and children #1779/#1780,
2026-07-27) is solving an adjacent problem — GitHub App vs. machine-account-PAT as the
*daemon's own* identity — with the same underlying shape: "the credential model needs to
express more than one flat secret per identity." G6 and UNOP-7 should share the App-token
minting/down-scoping plumbing where they overlap (§ MGV-16 below) rather than growing two
independent App-auth code paths.

**Blast radius.** The schema/resolution core (G6 itself, MGV-13/14/15) is **high** — it's the
credential path every gaggle runs on, same posture as MGV-5. The additive pieces that build on
it once the schema exists (App-token minting for read grants, migration diagnostics, the
conformance-test extension, docs) are **low** and independently shippable. See §5.

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
| MGV-10 | #1285 | Read-only reference-repo grants (`AdditionalReadGrants`, capability-level) | Med | **shipped** — the capability-routing half of G6; superseded as sole enforcement by MGV-13/14/15 |
| MGV-11 | #1286 | Read-only checkout of `AdditionalRepos` into a gaggle workspace | Low-Med | **shipped** |
| MGV-12 | #1287 | Live two-owner A+B multi-gaggle validation run | — | design's first-validation milestone; needs-human (operator must provision real repos+PATs) |
| MGV-13 | #1794 | G6 — credential schema: first-class `credentials:` list, repo(s)-bound not repo-owned | **High** (core creds, same posture as MGV-5) | supervised — not auto-approved |
| MGV-14 | #1795 | G6 — `additionalRepos` requires explicit `credential:`; `goobers validate` fails closed without one | **High + breaking** (changes accepted-config shape) | supervised — not auto-approved, PO review for the breaking change |
| MGV-15 | #1796 | G6 — `AdditionalReadGrants` consumes the explicit credential when present; write-grant backstop retained regardless | **High** (core creds path) | supervised — not auto-approved |
| MGV-16 | #1797 | G6 — generalize `internal/githubapp` down-scoped token minting as a provisionable read-only credential source (shares plumbing with UNOP-7/#1780) | Low (additive, net-new capability) | **approvable** once MGV-13 lands |
| MGV-17 | #1798 | G6 — `goobers validate`/`lint` migration diagnostic: warn (not yet fail) on `additionalRepos` entries lacking an explicit credential, ahead of MGV-14 | Low (additive, non-disruptive) | **approvable** |
| MGV-18 | #1799 | G6 — isolation-conformance test extension (builds on MGV-9): real distinct read-credential bytes, no derivable write grant even adversarially, leaked read-credential can't authenticate a push | Low (test-only) | **approvable** |
| MGV-19 | #1800 | G6 — instance-config authoring docs for the new credential model, worked `clubhouse-site` example | Low (docs-only) | **approvable** |

> **Isolation debt (not work items in this sprint, tracked as outstanding security posture, §4.5 G5):** OS-native sandbox rungs #165/#166/#167 (#35), and per-gaggle workload identity + store secret ACLs #685 (V2). MGV-9 proves *scoping*; these enforce it.
>
> **G6 sequencing:** MGV-13 (schema) → MGV-14 (required field + fail-closed validation) →
> MGV-15 (grant routing consumes it). MGV-16/17/18/19 are independently shippable once MGV-13
> lands; none of them depend on MGV-14/15. MGV-17 (migration warning) should land *before*
> MGV-14 flips validation to fail-closed, so existing instance configs get advance notice
> rather than a surprise break. None of G6 touches the workflow-authoring DSL
> (`internal/workflow/v_current`/`v_next`) — this is instance/gaggle config schema
> (`api/v1alpha1/gaggle_types.go`, `internal/instance`) only.

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

- **OQ-1 — credential config surface — resolved (2026-07-27), see §4 G6:** a top-level
  `credentials:` list, each entry bound to one or more repos and declaring its own capability
  scope — not a per-repo-entry token and not a per-gaggle block. A `RepoRef`'s existing single
  `Token`/`Auth` is sugar for an implicit single-repo credential (fully backward compatible).
  `additionalRepos` entries require an explicit `credential:` reference, fail-closed at
  `goobers validate` time if absent. The original recommendation below (token on each repo
  entry) is what MGV-10 effectively built and is what created the gap G6 fixes — a
  repo-owned token can't express "this consumer gets a different, narrower credential than
  the repo's own." *Original recommendation, superseded: token on each repo entry + optional
  gaggle block; top-level stays the shared default.*
- **OQ-2 — CI command surface:** on `GaggleSpec` or per-workflow input? *(Recommend:
  `GaggleSpec` default, overridable per workflow.)*
- **OQ-3 — multi-repo-per-gaggle CI:** when a gaggle spans repos (server+client), is CI one
  command over the checked-out set, or per-repo? *(Recommend: one gaggle command over the
  workspace; revisit if a real case needs per-repo.)*
- **OQ-4 — different-owner PAT trust:** confirm a fine-grained PAT scoped to one repo under a
  distinct owner is an acceptable trust boundary for an unattended daemon.
