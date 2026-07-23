# Spec: PR lifecycle & merge loop

> Status: **Draft** · Aligned to ../ARCHITECTURE.md (2026-07-12) · Derives from
> ../design/v0/pr-lifecycle-loop.md (ratified decisions), ../design/v0/pr-remediation-capability.md,
> ../design/sibling-pr-sequencing.md · Area prefix: PRL

## Purpose

The **PR lifecycle loop** is the closed loop that takes an open pull request to
**merged or explicitly escalated** — closing "issue → PR" into "issue → merged
change" (V0.5's north star). It is the one subsystem whose contracts live at the
**cross-PR altitude**: which PR is looked at next, how mutually-colliding PRs are
sequenced, when the machine may merge or close a PR on its own, and when a human
must be pulled in.

## Model

Three workflows at two altitudes, composed **only through durable state on the
PR** (labels + sticky comments carrying machine-readable payloads) — never by one
workflow invoking another:

| Workflow | Altitude | Role |
|---|---|---|
| `implementation` | one issue → one diff | executor; opens the PR (out of scope here — see workflow/task specs) |
| `merge-review` | the whole open-PR set | **decider** — select one PR, review it with every sibling's state as evidence, emit a Verdict, and (capability-gated) land it |
| `pr-remediation` | one existing PR | **executor** — rebase/rework a flagged PR against the verdict and re-push |

Label contract (shipped): `goobers:merge-ready`, `goobers:needs-remediation`,
`goobers:blocked-on-sibling`, `goobers:merge-escalated`, `goobers:merge-demoted`.
Every label is paired with a machine-readable state payload (verdict-json /
blocked-on-sibling / remediation-state / merge-demotion sticky comments), which —
not the label — is the source of truth for self-heal checks.

The loop's ratified operating decision (**G2**, pr-lifecycle-loop.md §1) is that
**full-auto merge is in scope**: the machine merges its own work end-to-end with
no human in the critical path, behind an explicit per-workflow capability grant
and a conjunctive safety gate, while a human can look in, override, and pause.

## Scope & relationship to other specs

- The loop's workflows are **ordinary workflows**: their stage/gate/journal/
  crash-recovery semantics are owned by `workflow.md` (WF-002/WF-050…),
  `gate.md` (GT-002/GT-010/GT-011), and `task.md`. Nothing here redefines them.
- Per-run PR claiming reuses the scheduler's lease-based claim contract; owning
  requirement: `SCH-020` — PRL requirements below only fix *which namespace* is
  shared and *what selection order* feeds the claim.
- Capability grants (`github:pr:merge`, `github:pr:review`, `repo:push`,
  `github:branch:delete`) follow the security spec's fail-closed capability
  model; this spec owns only *which stages may hold which grants*.
- What this spec **owns** and no other spec does: cross-PR selection/fairness,
  the Verdict handoff contract, single-lander election, autonomous close and
  merge authority, the remediation loop-control state, the escalation ladder and
  its self-heal ("drainage") semantics, and post-merge fan-out/close-out.
- Forward designs stay prescriptive and are **referenced, not duplicated**:
  `../design/sibling-pr-sequencing.md` (S1–S4 have shipped; its §4.5 policy-seam
  opt-in framing remains the prescriptive contract) and
  `../design/v0/pr-remediation-capability.md` (PRR: brief gatherers beyond
  `gather-pr-context`, declared remediation policy, taxonomy split).

## Requirements

### PR selection & fairness

- **PRL-001 (MUST, Shipped):** `merge-review` MUST select **at most one PR per
  run** (never a batch pass over the open-PR set), from open, non-draft PRs on
  the configured base whose head matches the configured goober branch namespace
  (default `<namespace>implementation/`), with passing CI. *(All tiers)*
- **PRL-002 (MUST, Shipped):** The selected PR MUST be **leased in a claim
  namespace shared by `merge-review` and `pr-remediation`**, so the two
  workflows can never concurrently act on the same PR; distinct-PR concurrency
  is permitted (claiming semantics per `SCH-020`).
- **PRL-003 (MUST, Shipped):** Selection MUST exclude a PR already carrying an
  in-flight decision (`goobers:merge-ready`, `goobers:needs-remediation`), and
  MUST exclude escalated, demoted, and sibling-blocked PRs **only via their
  liveness checks** (PRL-062/PRL-063), never via a permanent label test — an
  exclusion that cannot self-heal is forbidden (#716).
- **PRL-004 (MUST, Shipped):** Among eligible PRs, selection order MUST be
  deterministic: descending count of open PRs blocked on the candidate (so a
  cluster's lander is reviewed before its parked dependents), then ascending PR
  number (FIFO). The first unclaimed candidate in that order is selected.
- **PRL-005 (SHOULD, V1):** Selection priority SHOULD become configurable
  (label-driven / user-defined ordering) rather than FIFO-only; #509 owns the
  design. The PRL-004 order is the shipped default, not a ceiling.
- **PRL-006 (MUST, Shipped):** A cycle with no eligible or claimable PR MUST be
  a normal no-work outcome (exit 0), never a failure.

### Sibling context & the verdict contract

- **PRL-010 (MUST, Shipped):** Before review, the loop MUST gather **every
  other open goober PR** on the same base — files touched, head SHA, draft
  state, labels, check state — regardless of that sibling's own eligibility (a
  draft or red sibling is still evidence). This is what lifts the review above
  single-diff altitude.
- **PRL-011 (MUST, Shipped):** The gatherer MUST compute the **deterministic
  file-overlap set** (selected PR's files ∩ each sibling's files, #989) and
  emit the overlapping sibling numbers as a scalar output threaded to election
  and verdict application — cross-PR sequencing MUST have a ground-truth
  backstop that does not depend on the LLM reviewer noticing the collision.
- **PRL-012 (MUST, Shipped):** The holistic review MUST emit a structured
  **Verdict**: `decision` (`pass` | `needs-changes` | `fail`), classed findings
  (`rebase-needed`, `conflict`, `substantive`, `cross-pr-blocked` — the last
  carrying `BlockingPRs`), summary/rationale, and a **SHA pin**
  (`headSha`, `baseSha`) plus review digest and source run id. The prose PR
  comment is a projection of this artifact, never a second source of truth.
- **PRL-013 (MUST, Shipped):** Every verdict is **SHA-pinned** (design D6): no
  stage may act on a verdict whose pin no longer matches the PR's live
  head/base. A stale pin **voids** the verdict — a normal outcome (no comment,
  no label, exit 0), re-reviewed next cycle — never an error and never a merge.
- **PRL-014 (SHOULD, Shipped):** Re-review of an unchanged
  `(head, base, sibling-set)` SHOULD be skipped via the review-digest verdict
  cache (#523); a cache hit reuses the prior verdict unchanged, including its
  digest and source run id.
- **PRL-015 (MUST, Shipped):** The agentic reviewer gate MUST retry a
  transient evaluator-harness failure within declared bounds
  (`retry.maxAttempts`) instead of failing the run on first occurrence (#765);
  non-transient errors still fail fast. (Gate mechanics owned by `GT-011`.)

### Election: single lander, policy seam

- **PRL-020 (MUST, Shipped):** When a `needs-changes` verdict consists
  **entirely** of cross-PR-ordering findings (after folding in the PRL-011
  overlap backstop, #990), the loop MUST treat the cluster as a **sequencing
  problem, not a reconciliation problem**: elect exactly one lander and park
  the rest, rather than asking an agent to reconcile overlapping diffs.
- **PRL-021 (MUST, Shipped):** **Single-lander invariant:** the election is a
  pure, deterministic function of `{selected PR, blocker set, policy}` so every
  cluster member independently computes the **same** winner — exactly one
  member is crowned, with no central coordination. A verdict carrying any real
  defect (substantive/conflict/rebase-needed finding) is **never electable**.
- **PRL-022 (MUST, Shipped):** The election policy MUST be a **workflow-
  configurable seam** (#834): `fifo` (lowest PR number — default) and `newest`
  ship as pure policies; cluster-data policies (`most-blockers`,
  `fewest-overlaps`, #1028/#1029) resolve from the live open-PR set. An
  unknown policy name falls back to `fifo` with a logged warning, never a
  pipeline failure. `elect-lander` and `apply-verdict` MUST be configured with
  the **same** policy and MUST derive identical decisions from identical
  inputs (a pinned test enforces agreement).
- **PRL-023 (MUST, Shipped):** Election means "those siblings stop counting as
  blockers", **not** a separate merge authority: the crowned lander's verdict
  is resolved into a **derived, published `pass`** (rationale stating the
  policy and the rule), and the PR reaches merge through the same
  published-verdict path as every other pass — never a gate bypass.
- **PRL-024 (MUST, Shipped):** A parked (non-elected) member MUST be labeled
  `goobers:blocked-on-sibling` with a recorded blocker set narrowed to its
  **predecessors under the election order only** (#991) — never the symmetric
  overlap union, which deadlocks any cluster of 3+.
- **PRL-025 (MUST, Shipped):** The **zero-winner case** — the deterministic
  policy winner carries a real defect while every green sibling defers to it —
  MUST be detected and published as an explicit `fail` escalation naming the
  cluster, instead of silently splitting the cluster between parked and
  remediation states.
- **PRL-026 (MUST, Shipped):** A **demoted** PR (PRL-046) MUST be excluded
  from candidacy and dropped from every sibling's blocker set, so the
  next-eligible member wins and the cluster drains around the stuck lander
  (#950). Demotion-state resolution failures fail **open** (treated as
  not-demoted) — the demotion signal must never itself become a merge outage.

### Verdict application & autonomous close

- **PRL-030 (MUST, Shipped):** A valid (non-void) verdict MUST be published as
  a **native platform review** pinned to the current head plus a single
  reconciled sticky status comment embedding the verdict JSON; when the
  platform refuses a self-authored review (single-identity instance, #870) the
  comment/label handoff alone MUST carry the verdict — degraded, not failed.
  Concurrent runs MUST converge to one canonical status comment.
- **PRL-031 (MUST, Shipped):** Decision→label routing: `pass` →
  `goobers:merge-ready` path (merge conjuncts, PRL-040); `needs-changes` →
  `goobers:needs-remediation`, or `goobers:blocked-on-sibling` when findings
  are entirely cross-PR (PRL-024); `fail` → `goobers:merge-escalated` (a
  wrong approach is a human's call, never burned on remediation budget — D2).
  An empty-findings `needs-changes` routes to remediation, not to parking.
- **PRL-032 (MUST, Shipped):** The loop MAY **autonomously close** a PR only
  under one of three predicates (#923/#987/#1211, PR #1256), each established
  by a **deterministic, independently-verifiable repository fact** — never the
  reviewer's prose — and all three **fail closed** on any provider error or
  unverifiable input:
  1. **Moot:** the PR's diff against base is empty, or every issue it exists
     to resolve is already closed.
  2. **Shared-issue duplicate:** an earlier (lower-numbered) open goober PR
     already references one of the same issues.
  3. **Byte-identical superseded:** an earlier open sibling proposes the
     byte-identical diff (length-prefixed digest over every file's path,
     status, and verbatim patch; any omitted patch makes identity
     unverifiable → no close).
- **PRL-033 (MUST, Shipped):** Autonomous close MUST only ever close the
  **later** PR (the earlier claim wins, consistent with FIFO election), MUST
  **never intercept a `pass`** (a passing PR merges and wins), and MUST post a
  close comment stating the objective reason, with the reviewer's rationale
  quoted as explanation only. This close authority lives in the decider
  (`apply-verdict`), not in remediation.

### Merge execution

- **PRL-040 (MUST, Shipped):** Merging MUST be gated on the explicit
  **`github:pr:merge` capability**, granted per workflow (instance opt-in —
  the G2 decision). Absent the grant, the merge stage refuses before polling
  any state (fail-closed); no other stage in the loop holds merge authority,
  and `merge-review` never holds `repo:push`.
- **PRL-041 (MUST, Shipped):** A merge MUST proceed only when **every
  independent conjunct holds against a live re-poll** (never a caller claim):
  verdict = `pass`; CI green — where the provider's `mergeable_state:
  unstable` (only non-required checks red, #961) also satisfies CI; not a
  draft; head SHA equals the pin; and base SHA equals the pin **or** the base
  movement is provably disjoint from the PR's own files (#718's delta-aware
  check; an undeterminable intersection is treated as stale). A failed
  conjunct is a normal refusal — exit 0, `merged=false`, human-readable
  `reason` — not a stage failure.
- **PRL-042 (MUST, Shipped):** The whole poll→decide→land window MUST be
  serialized instance-wide (a local file lock, #719) so concurrent runs
  reviewing distinct PRs cannot both pass the SHA-pin conjunct against a
  pre-merge base. Review work ahead of the window is intentionally parallel.
- **PRL-043 (MUST, Shipped):** Landing MUST dispatch through the
  **merge-policy seam** (`internal/mergepolicy`, #758): direct-merge vs
  merge-queue-enqueue, detected per repo/branch from live protection/ruleset
  state (cached), resolved under the same lock. The two outcomes (`merged` /
  `enqueued`) MUST stay distinct in stage outputs and gate routing — enqueued
  is never conflated with merged. An enqueue the queue completes immediately
  reports `merged`.
- **PRL-044 (MUST, Shipped):** The default merge commit message MUST be built
  from the **pinned pass verdict** on the locked poll's own comment snapshot
  (title + summary/rationale + `Closes #N` footers) — attributable to the
  verdict author, never re-fetched outside the lock.
- **PRL-045 (MUST, Shipped):** An enqueued PR MUST be **watched** to one of
  three determined outcomes: `merged` (same post-merge path as a direct
  merge), `evicted`, or `timeout` — eviction and timeout label the PR
  `goobers:needs-remediation` with an explanatory comment, and a failure to
  leave that trail is a stage failure, not a warning. A timeout additionally
  enters a durable **reconciliation ledger**; a bounded next-run sweep runs
  the normal post-merge path if the queue landed the PR after watching
  stopped, and completed entries are durably skipped (idempotent).
- **PRL-046 (MUST, Shipped):** Every real (non-advisory) merge refusal MUST be
  recorded against a durable per-PR counter **keyed by the refused head SHA**
  (#950); after a bounded number of consecutive refusals at an unchanged head
  (default 3) the PR is labeled `goobers:merge-demoted` (+
  `needs-remediation`, so remediation has a path to move its head) and stops
  being crowned. The demotion self-heals the moment the head advances.
- **PRL-047 (SHOULD, Shipped):** An **advisory mode** toggle SHOULD let an
  instance evaluate the full conjunct set without attempting a merge;
  advisory refusals never accrue toward demotion. Advisory is the recommended
  default for mixed-company repos (V1, PRL-Q1).

### Remediation loop

- **PRL-050 (MUST, Shipped):** `pr-remediation` MUST select by descending
  cause priority — labeled `needs-remediation`, then failing CI, then merely
  behind-base — in the shared claim namespace (PRL-002). A clean, behind-base,
  finding-free candidate SHOULD be completed through the provider's
  update-branch API without provisioning a worktree (#720); everything else
  enters full remediation.
- **PRL-051 (MUST, Shipped):** The entry stage MUST check out the **PR's own
  branch**, rebind the run's workspace branch to it for every later stage
  (#392 — so the reviewer's computed diff is the PR's real diff), and emit the
  versioned **remediation-brief** artifact (verdict + PR thread + base state);
  structured evidence travels as journal artifacts, only routing scalars via
  `inputsFrom`. Additional brief gatherers (CI failures, review threads,
  issue context) and the declared `remediate`/`minSeverity`/`maxCycles`
  policy inputs are **prescriptive** — owned by
  `pr-remediation-capability.md` D1/D2 (V1); the shipped scope hardcodes
  `conflict ∨ substantive ∨ failing-CI`.
- **PRL-052 (MUST, Shipped):** Routing MUST be **finding-driven, never
  rebase-driven** (D3): a clean rebase with no substantive finding and green
  CI force-pushes and clears the label (done); failing CI pushes the clean
  rebase to retrigger CI and still passes loop control; a conflict or any
  substantive finding routes to the agentic chain — a clean rebase never
  suppresses known substantive work, and a conflict is itself substantive.
- **PRL-053 (MUST, Shipped):** Every push to an existing PR branch MUST be
  `--force-with-lease` with an expectation captured **before** this cycle's
  work (checkout-time tip, or the checkpoint-recorded head) — never
  re-resolved at push time, which makes the lease tautological. On lease
  failure the loop loses gracefully and re-selects next tick.
- **PRL-054 (MUST, Shipped):** Loop control MUST run **before** the agentic
  chain each cycle (D4/D5): a durable per-PR cycle counter (liberal default
  10, overridable) and a stall check that escalates on a **byte-identical
  diff at an unchanged base** — a clean rebase that reproduces the same diff
  while advancing the base is progress, not a stall (#832). Escalation
  applies `goobers:merge-escalated`, clears `needs-remediation`, and records
  the reason plus a head/live-base-tip snapshot on the sticky state comment.
- **PRL-055 (MUST, Shipped):** A reviewer `fail` verdict inside remediation
  MUST escalate immediately with the terminal reason (skipping budget/stall
  checks), surfaced through the run's escalated phase — never retried against
  budget (D2 at both altitudes).
- **PRL-056 (MUST, Shipped):** After a successful publish, the loop MUST post
  an auditable **per-finding response** (exactly one `addressed`/`declined`
  disposition with detail per original verdict finding), published only after
  the branch actually moved, reconciling one run-scoped comment across
  retries (PRR D3). The cycle's product is the republished PR plus this
  changelog; remediation itself **never merges**.
- **PRL-057 (MUST, Shipped):** A cross-PR-blocked (parked) PR is **excluded
  from remediation** — parking means wait, not rework; the drain path is the
  sibling landing (PRL-064), not a rewrite of a diff that would be fine once
  the sibling lands.

### Escalation ladder & drainage

- **PRL-060 (MUST, Shipped):** The escalation ladder is:
  `needs-changes` → bounded remediation (PRL-054) → `merge-escalated`;
  reviewer `fail` → `merge-escalated` directly; zero-winner clusters →
  explicit `fail` escalation (PRL-025); repeated merge refusal →
  `merge-demoted` (PRL-046). Every escalation MUST carry a recorded,
  human-readable reason distinguishing *what was attempted* — never a bare
  label.
- **PRL-061 (MUST, Shipped):** Every park state MUST have a **deterministic
  exit** — either self-heal or a defined human surface. No label may be
  "permanent until a human notices" without a recorded snapshot to check.
- **PRL-062 (MUST, Shipped):** `merge-escalated` self-heals when the PR's
  head has moved **or the live base branch tip** has advanced past the
  escalation snapshot (#1052 — the pinned `base.sha` never moves and MUST NOT
  be the comparison); a labeled PR with no snapshot fails closed (blocked
  until a human clears it).
- **PRL-063 (MUST, Shipped):** `blocked-on-sibling` self-heals when every
  recorded predecessor blocker is closed, merged, or demoted; an absent or
  empty blocker record fails **open** (nothing concrete can hold the park).
- **PRL-064 (MUST, Shipped):** Post-merge MUST run the **drainage sweeps**
  (sibling-pr-sequencing S4, #992): unpark siblings whose blockers resolved,
  remove self-healed escalations and demotions, and fan out
  `needs-remediation` — so a file-overlap cluster of N mergeable PRs drains
  one landing per cycle with a single mechanical rebase each, no human action.
- **PRL-065 (MUST, Shipped):** Escalated runs MUST be inspectable via a
  dedicated needs-human surface (`goobers escalations` / `escalations show`):
  structured cause, repass/retry counts, and the per-stage artifact timeline,
  keyed on the run's escalated phase.

### Post-merge

- **PRL-070 (MUST, Shipped):** The **merge event** — not PR-open — closes the
  originating issue(s) (#355): at PR-open the issue only moves to in-review;
  post-merge parses the merged PR's closing-keyword references and marks each
  done with a linking comment, idempotently. Not every PR closes an issue —
  zero references is a normal outcome.
- **PRL-071 (MUST, Shipped):** Post-merge fan-out MUST be **triaged** (#715):
  only siblings that are conflicted after the merge or file-overlap with the
  merged PR are labeled `needs-remediation`, each with a durable structured
  handoff (displacing PR + overlapping paths); a clean disjoint sibling is
  untouched. A failed triage signal labels conservatively rather than
  silently skipping.
- **PRL-072 (MUST, Shipped):** After an actual merge (direct or
  queue-reported), the merged head branch MUST be deleted unless another open
  PR is stacked on it; cleanup requires the `github:branch:delete` grant and
  a cleanup failure is a warning on an already-successful merge, never a
  merge failure. (The grant was designed in #581 but unwired until #1075 —
  now closed; treat any regression as a PRL-072 violation, not a new gap.)
- **PRL-073 (MUST, Shipped):** Post-merge MUST be **idempotent per PR** via
  the reconciliation ledger under its lock — the timeout-recovery sweep and
  the in-run path never double-apply fan-out or close-out.

### Journal & telemetry obligations

- **PRL-080 (MUST, Shipped):** Verdicts are recorded as **gate artifacts in
  the emitting run's journal** (`GT-015`); in-run consumers read them back
  from that journal, never from re-prompted model output.
- **PRL-081 (MUST, Shipped):** All **cross-run** loop state MUST travel as
  durable provider-side state — labels plus machine-readable sticky-comment
  payloads (verdict-json, blocked-on-sibling, remediation-state,
  merge-demotion, post-merge handoff) — since no two runs share a journal.
  Each payload MUST be SHA-snapshotted where a self-heal check reads it, and
  updates MUST reconcile a single sticky comment rather than append per cycle.
- **PRL-082 (MUST, Shipped):** Provider mutations on the merge path (reviews,
  merges, branch deletions, label writes by the merge stages) MUST be
  recorded through the run's mutation recorder so the journal attributes
  every external side effect.
- **PRL-083 (MUST, Shipped):** Refusals, voids, and no-work outcomes are
  **normal, journaled outcomes** (exit 0 with structured result files), so
  telemetry can distinguish "the machine declined for a stated reason" from a
  failure — the loop's health is measured by drain rate, not green rate.

## Capability-vs-policy audit

`Task.policyActions` is the closed, auditable vocabulary for actions that a
command, policy, persona, or verdict can direct a task to perform. Personas
declare their unconditional vocabulary in `Goober.spec.policyActions`; an
invoking task must redeclare every one. Capability-gated persona actions use
`conditionalPolicyActions` and remain disabled unless the task explicitly
declares the action and its capability. The workflow compiler rejects unknown
actions, omitted built-in/persona declarations, and actions whose task or
goober lacks the canonical grant.

The audit below covers all six shipped self-host workflows. Read-only stages
(`gather-signals`, PR/context selection, CI polling, validation, local tests)
and the `analyst` and `reviewer` personas prescribe no external mutation and
therefore have no action row.

| Action | Workflow / task | Required capability | Status |
|---|---|---|---|
| Claim eligible backlog items and reconcile claim labels (`claim-backlog-items`) | `backlog-curation/query-backlog`, `implementation/query-backlog` | `github:issues:write` | Covered |
| Release the provider-visible claim (`release-backlog-claim`) | `backlog-curation/release-claim` | `github:issues:write` | Covered |
| Close a duplicate, obsolete, or configured-stale issue (`close-issue`) | `backlog-curation/curate` | `github:issues:write` | Covered |
| Post explanatory or evidence comments (`comment-on-issue`) | `backlog-curation/curate`, `work-nomination/nominate` | `github:issues:write` | Covered |
| Create a split child or nominated issue (`create-issue`) | `backlog-curation/curate`, `work-nomination/nominate` | `github:issues:write` | Covered |
| Edit a split parent into a tracking issue (`edit-issue`) | `backlog-curation/curate` | `github:issues:write` | Covered |
| Apply curation or nomination labels (`label-issue`) | `backlog-curation/curate`, `work-nomination/nominate` | `github:issues:write` | Covered |
| Self-approve a nominated issue (`approve-issue`) | `work-nomination/nominate` (conditional persona action) | `github:issues:approve` | Capability-gated; disabled in the shipped task |
| Modify and commit a worktree (`modify-repository`) | `implementation/implement`, `pr-remediation/implement`, `tutor/draft-change` | `repo:push` | Covered |
| Push a run branch (`push-repository-branch`) | `implementation/push-branch`, `tutor/push-branch` | `repo:push` | Covered |
| Open or update a PR (`open-or-update-pr`) | `implementation/open-pr`, `tutor/open-pr` | `github:pr:write` | Covered |
| Comment on and update the driving issue's status (`update-issue`) | `implementation/close-out`, `park-escalated`, `park-needs-human` | `github:issues:write` | Covered |
| Publish the verdict as a native review (`publish-review`) | `merge-review/apply-verdict` | `github:pr:review` | Covered |
| Route a verdict to merge-ready, remediation, sibling-blocked, or escalation (`route-verdict`) | `merge-review/apply-verdict` | `github:pr:write` | Covered |
| Close a moot, duplicate, or byte-identical superseded PR (`close-pr`) | `merge-review/apply-verdict` | `github:pr:write` | Covered |
| Merge a PR after all safety conjuncts hold (`merge-pr`) | `merge-review/merge-pr` | `github:pr:merge` | Covered |
| Watch an enqueued merge to a determined outcome (`watch-merge-queue`) | `merge-review/queue-watch` | `github:pr:merge` | Covered |
| Route an evicted or timed-out queue entry to remediation (`route-queue-outcome`) | `merge-review/queue-watch` | `github:issues:write` | Covered |
| Delete a merged head branch (`delete-branch`) | `merge-review/reconcile-post-merge`, `merge-pr`, `queue-watch` | `github:branch:delete` | Covered |
| Close originating issues after merge (`close-issues`) | `merge-review/reconcile-post-merge`, `post-merge` | `github:issues:write` | Covered |
| Fan out remediation to affected siblings (`fan-out-remediation`) | `merge-review/reconcile-post-merge`, `post-merge` | `github:pr:write` | Covered |
| Record a merge refusal at the current head (`record-merge-refusal`) | `merge-review/record-merge-refusal` | `github:pr:write` | Covered |
| Demote a repeatedly refused lander (`demote-pr`) | `merge-review/record-merge-refusal` | `github:pr:write` | Covered |
| Update a clean behind-base PR through the provider API (`update-pr-branch`) | `pr-remediation/update-behind-pr` | `github:pr:write` | Covered |
| Clear a completed remediation handoff (`clear-remediation`) | `pr-remediation/update-behind-pr`, `rebase-pr`, `push-remediated` | `github:issues:write` | Covered |
| Rebase a PR branch (`rebase-pr`) | `pr-remediation/rebase-pr` | `repo:push` | Covered |
| Rework a PR from reviewer findings (`rework-pr`) | `pr-remediation/implement` | `repo:push` | Covered |
| Record remediation progress or cycle exhaustion (`record-remediation-checkpoint`) | `pr-remediation/remediation-checkpoint`, `park-escalated` | `github:pr:write` | Covered |
| Publish a remediated PR branch (`push-pr-branch`) | `pr-remediation/push-remediated` | `repo:push` | Covered |
| Respond to every original finding (`respond-to-findings`) | `pr-remediation/respond-to-findings` | `github:issues:write` | Covered |
| Escalate a non-converging or rejected remediation (`escalate-pr`) | `pr-remediation/remediation-checkpoint`, `park-escalated` | `github:pr:write` | Covered |
| Retarget a PR | Not present in the shipped policy/persona/verdict vocabulary | — | Not prescribed |

## Known gaps (prescriptive)

Verified open issues this spec expects to be closed against these IDs:

- **#952** — the crowned lander gets no *added* election-stage priority, so a
  freshly-crowned cluster can wait a full cycle to start draining. (pr-select's
  blocked-dependents ordering, PRL-004, already prioritizes landers at
  selection time; the residual is the elect-lander-side latency.) → PRL-004/020.
- **#1061** — `apply-verdict` fails with `selectedHeadSha is required` on the
  elect-lander `elected:false` branch under some threading orders — a
  violation of PRL-030's "every non-void verdict is applied".
- **#1071** — a repo's **native merge queue** can land a sibling-overlap PR
  without the election completing, bypassing PRL-021's single-lander
  arbitration; the queue is a second merge authority the seam (PRL-043) does
  not yet arbitrate with.
- **#509** — configurable selection priority (PRL-005).

## Open questions

- **PRL-Q1:** Mixed-company mode — reviewing/merging human- and other-agent-
  authored PRs (relaxing G1): author classification, trust policy, advisory
  default, human-active backoff. Deferred to the mixed-mode epic (#804 line);
  the `goobers/` branch-namespace restriction is load-bearing until then.
- **PRL-Q2:** Event-driven triggering (review-comment / synchronize →
  reconcile) vs the shipped cron polling — cadence is currently the loop's
  floor on end-to-end latency.
- **PRL-Q3:** Native-queue arbitration (#1071): should enqueue be withheld
  for cluster members until election resolves, or should election consume the
  queue's own ordering as its policy input?
- **PRL-Q4:** Declared remediation policy (PRR D2: `remediate` cause list,
  `minSeverity`, per-cause `maxCycles` per #953) — DSL shape agreed in the
  design doc, unimplemented; until then remediation scope is compiled-in.
- **PRL-Q5:** Escalation-reason taxonomy: PRL-060 requires *a* reason;
  a closed vocabulary ("could not converge" / "out of budget" / "not in
  declared policy" / "approach rejected") would make the escalated bucket
  triageable mechanically (PRR D4).
