# Design: Trust-boundary hardening — proposal/executor split, staged mode, integrity labels

> Status: **draft — TBH-1 phase-0 RFC complete; human design gate pending; prescriptive** ·
> Area prefix: `TBH` · Milestone: **Trust & Isolation**
> ([#25](https://github.com/Agent-Clubhouse/Goobers/milestone/25))
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
- Refusal and non-atomic completion are fail-closed and first-class:
  `proposal.refused`, `proposal.partially_applied`, and
  `proposal.execution_ambiguous` each return a distinct `blocked` result through
  the existing escalation disposition.
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
   Issue-write proposals cannot mutate trust/eligibility labels; applying
   `goobers:approved` remains the separate `github:issues:approve` authority and is
   not part of the initial issue-write route.
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
4. **A proposal set preflights atomically, then executes with provider-realizable
   per-call guards.** One preflight refusal prevents every mutation in the set.
   Immutable authority guards are atomic where the provider must prevent object
   substitution: GitHub merge/enqueue pins the reviewed head and push pins the
   remote ref. GitHub exposes no atomic base-SHA, PR-close head-SHA, or issue
   resource-version condition, so those are explicitly observational freshness
   guards checked under a shared runner target lease immediately before narrow,
   typed provider calls. They detect stale intent but do not authorize or widen
   the target. An execution-started event is fsynced before each call; a later
   mismatch stops the remaining work but does not roll back confirmed effects.
   Existing-target issue proposals refresh `updatedAt` between ordered operations.
   A confirmed prefix followed by rejection is
   `proposal.partially_applied`, never `proposal.refused`; an unterminated call is
   reconciled read-only and becomes ambiguous unless an exact effect or enforceable
   idempotency key proves a safe outcome. Merge, close, and push cannot be batched
   with another proposal.

#### TBH-1 migration

Migration is capability-by-capability and pinned by workflow/DSL version. A run
cannot switch routing mode after it starts, and no stage may retain both direct and
proposal-routed authority for the same capability.

| Order | Surface | Migration |
|---|---|---|
| 1 | Merge and PR-write routing | Move merge and close behind `apply-proposals`. The trusted merge producer preserves workflow-pinned or synthesized structured commit title/message; the executor detects direct-vs-merge-queue policy live, applies the provider's atomic expected-head guard, and reports `merged` separately from `enqueued` so the existing queue watcher retains merge/eviction/timeout routing. Moot close requires its bounded explanatory comment and posts it before closing; a confirmed comment followed by close rejection is partial. Because close shares the broad `github:pr:write` grant with open/update/list/poll, migrate that whole credential route at once: close is exclusive to the proposal executor, while every remaining operation is assigned to a runner-owned, method-scoped provider adapter that cannot call close. No agentic or arbitrary-command stage receives the broad PR-write credential. Compatibility entrypoints remain only for workflow versions pinned to direct injection before migration. |
| 2 | Push | Agentic implementation/remediation stages commit without a push credential and propose the exact source SHA and run-branch ref. The deterministic push stage requires that SHA to equal the runner-pinned, reviewed current branch tip (and requires fast-forward ancestry in fast-forward mode), then applies it after namespace and remote-SHA checks; rebased remediation uses provider-native force-with-lease against that exact remote SHA, never unconditional force. |
| 3 | Issue writes | Migrate the whole canonical route, not only curation/nomination. Their agent-authored create/edit/comment/label/state intents become bounded proposals, confined to `repoRef` and claimed or trusted-selector issue scope and barred from trust labels. Runner bookkeeping stays on closed method-scoped adapters: claim/release/reconciliation preserves the claim-ledger lock, provider winner election, losing-reservation rollback, and release ordering; backlog metadata repair preserves ledger/provider drift correction; disposition preserves implementation close-out, parking, escalation/status, queue outcome, and post-merge closure; PR-lifecycle bookkeeping preserves verdict/status comments, merge refusal/demotion, fan-out/unparking/healing, and remediation checkpoint/rebase/update/publish/finding-response paths. Read-only consumers receive no writer token. The new routing version is invalid until every shipped/example workflow, scheduler/gate hook, terminal path, and compatibility entrypoint is assigned exactly once. |

Until a row is migrated, its canonical capability keeps today's direct-injection
behavior. A row is complete only when the compiler rejects mixed routing, every
agentic producer is credentialless for that capability, every mutating operation has
exactly one trusted route, only the proposal executor receives a handle for
proposal-routed operations, and refusal/escalation coverage is present. A split
canonical capability such as `github:pr:write` or `github:issues:write`
additionally requires all non-proposal operations to use closed runner-owned
adapters; leaving a direct broad credential on any task process leaves the row
incomplete. This preserves incremental delivery without claiming the boundary
before it exists.

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
| 0 (design) | TBH-1 envelope schema + executor contract RFC'd against stage-contract.md | PO sign-off (architectural blast radius); no implementation issue may land first until recorded |
| 1 | TBH-1 for merge/close; TBH-2 staged-lite on those capabilities | Reference workflow runs with merge/close behind proposals for a full watched round |
| 2 | TBH-3 sandbox-on-by-default; TBH-1 push/issue migration | Zero un-journaled opt-outs on reference-workflows |
| 3 | TBH-2 full staged mode; TBH-4 integrity labels | Stranger-repo pilot onboards in staged mode |

The phase-0 sign-off is recorded on this sign-off PR, which pins the RFC revision
presented for approval to merge commit
[`b741345aa6de32cff59a9f848d209845d27c6984`](https://github.com/Agent-Clubhouse/Goobers/commit/b741345aa6de32cff59a9f848d209845d27c6984)
(PR #1613). Filing or approving an implementation issue is not a substitute.

A second-reviewer sign-off is not required for this repo at this stage (2026-07-27
ruling) — this repo does not currently have a second reviewer available. Revisit this
gate if/when it does; until then, PO sign-off alone authorizes phase-1 implementation.

| Phase-0 sign-off role | Name | Date |
|---|---|---|
| Product owner (PO) | Mason Allen | 2026-07-27 |

Once the PO sign-off row above contains a name and date, this document authorizes
phase-1 implementation (TBH-1 for merge/close, TBH-2 staged-lite), not just design
review.

## 4. Resolved and open questions

- **TBH-Q1 — resolved:** Per-kind Go validators are the phase-0 authorization
  vocabulary. There is no Tutor-editable constraint block. See TBH-1 decision 2.
- **TBH-Q2 — resolved:** Existing reviewer gates remain diff/evidence-based; proposals
  are deterministically produced after the applicable verdict. Explicit
  proposal-review gates are optional defense in depth, never authorization. See TBH-1
  decision 3.
- **TBH-Q3 — resolved:** Persist integrity in both the stage-contract envelope and
  conformance-normative journal records. Backlog items, context pointers, and
  artifact pointers carry the grade used for pre-dispatch admission; immutable
  input refs plus `artifact.recorded` and typed refusal events
  preserve the same decision for replay and cross-runner comparison. Integrity is
  never a `runner.*` annotation.
- **TBH-Q4:** How does staged mode interact with the merge queue (a "promote" that lands
  a week-old proposal must revalidate freshness — reuse the elect-lander staleness
  checks?).
