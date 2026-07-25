# Design: Trust-boundary hardening — proposal/executor split, staged mode, integrity labels

> Status: **TBH-1 phase-0 RFC complete; human design gate pending — prescriptive** ·
> Area prefix: `TBH` · Milestone: **V1 — arbitrary repos, teams, hardening**
> Origin: the comparative-security review of GitHub Agentic Workflows
> (`~/source/Goobers-Review/GH-AW-VS-GOOBERS.md`), whose central finding names our gap
> precisely: *capability-scoped credential injection is not enough when the same
> unconstrained agent process receives the write credential.* Also the arbitrary-repo
> onboarding goal (`docs/guides/arbitrary-repo-onboarding.md`) and SEC-047's
> untrusted-input doctrine.
> Builds on (all landed): capability registry + fail-closed admission
> (`internal/capability`), capability-scoped credential non-injection
> (`internal/credentials`), per-goober scoping (#823), sandbox mechanism ADR-0001 +
> native implementations (`internal/sandbox`), journal scrubbing (SEC-041).
> Companions: `docs/design/v1/35-sandboxing-per-goober-creds.md` (S2–S4),
> `docs/design/v1/34-arbitrary-repo-hardening.md`, `docs/requirements/security.md`.

## 1. The gap, stated plainly

Today, when an agentic stage declares `github:pr:write`, the harness process receives a
credential that can perform *any* `github:pr:write` operation, and the agent decides
which calls to make. Capability admission bounds *which credential* is injected — it does
not bound *what the process does with it*. On our own repo, with one operator, that risk
is accepted. Pointed at a repo the operator does not own — the V1 promise — it is the
difference between "an agent proposed a bad merge and we caught it" and "an agent merged."

gh-aw's architecture demonstrates the fix shape at production scale: the agent runs
**read-only**; its intended side effects leave as **typed, validated proposals**; a
**trusted deterministic executor** — the only holder of write credentials — validates
and performs them. We adopt that boundary, adapted to Goobers' stage/journal model
(no Actions coupling, proposals journaled as artifacts, provider-neutral types).

Three properties this design holds fixed:

1. **The agent process never holds a mutating credential** for capabilities routed
   through the proposal boundary.
2. **Every mutation has a durable journal lifecycle**: the proposal artifact records
   what was asked, by whom, and from what evidence; a fsynced pre-call barrier and a
   terminal execution event record what was attempted and what was done or refused.
   Recovery reconciles an unterminated call and never blindly replays it.
3. **Adoption is per-capability and incremental** — no big-bang rewrite of the stage
   contract; un-migrated capabilities keep today's direct-injection behavior until
   migrated.

## 2. Workstreams

### TBH-1 — Mutation-proposal envelope + trusted executor

- The normative phase-0 contract is
  [`stage-contract.md` §“TBH-1 mutation proposals”](../stage-contract.md#tbh-1-mutation-proposals-design-target-not-implemented).
  It defines the closed proposal set, four initial typed kinds, canonical-capability
  mapping, fixed bounds, scope/allowlist checks, idempotency, execution ordering,
  journal events, and refusal disposition.
- Proposals use the existing `ResultEnvelope.artifacts` →
  `InvocationEnvelope.contextPointers` path. There is no new IPC mechanism and no
  proposal body in scalar `outputs`. A singular static `proposalInput` edge names the
  producer; the runner fsyncs the exact producer attempt and artifact digest before
  credentialing the executor, so paths and media metadata are not authorization.
- `kind: apply-proposals` is built-in trusted Go code, not an arbitrary command. It
  is the only stage that receives a migrated writer credential. The producer still
  declares the canonical capability as proposal authority but receives no credential
  for it.
- Refusal is fail-closed and first-class: `proposal.refused` plus a `blocked`
  `PROPOSAL_REFUSED` result routes through the existing escalation disposition.
- Explicit non-adoptions from the gh-aw review: no auto-enabled default mutations, no
  probabilistic threat-detection as an authorization gate, no runtime-editable prompt
  surface outside the run pin.

#### TBH-1 decisions

1. **The proposal vocabulary names intent, not credential aliases.** The initial
   kinds are `repo:merge`, `github:pr:close`, `repo:push`, and
   `github:issues:write`. They map respectively to the existing canonical
   capabilities `github:pr:merge`, `github:pr:write`, `repo:push`, and
   `github:issues:write`. This avoids weakening the canonical registry with duplicate
   names while keeping close narrower than the credential that implements it.
2. **Validation is trusted Go code (resolves TBH-Q1).** Phase 0 does not add a
   workflow-authored constraint DSL. Repository, item/PR, and branch allowlists are
   derived from the pinned run identity, `repoRef`, the claimed item or a trusted
   deterministic selector, and the run branch namespace. Fixed limits are ceilings,
   not defaults a workflow may raise. A future declarative vocabulary may only
   narrow these rules and requires a separate design gate.
3. **Existing reviewer gates stay evidence-based (resolves TBH-Q2).** The
   implementation reviewer continues to review the diff and evidence it reviews
   today. It is not an authorization oracle and does not consume proposals by
   default. For merge/close, a trusted deterministic adapter maps the existing pinned
   verdict into a typed proposal after review. A workflow that needs human or agentic
   review of mutation intent may add an explicit proposal-review gate, but passing
   that gate never bypasses deterministic validation.
4. **A proposal set preflights atomically, then executes with durable per-call
   guards.** One preflight refusal prevents every mutation in the set. Freshness is
   re-checked immediately before each provider call, then an execution-started event
   is fsynced before the call; a later mismatch stops the remaining proposals but
   does not roll back confirmed issue writes. An unterminated call is reconciled
   read-only and becomes ambiguous unless an exact effect or an enforceable provider
   idempotency key proves a safe outcome. Merge, close, and push cannot be batched
   with another proposal.

#### TBH-1 migration

Migration is capability-by-capability and pinned by workflow/DSL version. A run
cannot switch routing mode after it starts, and no stage may retain both direct and
proposal-routed authority for the same capability.

| Order | Surface | Migration |
|---|---|---|
| 1 | Merge and PR close | Extract the credentialed provider handlers from `merge-pr` and `apply-verdict` behind `apply-proposals`. Their deterministic verdict/SHA checks become proposal production or executor validation as appropriate; compatibility entrypoints remain only for workflow versions still on direct injection. |
| 2 | Push | Agentic implementation/remediation stages commit without a push credential and propose the exact source SHA and run-branch ref. The deterministic push stage requires that SHA to equal the runner-pinned, reviewed current branch tip (and requires fast-forward ancestry in fast-forward mode), then applies it after namespace and remote-SHA checks; rebased remediation uses provider-native force-with-lease against that exact remote SHA, never unconditional force. |
| 3 | Issue writes | Curation/nomination stages emit bounded create/edit/comment/label/state proposals; the executor confines every operation to `repoRef` and the claimed issue except for bounded creation in that repository. |

Until a row is migrated, its canonical capability keeps today's direct-injection
behavior. A row is complete only when the compiler rejects mixed routing, every
agentic producer is credentialless for that capability, the built-in executor is the
sole credential recipient, and refusal/escalation coverage is present. This preserves
incremental delivery without claiming the boundary before it exists.

### TBH-2 — Staged / dry-run mode

- `goobers up --staged` (and per-workflow `mode: staged`): the full loop runs, but the
  proposal executor journals **previews** instead of executing — no writer credential
  is materialized anywhere in the instance.
- `goobers staged list|show|promote` reviews accumulated previews; `promote` executes
  a selected proposal set under the normal validation path.
- This is the onboarding story for every new repo/team: first week staged, then
  per-capability promotion to live. Pairs with `arbitrary-repo-onboarding.md`.
- Prerequisite: TBH-1's envelope (a preview is a proposal not yet executed) — but a
  useful staged-lite (journal intended mutations from today's deterministic stages)
  can ship earlier for the merge/close capabilities.

### TBH-3 — Sandbox enforcement + egress posture (completes SEC-044, extends ADR-0001)

- Turn the landed native sandboxes (`internal/sandbox`: sandbox-exec / Linux
  implementation) **on by default** for agentic stages: worktree-scoped writes,
  default-deny env passthrough (explicit allowlist), no `$HOME` exposure beyond the
  harness's own auth needs — each opt-out journaled.
- Egress: per-goober domain allowlist (provider + harness endpoints by default) with a
  journaled network audit trail; mechanism per platform chosen under ADR-0001's
  framework (proxy-based first — works everywhere; kernel-level where available).
  Hardened-distro caveat from the cross-platform work (userns) applies and is
  documented per-platform.
- Note: TBH-1 reduces what egress control must catch (exfil of a write credential is
  moot when the process never holds one) — sandbox and proposal boundary are
  complementary layers, not alternatives.

### TBH-4 — Input-integrity labels

- Provenance grade carried on snapshots, context pointers, artifacts, and provider
  reads: `trusted` (operator/config), `maintainer` (trust-labeled backlog item per
  SEC-047), `unapproved` (arbitrary issue/PR body), `derived` (agent output).
- Consumers declare the minimum integrity they accept; the compiler validates (same
  fail-closed shape as capability admission). First consumer: the implement stage's
  invocation envelope distinguishes maintainer-approved task text from unapproved
  comment threads.
- This generalizes SEC-047 from "a label gates eligibility" to "provenance flows with
  the data" — the prerequisite for public-repo backlogs and mixed-mode (#804).

## 3. Phasing

| Phase | Ships | Gate |
|---|---|---|
| 0 (design) | TBH-1 envelope schema + executor contract RFC'd against stage-contract.md | **Pending:** PO + second-reviewer sign-off (architectural blast radius); no implementation issue may land first |
| 1 | TBH-1 for merge/close; TBH-2 staged-lite on those capabilities | Selfhost runs with merge/close behind proposals for a full watched round |
| 2 | TBH-3 sandbox-on-by-default; TBH-1 push/issue migration | Zero un-journaled opt-outs on selfhost |
| 3 | TBH-2 full staged mode; TBH-4 integrity labels | Stranger-repo pilot onboards in staged mode |

The phase-0 sign-offs belong on the RFC change review so they identify the exact
revision approved. Filing or approving an implementation issue is not a substitute.
Until both are present, this document authorizes design review only.

## 4. Resolved and open questions

- **TBH-Q1 — resolved:** Per-kind Go validators are the phase-0 authorization
  vocabulary. There is no Tutor-editable constraint block. See TBH-1 decision 2.
- **TBH-Q2 — resolved:** Existing reviewer gates remain diff/evidence-based; proposals
  are deterministically produced after the applicable verdict. Explicit
  proposal-review gates are optional defense in depth, never authorization. See TBH-1
  decision 3.
- **TBH-Q3:** Integrity-label persistence — envelope field vs journal event attribute vs
  both? Interacts with the conformance surface (§3.3 ARCHITECTURE) — labels must not
  become a runner-specific divergence.
- **TBH-Q4:** How does staged mode interact with the merge queue (a "promote" that lands
  a week-old proposal must revalidate freshness — reuse the elect-lander staleness
  checks?).
