# Design: Autonomous sibling-PR sequencing — draining file-overlap clusters without a human

> Status: **Draft for review** · Area: `PRL` / `RUN` (merge-review + pr-remediation)
> Origin: `weekend_12` deep-dive (`~/source/Goobers-Review/weekend_12/findings/010`,
> `findings/011`) — every human-escalated PR that round was a PR the daemon
> *created but could not drain itself*, and the single biggest bucket was
> **file-overlap collisions between sibling PRs**.
> References: `cmd/goobers/prsiblingcontext.go`, `cmd/goobers/applyverdict.go`,
> `cmd/goobers/electlander.go`, `cmd/goobers/postmerge.go`,
> `cmd/goobers/remediationcheckpoint.go`, `cmd/goobers/blockedonsibling.go`,
> `cmd/goobers/rebasepr.go`, `selfhost/gaggles/goobers/workflows/merge-review.yaml`,
> `selfhost/gaggles/goobers/goobers/reviewer/instructions.md`,
> `api/v1alpha1/envelope.go`. Related issues: #837, #836, #952, #950, #843,
> #716, #715, #747, #941, #980, #986.

## 1. The problem, and why it is the highest-leverage gap

The autonomous loop can **create** PRs faster than it can **drain** them. It has
exactly two autonomous drainage paths — `merge-pr:success` (the repairable
subset, proven n=3 in `weekend_12`) and the narrow moot-close — and everything
else escalates to a human. `weekend_12`'s escalation bucket was drained entirely
by a manual operator cleanup wave at 21:08–21:19 PDT (4 hand-merged, 2
hand-closed). That manual wave is the hidden human dependency standing between
"impressive with a babysitter" and "sustained autonomous processing."

The **largest, most common** cause in that bucket was **sibling file-overlap**:
two or more independently-implemented PRs touch the same files, each is correct
and individually mergeable, and neither can land without the others rebasing
onto it. This round: `#955`/`#956`/`#957` (all touch `cmd/goobers/prselect.go`
and a shared provider-deadline helper) and `#972`/`#959` (17 shared portal
files). The daemon *detected* every one precisely — it named the files, the
sibling PRs, sometimes the exact shared helper — and even drained **one** of
them autonomously (`#959`, the sibling of `#972`). But the rest escalated, and
along the way pr-remediation burned repeated **byte-identical** repair cycles
trying to reconcile two overlapping PRs at once (`#972`, 3 cycles, no progress).

So the goal of this design is narrow and concrete: **make the daemon drain a
file-overlap cluster the way a careful human does — pick an order, merge one,
rebase the rest onto it, repeat — instead of trying to reconcile the whole
cluster in one agentic pass and escalating when it can't.**

## 2. What already exists (verified against `origin/main`)

Most of the machinery is already present. The gap is not "build a sequencer from
scratch" — it is "connect three seams that today route past each other."

| # | Component | What it does today | Anchor |
|---|---|---|---|
| 1 | `gather-sibling-context` | Lists every other open goober PR on the same base with each one's **files**, SHAs, labels, check-state. Does **not** compute overlap — hands raw file lists to the reviewer. | `prsiblingcontext.go:47,110,167` |
| 2 | cross-pr-blocked class | **Exists.** `FindingCrossPRBlocked` + required `BlockingPRs`. `verdictLabel`/`allCrossPRBlocked`: a needs-changes verdict whose findings are *entirely* cross-pr-blocked → `goobers:blocked-on-sibling`; any substantive finding mixed in → `goobers:needs-remediation`. | `envelope.go:220,304`; `applyverdict.go:42,61` |
| 3 | `elect-lander` | Policy registry (`fifo`=lowest PR#, `newest`=highest). Fires only when `allCrossPRBlocked`. Elected → `merge-pr`; not-elected → `apply-verdict`. Each member recomputes the winner independently. | `electlander.go:29,66,90` |
| 4 | park + self-heal | `blocked-on-sibling` unparks when **named blockers** all close (`blockedOnSiblingStillBlocks`). `merge-escalated` self-heals when the PR's head or the **live base tip** advances past the escalation snapshot (#1052), and post-merge sweeps now **remove self-healed escalations programmatically** (`unparkSelfHealedEscalations`, #992/#836 — this row's original "never removed" gap is closed). | `blockedonsibling.go:47`; `remediationcheckpoint.go:177`; `postmerge.go` |
| 5 | post-merge drain | `unparkResolvedSiblings` removes `blocked-on-sibling` when all named blockers resolved; `fanOutNeedsRemediation` labels conflicted/overlapping siblings `needs-remediation`. | `postmerge.go:122,152,204` |
| 6 | `rebase-pr` | Behind-base PR: clean rebase → force-push + clear label; conflict/substantive → hand to agentic implement. The catch-up path a loser would take. | `rebasepr.go:32,102,128` |

## 3. Root cause — three seams that route past each other

**R1 — File overlap is classified as `substantive`, so it never reaches
election.** The reviewer instructions (`reviewer/instructions.md:70-88`) tell the
model to file a **`substantive`** finding for file overlap ("naming the specific
sibling PR") and reserve **`cross-pr-blocked`** for an *asymmetric logical
dependency* ("the selected PR extends something a sibling is introducing"). So a
symmetric file-collision cluster routes to `needs-remediation` → pr-remediation
tries to **reconcile** it (the byte-identical wasted cycles) → escalates to
`merge-escalated`. The election/sequencing path (§2 #3–#5) is wired to a finding
class the reviewer almost never emits for the collision that actually dominates.
*(This refutes #837's premise that the classification doesn't exist — it exists,
but the trigger is the wrong one.)*

**R2 — There is no materialized landing order, and symmetric blockers deadlock
the drain.** Nothing computes or persists a cluster-wide order (grep: `landingOrder`
appears nowhere in the PR path). "Who goes first" exists only as each PR's
independent recomputation of the election winner, and each loser's recorded
`Blockers` is the **symmetric union** of overlapping siblings. So in a 3-cluster,
after the lander merges, loser A still lists loser B as a blocker and B lists A —
`unparkResolvedSiblings` never fires for either. The cluster stalls at N−1.

**R3 — The wrong park label, with no exit.** Because R1 sends collisions to
`needs-remediation`→`merge-escalated`, losers land in the one park bucket that
(a) keys self-heal on the PR's *own* SHA — which a sibling merging does **not**
change — and (b) is **never removed by any code path**. That is the concrete
reason `#972` never followed its merged sibling `#959`: it was parked
`merge-escalated`, whose snapshot only clears if `#972`'s own branch is touched,
and nothing touched it.

## 4. Design — deterministic sequencing as the default drain

Principle: **a cluster of individually-mergeable PRs that differ only by
file-overlap is a *sequencing* problem, not a *reconciliation* problem.** Don't
ask an agent to merge N overlapping diffs into one; pick an order, land the head,
and let each successor rebase onto the now-updated base — a plain, single,
mechanical rebase per PR, escalating only if *that* rebase hits a genuine
semantic conflict.

This is exactly the operator's own instinct: *"if there is no other finding or
issue, merge-review should apply [the order] generally; when it looks at one
that has no other findings it should go in order."*

### 4.1 Deterministic overlap detection (foundation)

`gather-sibling-context` already gathers every sibling's file list. Add a
deterministic **overlap computation**: the selected PR's changed files ∩ each
sibling's changed files. Emit the overlap set (and the overlapping sibling PR
numbers) into `sibling-context.json`. This gives every downstream stage a
**ground-truth cluster** that does not depend on the LLM noticing the collision —
addressing R2's "no deterministic backstop" and feeding both the reviewer (as
evidence) and the ordering step.

*No behavior change on its own — pure surfacing. Ships first, de-risks the rest.*

### 4.2 Classify individually-green overlap as a sequencing case (fixes R1)

When the selected PR is **individually mergeable** — green CI, and no substantive
finding *of its own* (no bug, no scope problem, only the sibling overlap) — the
verdict for the overlap is `cross-pr-blocked`, **not** `substantive`. Two-part:

- **Reviewer instructions**: rewrite the file-overlap guidance so a pure,
  symmetric file collision on an otherwise-clean PR is filed `cross-pr-blocked`
  with `BlockingPRs` = the overlapping siblings. Reserve `substantive` for a PR
  with a real defect of its own.
- **Deterministic backstop**: using §4.1's overlap set, if the reviewer returns
  no substantive/conflict/CI finding but the PR overlaps ≥1 sibling, treat it as
  `cross-pr-blocked` even if the model omitted it. The LLM verdict is no longer
  the sole arbiter of whether a collision enters the sequencing path.

This routes collisions into the *existing* election → `blocked-on-sibling` →
unpark machinery (§2 #3–#5) — the path that **can** drain, and whose park label
**does** self-heal — instead of `needs-remediation` → `merge-escalated`.

### 4.3 Materialize a total order; blockers = predecessors only (fixes R2/R3)

Turn the implicit per-PR election into an explicit **total order over the
cluster**, computed deterministically from the election policy (default `fifo` =
ascending PR number). Persist it, and set **each member's `Blockers` to its
predecessors only**, not the symmetric union:

```
cluster {955, 956, 957} under fifo →  order [955, 956, 957]
  955.blockers = {}          → elected lander → merge-pr
  956.blockers = {955}       → blocked-on-sibling
  957.blockers = {955, 956}  → blocked-on-sibling
```

Now the drain is monotone: `955` merges → `unparkResolvedSiblings` clears `956`
(its only blocker closed) → `956` rebases onto new main (clean; `955`'s changes
are now *in* main, not a conflicting sibling), re-enters, becomes the new head,
merges → `957` unparks → … Cluster drains one PR per cycle with a single
mechanical rebase each, no reconciliation, no byte-identical loops.

### 4.4 Bounded, honest escalation (kills the wasted cycles)

A successor only escalates to a human when **its own rebase onto the merged
predecessor hits a real semantic conflict** the agentic `rebase-pr`→implement
path cannot resolve — never for mere ordering. This directly removes the
`#972`-style "3 byte-identical cycles then escalate": there is no reconciliation
attempt to loop on, only a rebase that either applies or surfaces a true
conflict once.

### 4.5 Opt-in, conservative by default

Auto-sequencing is a **declarative disposition policy** on the merge-review
policy seam (the same seam #843/#835/#941 establish), defaulting to the current
behavior. A team opts into `autoSequence: fifo|newest|…`; unset keeps today's
escalate-to-human. Not every repo wants the daemon merging one sibling and
rebasing others unattended — it must be a choice, not an imposition.

## 5. Incremental delivery — each PR independently green-and-mergeable

| Step | Issue | Scope | Ships | Risk |
|---|---|---|---|---|
| **S1** | #989 | Deterministic overlap detection in `gather-sibling-context`; emit overlap set + reviewer evidence. | Surfacing only, no routing change. | Low |
| **S2** | #990 | Classification: individually-green overlap → `cross-pr-blocked`, with the §4.1 deterministic backstop. Reviewer-instruction change + backstop in `apply-verdict`/`elect-lander`. | Collisions enter the election path. | **High** (touches verdict classification + reviewer instructions; blast-radius audit required — see §7). |
| **S3** | #991 | Total-order materialization; predecessors-only `Blockers`; ensure losers park `blocked-on-sibling` (not `merge-escalated`). | Monotone drain; no deadlock. | Med |
| **S4** | #992 | Drain hardening: confirm post-merge unpark → `rebase-pr` catch-up advances the next member; bounded escalation on genuine conflict only; belt-and-suspenders programmatic `merge-escalated` removal when base advances. | No wasted cycles; no stranded losers. | Med |

S1 and S3 are self-contained. S2 is the behavioral core and the one to review
hardest. Land S1 first (foundation, safe), then S2+S3 together (they are only
meaningful as a pair), then S4.

## 6. Non-goals

- **True duplicates / stale-issue PRs** (close-disposition) — different problem,
  tracked separately (#987, #980, #947, #983). Sequencing is for *valid*
  overlapping PRs, not redundant ones.
- **Cross-*issue* dependency ordering** (declared `blocked_by`) — #751 territory;
  this design is about *file* overlap discovered at review time.
- **Preventing the collision at claim time** — a separate, complementary lever
  (#980 is step one). This design drains collisions that already exist.

## 7. Risks & guardrails

- **Blast radius (S2).** Verdict classification flows through the shared gate
  evaluator; a change here can affect every agentic gate. Audit
  `grep -rl "evaluator: agentic"` and the `internal/gate` path before landing,
  and keep the change scoped to the sibling-overlap finding class.
- **Reviewer-instruction change is behavioral and hard to unit-test.** Pair it
  with the deterministic backstop (§4.2) so correctness does not depend solely on
  model wording, and add a fixture verdict test for the backstop.
- **Workflow YAML edits** (`merge-review.yaml`) must sync the `selfhost` and
  `acme-web` copies and update `internal/workflow` tests in the same PR.
- **Wrong-order merges.** `fifo` is deterministic and conservative; the merge
  queue re-tests each land, so a bad order fails a required check rather than
  breaking main. Escalate on a genuine post-rebase conflict.
- **Over-eager auto-close is *not* in scope here** — sequencing only *rebases*
  losers, never closes them; closing is the separate disposition (#987).

## 8. Success criteria

A file-overlap cluster of N individually-mergeable PRs drains to zero **with no
human action**: one merges per cycle, each successor rebases cleanly onto the
prior, and a human is involved only when a successor's rebase hits a real
semantic conflict — measured against a `weekend_12`-style live run where the
`#955`/`#956`/`#957` and `#959`/`#972` clusters would have drained autonomously.
