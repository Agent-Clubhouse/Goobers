# Design: V0.6 ladder remediation — executor convergence + lifecycle unblock

> Status: **Draft for review** · Area: `RUN` / `PRL` · Milestone: **V0.7 — ladder
> frontload (must-fix before next ladder)** + **V0.8 (fast-follow)**
> Origin: the **V0.6 reliability eval ladder (round 2)** — epic #394, report at
> `~/source/Goobers-Review/ladder/{scorecard,observations}.md`. First round run
> through the **full V0.5 lifecycle** via `goobers up` (implementation →
> merge-review → pr-remediation, human keeps the merge).
> References: [`docs/stage-contract.md`](../stage-contract.md),
> [`docs/design/v0/pr-lifecycle-loop.md`](v0/pr-lifecycle-loop.md),
> `internal/runner/run.go`, `internal/harness/context.go`, `internal/gate`.

## 1. What the ladder proved

Round 2 climbed from a 6-line flag to a ~350-line cross-subsystem provider feature
(#400) across 10 rungs. Headline: **single-PR reliability is not bounded by
complexity/tier — it is bounded entirely by whether the *first* review comes back
clean.** 7/10 rungs whose first review passed → PR-opened, 0 repasses, tight scope,
*including* the two largest Tier-4 items. All 3 non-PR outcomes were the loop
terminating *correctly* (2 non-convergence escalates, 1 over-scope refusal) — never
a wrong-escalate, scope-creep, or forced bad diff.

So the executor's first-pass quality is excellent and scale-insensitive. **The
weaknesses are all in the machinery around it**: the repass can't converge, the
lifecycle can't write its verdict back, the escalation path is wasteful/mislabeled,
and completed items can become falsely re-eligible. This doc designs the fixes,
split into a **frontload set that must land before the next ladder** (§3) and a
**fast-follow set** (§4).

## 2. Findings map (all verified against `bc08a97`)

| ID | Finding | Fix altitude | Anchor | Bucket |
|---|---|---|---|---|
| **L3** | Repass `implement` never receives the reviewer verdict → byte-identical diff → escalate | runner context-assembly | `internal/runner/run.go:479-480`, `internal/harness/context.go:137` | **Frontload** |
| **L1** | `merge-review` `apply-verdict` fails 100% — numeric `selectedNumber` is dropped during downstream input materialization | command result emission + workflow integration | `cmd/goobers/prsiblingcontext.go`, `internal/executor/env.go` | **Frontload** |
| **L7** | `goobers:claimed` never removed on completion + no open-PR eligibility backstop → false re-eligibility | provider + query | `providers/github.go:1001-1008`, `cmd/goobers/backlogquery.go` | **Frontload** |
| **L6** | Implement `failure` has no non-retryable/escalate disposition — re-enters repass loop, terminates `aborted` not `escalated` | runner task-outcome | `internal/runner/run.go:531-537` | **Frontload** |
| **L2** | PR bodies are static boilerplate despite rich material existing in-run | open-pr stage | `cmd/goobers/openpr.go:76-82` | Fast-follow (V0.8) |
| **L4** | Agent transcripts + token/credit captured but buried; assembled-context not a first-class artifact | telemetry/trace | `internal/harness/context.go`, `goobers trace` | Fast-follow (V0.8) |
| **L5** | PR-monitor not author-agnostic/state-driven; human review comments drive no action | pr-lifecycle policy | — | **Pre-covered** by #369 + #368 (augment, don't dup) |

## 3. Frontload design (V0.7 — must land before the next ladder)

### 3.1 L3 — thread the reviewer verdict into the repass `implement` context

**The single highest-leverage fix the ladder surfaced.** It converts the "first
review is a cliff" behavior into "first *or second* review can pass."

**Root cause (verified).** The runner's context for an agentic stage is built from
the walk's accumulated `pointers`, which only ever gains a *task's* own produced
artifacts (`run.go:734`, `contextPointersFor`). The **gate** path
(`state = gr.Target; continue`, `run.go:479-480`) appends **nothing** — `gate.Result`
(`internal/gate/evaluate.go:22-47`) has no `Pointers` field. The reviewer verdict is
persisted only as a journal artifact `verdict/<gate>-<attempt>.json` (`recordVerdict`,
`internal/gate/journal.go:52`), never surfaced as a `contextPointer`. So when the
review gate routes `needs-changes → implement`, the repass `implement` invocation is
rebuilt from `pointers` containing only the *original* query-backlog artifacts — the
issue, not the feedback. The stage `goal`
(`selfhost/gaggles/goobers/workflows/implementation.yaml:82-87`) and the implementer
instructions (`.../implementer/instructions.md:50-54`) **tell the agent to "read the
reviewer rationale … attached as context"** — feedback that is never attached. The
agent re-reads the issue, infers "repass" from git, and re-affirms the same diff.
Transcript-confirmed on both #398 and #399.

**This is not #316/#385.** That work added the identical-diff *guard* (escalate when a
repass reproduces the prior diff) — the guard is working correctly. L3 is the reason
the guard keeps firing: the implementer has nothing new to act on.

**Design.** When a gate routes back to a prior `implement` stage (a repass), the
runner must inject the gate's most-recent `Verdict` artifact as a `contextPointer` on
the repass invocation, using the **same mechanism** `materializeContext`
(`internal/harness/context.go:137`) already uses to write `.goobers/context/NN-<name>`
files. Concretely:

- Give the gate walk a way to contribute a pointer to the verdict it just recorded
  (either `gate.Result` carries the verdict `ArtifactPointer`, or the runner
  re-resolves `verdict/<gate>-<attempt>.json` by convention at repass-dispatch time).
- On a repass dispatch (target is a stage the walk has already executed), append that
  verdict pointer to the stage's `contextPointers` before invocation, named
  distinctly (e.g. `NN-review-verdict`) so the agent — and any future automated
  context-manifest (L4) — sees it.
- **Contract-faithful:** this is entirely within the existing `contextPointers`
  mechanism (read-only journal pointer). No envelope reach-through, no schema change.
  It makes real the assumption `pr-lifecycle-loop.md` §4 already states ("the same
  evidence-pointer mechanism the in-run reviewer already uses to feed `implement`").

**Expected scorecard move:** both #398 and #399 had small, local, single-finding
`needs-changes` (DOT node-ID collision; ADO URL-escaping). With the verdict in
context they plausibly become clean 1-repass PRs — converting 2 escalates → 2 PRs and
lifting the effective ceiling from "first review must be perfect" to "first or second."

**Doc:** clarified in `docs/stage-contract.md` ("What the runner does on each status"
— repass context obligation).

### 3.2 L1 — thread `selectedNumber` through `merge-review`

**Small fix; unblocks the entire V0.5 lifecycle.** `merge-review`'s decider reaches a
correct `decision:pass` but `apply-verdict` aborts with `selectedNumber is required`.
Prerequisite for every label-gated path (L7, L5, pr-remediation).

**Root cause (corrected 2026-07-15, per Dev-3's trace on #413 — this doc's first draft
mis-ranked it).** The **load-bearing fix is stringifying `selectedNumber` end-to-end**:

- [`task.expectedOutputs` is declared-not-enforced at V0](versioning-and-compatibility.md#compatibility-registry):
  no runner path consumes it for the result-file→`Outputs` merge, input threading, or
  postcondition validation. Enforcing it would be a future contract change, not an L1
  fix.
- `internal/executor`'s `InputResultFile` convention only threads **string-valued**
  top-level result-file keys into a downstream stage's `GOOBERS_INPUT_*` env var — a
  numeric value survives into the run's `Outputs` map but is **silently dropped** at
  that step. `gather-sibling-context` emits `selectedNumber` as a native int
  (`403`), so it never reaches `apply-verdict.inputsFrom`
  (`selfhost/gaggles/goobers/workflows/merge-review.yaml:74-82`). The **sibling** stage
  `gather-pr-context` already stringifies it *for this exact reason*
  (`cmd/goobers/gatherprcontext.go:164-171`); `gather-sibling-context`'s failure to do
  so is the asymmetry that is the bug.

**Fix (two parts):** (1) emit `selectedNumber` as a **string** in
`gather-sibling-context` (matching `pr-select`'s `strconv.Itoa` + `apply-verdict`'s
`Atoi`) — the runtime fix; (2) add the poll→select→gather→review→apply **integration
test** asserting a label is *actually applied*. **Test debt this exposed:** V0.5 had no
such end-to-end test — unit tests that stub stage IO passed while the wired workflow
was 100% broken. General gotcha: **any** non-string value threaded through
`Task.InputsFrom` hits this same silent drop.

### 3.3 L7 — one eligibility lifecycle the selection query trusts

**Two out-of-sync sources of truth.** The local claim ledger *releases* on completion
(`cmd/goobers/issuecloseout.go:152`), but the GitHub `goobers:claimed` label
(`providers/model.go:18`) is **removed nowhere** — `UpdateWorkItemStatus`
(`providers/github.go:1001-1008`) only swaps `goobers/status:`-prefixed labels, so
`goobers:claimed` survives indefinitely. Meanwhile #279/#280 lost their label in a
prior session and today's run re-claimed them in the ledger without re-applying the
label → both are now falsely eligible. Eligibility currently excludes in-review items
**only** via the configurable `excludeLabels: [goobers/status:in-review]`
(`implementation.yaml:61`), applied at PR-open — no backstop if that write was missed.

**Root problem:** Mode A (no merge) has **no terminal issue state** — close-out marks
`in-review`, the real close is `post-merge` which never fires, so a completed rung's
issue stays `open` forever, gated only on labels that aren't reconciled against "a PR
already exists."

**Design (converge to one lifecycle):**
1. **Release the `goobers:claimed` label on the same event the ledger releases the
   claim** (close-out) — pair the ledger `Release` with a provider label removal so the
   two sources never diverge.
2. **Add an open-PR eligibility backstop:** the selection query excludes any issue that
   already has an open goober PR (linked via `Fixes #N` / branch), not only issues
   carrying the `in-review` label — so a missed label write can't cause a duplicate PR.
3. Make the **durable ledger authoritative** (or reconcile the label from it) so
   eligibility never depends on a single mutable label being perfectly maintained.

This is the natural companion to `pr-lifecycle-loop.md` §8 (close-out on the merge
event): until merge closes the issue, the issue must be reliably *ineligible* while its
PR is open.

### 3.4 L6 — a first-class non-retryable / escalate disposition

**The correct answer terminated as `aborted` after 3 wasted cycles.** On #402 the
implementer emitted `status:"failure", error.code:"ISSUE_OVER_SCOPE"` on **attempt 1**,
but `taskOutcome` (`internal/runner/run.go:531-537`) routes any `failure` whose `Next`
is a gate straight *into* the review gate, which branches `needs-changes → implement` —
back into the repass loop. `error.code`/`retryable` is **recorded**
(`errorDetailFrom`, `run.go:898-903`) but **never routed on**. The only path to
`@escalate` is repass-budget exhaustion, so the identical correct conclusion was
re-derived 3× (~11 min, ~150 credits) and the run terminated `aborted` (via review
`fail → @abort`), **not** `escalated` — inverted vs. the non-convergence rungs
(#398/#399), which *did* reach `escalated`.

**Design.** Honor a **non-retryable business disposition** the `implement` stage can
emit and the runner routes **straight to `@escalate` (terminal `escalated`) after one
attempt**, bypassing the review gate and the repass loop:

- The contract already carries `error.retryable` (`ResultEnvelope.error`,
  `stage-contract.md`). Define: an agentic stage returning `status:"failure"` with
  `retryable:false` **and** a recognized escalate code (e.g. `ISSUE_OVER_SCOPE` /
  `NEEDS_DECOMPOSITION`) routes to `@escalate`, not to `Next`.
- Terminal state is `escalated` (the signal a human / a future decomposition workflow
  selects on), never `aborted`.
- **Sibling need:** the reviewer gate should also fast-`fail` an explicit
  empty-diff-on-an-over-scope-probe rather than issuing two `needs-changes` first.
- **Distinct from #263** (a crash-durability gap in repass-budget accounting) — adjacent
  machinery, different concern.
- Doc: `docs/stage-contract.md` "What the runner does on each status" gains the
  non-retryable-failure → `@escalate` row.

## 4. Fast-follow design (V0.8 — near-term, non-blocking)

### 4.1 L2 — rich structured PR bodies (from existing run artifacts)
Every loop PR gets an identical 3-line boilerplate body (`cmd/goobers/openpr.go:76-82`;
a `body` provider-input override exists but is never set). All the material for a rich
body **already exists in the run** and is not threaded in: reviewer verdict
rationale/summary (`verdict/review-0.json`), the diff (files + ±lines), local-ci output
(`local-ci/stdout.log`), the issue AC, SHAs, digest. Design: the `open-pr` stage reads
those same journal artifacts (the mechanism `apply-verdict` already uses) and renders a
structured body — **Summary · Changes · Testing · Reviewer verdict · footer** — with a
repass PR also showing the review→repass history. Formatting/threading change, no new
capture.

### 4.2 L4 — make the captured signal first-class
Transcripts (with token/credit footers) and inter-stage context are captured but
buried. Three independent slices:
1. `goobers trace --transcripts` (+ a per-stage transcript view) — surface the
   `spans/sha256/<digest>` blobs that today need hand-mapping.
2. Structured `agent.usage{stage, credits, tokensIn/Out, cached}` telemetry events —
   move token/credit out of free-text footers so per-stage/run cost is queryable.
3. **Record the runner-assembled context manifest as a stage artifact** — the blind
   spot that hid L3; with it, L3-class bugs are self-evident without transcript
   archaeology. (Do this *after* L3 so the manifest includes the new verdict pointer.)

### 4.3 L5 — augment existing mixed-mode design, don't duplicate
The ladder confirmed the need for an **author-agnostic, state-driven PR-monitor** that
reconciles on PR facts (unresolved review threads from *any* author, behind-base, CI,
staleness) and feeds the human/agent review thread as first-class remediation input.
This is **substantially already designed**: `pr-lifecycle-loop.md` §9 (mixed-company
mode, V1) + issues **#369** (author-agnostic selection, human-active backoff, opt-out)
and **#368** (event-driven trigger on review-comment/synchronize). **Action:** augment
#369/#368 with the ladder's concrete confirmation and the "reconcile on PR state incl.
human comments" framing; do not file a duplicate. Note the hard dependencies: L1
(label writeback) and L3 (the rework chain hits the same non-convergence) are
prerequisites for any of this to function.

## 5. Sequencing & the next ladder

**Gate for the next ladder run: V0.7 (L3, L1, L7, L6) merged.** Rationale:
- L3 — otherwise the ladder can't measure past "first review clean."
- L1 — otherwise the full lifecycle (which the next ladder runs in-app) is dead.
- L7 — otherwise a re-run re-claims completed items and duplicates PRs.
- L6 — otherwise hard/over-scope rungs pollute the run with `aborted` + wasted cycles.

**Then:** re-run a ladder — ideally **driven by Goobers itself in-app** (its own
curation → implementation → merge-review lifecycle preparing the *next* backlog),
which is the dogfooding milestone that closes **V0**. Two things that ladder should add,
designed but deferred here:
- a **non-telegraphed over-scope probe** (the #402 probe announced itself; a stronger
  test is genuinely-too-big *without* saying so — measures unprompted un-scopeability
  detection), and
- a real **decomposition workflow** to consume `ISSUE_OVER_SCOPE`/`needs-decomposition`
  escalations (epic #318's "deciding what / decomposing is a separate workflow" still
  has no implementation) — the downstream that L6's `escalated` signal feeds.

## 6. Dispatchable issues

**V0.7 (frontload — approved + ready now):**
1. **[epic]** V0.7 — ladder frontload.
2. **L3** — thread reviewer verdict into repass `implement` context (§3.1).
3. **L1** — emit `selectedNumber` as a string + poll→apply integration test (§3.2).
4. **L7** — release `goobers:claimed` on close-out + open-PR eligibility backstop +
   ledger-authoritative eligibility (§3.3).
5. **L6** — non-retryable `@escalate` disposition (`retryable:false` + escalate code) +
   `escalated` terminal state (§3.4).

**V0.8 (fast-follow):** L2 (rich PR bodies) · L4 (trace transcripts / structured usage /
context manifest) · non-telegraphed probe · decomposition workflow.

**Augment (no new issue):** L5 → comment on #369 + #368.
