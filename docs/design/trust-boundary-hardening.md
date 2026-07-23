# Design: Trust-boundary hardening — proposal/executor split, staged mode, integrity labels

> Status: **Draft for review — prescriptive** · Area prefix: `TBH` · Milestone: **V1 —
> arbitrary repos, teams, hardening**
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
2. **Every mutation is journaled twice**: as the proposal artifact (what was asked,
   by whom, from what evidence) and as the execution event (what was done, or why
   refused). Fail closed, carry the cause.
3. **Adoption is per-capability and incremental** — no big-bang rewrite of the stage
   contract; un-migrated capabilities keep today's direct-injection behavior until
   migrated.

## 2. Workstreams

### TBH-1 — Mutation-proposal envelope + trusted executor

- A typed proposal schema per mutating capability (start: `repo:merge`,
  `github:pr:close`, `repo:push`, `github:issues:write`), carried as a result-envelope
  artifact (`proposals/` pointer) — additive to the existing stage contract
  (`docs/stage-contract.md`), not a new IPC mechanism.
- A deterministic **executor stage kind** (`kind: apply-proposals`) that validates
  proposals against: declared capability scope, target allowlists (repo/branch
  patterns), size/count limits, and the run's own claimed scope — then executes with
  the writer credential only it receives.
- Refusals are first-class: a refused proposal journals a typed reason and routes to
  the existing escalation ladder; it never silently drops.
- Migration order: merge/close first (highest blast radius, already deterministic
  stages — `merge-pr`, `apply-verdict` become the first executors), then push, then
  issue mutations. Agentic stages that today call `gh` directly for these lose the
  credential and gain the proposal path, capability by capability.
- Explicit non-adoptions from the gh-aw review: no auto-enabled default mutations, no
  probabilistic threat-detection as an authorization gate, no runtime-editable prompt
  surface outside the run pin.

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
| 0 (design) | TBH-1 envelope schema + executor contract RFC'd against stage-contract.md | PO + second-reviewer sign-off (architectural blast radius) |
| 1 | TBH-1 for merge/close; TBH-2 staged-lite on those capabilities | Selfhost runs with merge/close behind proposals for a full watched round |
| 2 | TBH-3 sandbox-on-by-default; TBH-1 push/issue migration | Zero un-journaled opt-outs on selfhost |
| 3 | TBH-2 full staged mode; TBH-4 integrity labels | Stranger-repo pilot onboards in staged mode |

## 4. Open questions

- **TBH-Q1:** Proposal validation vocabulary — per-capability Go validators only, or a
  declarative constraint block in the workflow DSL (max-diff-size, branch patterns)?
  (Declarative is Tutor-editable, which is both the appeal and the risk.)
- **TBH-Q2:** Does the reviewer gate consume proposals (verdict over intended mutations)
  or stay diff-based with proposals validated after? (gh-aw validates after; our
  agentic reviewer could do better by seeing intent.)
- **TBH-Q3:** Integrity-label persistence — envelope field vs journal event attribute vs
  both? Interacts with the conformance surface (§3.3 ARCHITECTURE) — labels must not
  become a runner-specific divergence.
- **TBH-Q4:** How does staged mode interact with the merge queue (a "promote" that lands
  a week-old proposal must revalidate freshness — reuse the elect-lander staleness
  checks?).
