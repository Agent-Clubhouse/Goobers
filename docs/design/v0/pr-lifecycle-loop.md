# Design: PR lifecycle loop — closing "issue → PR" into "issue → merged change" (V0.5)

> Status: **Draft for review** · Area prefix: `PRL` · Milestone: **V0.5 — closing the loop: PR review → remediation → (auto-)merge**
> Architecture: [`docs/ARCHITECTURE.md`](../../ARCHITECTURE.md) §2 (invariants), §5 (stages/gates)
> Requirements: [`docs/requirements/workflow.md`](../../requirements/workflow.md) · [`docs/requirements/gate.md`](../../requirements/gate.md)
> Builds on: the shipped `implementation` workflow (issue #27) · reframes #353 (integration-review altitude) and #355 (close-out timing)
> Origin: the V0.3 reliability ladder (#318) produced 9 individually-clean PRs that
> were **mutually un-mergeable as a set** — surfacing that Goobers reviews per-run,
> per-diff, and has no layer that reviews across PRs, against a moving base, or that
> carries a PR forward to merge. This design adds that layer.

## 1. Why this exists

Today the core loop is **issue in → PR out**. The `implementation` workflow claims a
ready issue, implements it in a worktree, passes an in-run reviewer gate and a local
`make ci` gate, opens a PR, and stops. It **never merges** — by explicit design, a
human does, behind branch protection.

That leaves the loop open at exactly the point where the V0.3 ladder got stuck. Nine
runs each branched off the *same* `origin/main`, none aware of the others; the
resulting PRs overlapped on files and could not all merge. Each in-run reviewer was
**structurally blind** to this: it sees one diff, for one issue, against the base at
one moment in time. Nothing in Goobers:

- reviews a PR **against the other open PRs** (cross-PR conflict / drift),
- notices the base has **moved** and the PR now needs a rebase,
- reads **human or other-agent review comments** on an already-open PR and acts on them,
- carries a PR **forward to merge** once it is genuinely ready.

V0's north star is a **durable, humming machine that builds itself**. A machine that
opens PRs and then waits forever for a human to integrate them is not that machine.
This design closes the loop to **issue in → merged change out** under V0's operating
assumptions (below), with the robustness knobs deliberately turned toward
**throughput and learning over efficiency**.

### Operating assumptions (V0.5 scope)

- **G1 — Goober-authored repo, human looking in periodically.** V0.5 assumes the PRs
  under management were opened by Goobers (`goobers/*` branches). Reviewing
  human/other-agent PRs in a mixed-company repo is a *design goal of the model* but a
  **V1** capability (§9).
- **G2 — Full-auto merge is in scope.** The machine must be able to merge its own work
  end-to-end without a human in the critical path, while a human can look in, override,
  and pause. This is a deliberate relaxation of "this instance never merges."
- **G3 — Liberal limits; waste is acceptable; throughput/correctness/quality first.**
  Retry counts, repass budgets, and run caps are set **generously**. The machine
  should rather burn cycles and leave a rich log trail than exhaust a budget mid-task
  or sit idle against an arbitrary wall clock. Token/credit efficiency is **not** a
  V0.5 objective.
- **G4 — Solid foundations over completeness.** V0.5 ships the durable contracts
  (verdict schema, PR-state, SHA-pinning) correctly, and the harder robustness
  (oscillation detection, lazy rebase, native review protocol, mixed-mode) as
  clearly-scoped V1/V1.1 follow-ons that build on those contracts without rework.

## 2. The three-workflow model

The loop is three workflows at two altitudes, connected **only by durable state on the
PR** — never by one workflow directly invoking another (that is the child-workflow
model, #155, and is not required here).

| Workflow | Altitude | Role | Status |
|---|---|---|---|
| **`implementation`** | one issue → one diff | **Executor** — produce a scoped, AC-correct, CI-green PR | ships today (#27) |
| **`merge-review`** | the whole open-PR set | **Decider** — holistically judge merge-readiness, emit a verdict, and (optionally) merge | new (this design) |
| **`pr-remediation`** | one existing PR | **Executor** — rebase and rework a flagged PR against the verdict, re-push | new (this design) |

This mirrors the split #318 already draws for `implementation`: a **rock-solid
executor** of well-scoped work, with **deciding** kept separate. `merge-review` is the
decider one level up; `pr-remediation` is a second executor that happens to enter on an
existing PR rather than a fresh issue.

### Why decider and executor stay separate workflows

- **Capability isolation (the clinching reason).** `merge-review` needs read + comment +
  (optionally) a new `github:pr:merge` capability. It must **never** hold `repo:push`.
  `pr-remediation` is the *only* thing that pushes to existing PR branches — exactly as
  `implementation` scopes its `implement` stage to `repo:push` and nothing else. Inlining
  the fix into the decider would hand the merger write access to every branch.
- **Execution model.** Remediation runs in a fresh isolated worktree; a scan-and-comment
  reviewer should not be spinning those up.
- **Reuse.** `pr-remediation` reuses the **identical** `implement → review → local-ci →
  gates` chain. Inlining would duplicate it.
- **Independent cadence + human seam.** State-on-the-PR lets each run at its own cadence,
  lets a human read the decider's verdict and agree/override, and survives crashes.

## 3. Composition: state on the PR, not workflow-to-workflow calls

Every hand-off is a **label + a structured artifact** on the PR. No workflow calls
another. This is the same declarative-selection model `backlog-query` already uses
(claim by `goobers:approved` + `goobers:ready`).

Label contract (V0.5):

- `goobers:merge-ready` — `merge-review` verdict = pass, pinned to a SHA (§6). Eligible
  to merge (or already the elected next-to-merge).
- `goobers:needs-remediation` — `merge-review` verdict = needs-changes, or a post-merge
  rebase is required. Selected by `pr-remediation`.
- `goobers:blocked-on/<n>` — deferred behind another PR (ordering/serialization, §7).
- `goobers:merge-escalated` — a budget or convergence guard tripped (§5); a human must
  look. The machine stops selecting it.

`merge-review` and `pr-remediation` each trigger on **cron and manual** in V0.5
(`schedule` + `goobers run`, both exist). Event-driven triggering (fire on a PR
review-comment / synchronize webhook) is **V1**, gated on #342 (signal trigger) + #169
(daemon API sink) + #343 (run delegation into a live daemon).

## 4. The verdict contract (the handoff everything hangs off)

`merge-review` emits a **structured `Verdict`** for each PR it evaluates, mirroring the
existing in-run gate `Verdict` (`pass` / `needs-changes` / `fail` + rationale) so
`pr-remediation` consumes it with **zero new plumbing** — the same evidence-pointer
mechanism the in-run reviewer already uses to feed `implement`.

The prose PR comment is a **human-readable projection of the same artifact**, not a
separate source of truth — so the comment and the fix cannot drift.

### Finding classes (D1)

Each `needs-changes` verdict enumerates **classed findings** so remediation routes
correctly and the machine is *aware* when a PR only needs a rebase:

- `rebase-needed` — base advanced; a (possibly clean) rebase is required.
- `conflict` — rebase does not apply cleanly; needs resolution.
- `substantive` — a code change is required (cross-PR drift, regression, human comment,
  a real defect).
- `cross-pr-blocked` — correct in isolation but must wait behind another PR (§7).

The verdict is a **checklist**: `pr-remediation` must clear *every* item and
`merge-review` re-verifies (SHA-pinned) that *every* item is cleared before
`merge-ready`. A verdict is never "probably fine because the rebase worked."

### `fail` ≠ `needs-changes` (D2)

Mirror the in-run gate exactly: `pass → (merge-ready)`, `needs-changes → remediation`,
`fail → escalate to human` (a fundamentally wrong approach is not burned on remediation
budget). `fail` sets `goobers:merge-escalated`.

## 5. `pr-remediation` — rebase-first, finding-driven (D3)

The routing decision is **finding-driven, never rebase-driven.** A clean rebase never
suppresses known substantive work. Rebase cleanliness and "needs the agent" are
**orthogonal axes**:

| rebase result | substantive findings? | action |
|---|---|---|
| clean | none | re-push rebased branch, clear label → back to `merge-review` |
| clean | **yes** | rebase, **then** agent stage addresses findings, then gates |
| conflict | none | agent resolves the conflict (that *is* substantive), then gates |
| conflict | yes | agent resolves conflict **and** addresses findings, then gates |

Flow:

```
pr-remediation:  select(label = needs-remediation)
  → gather-pr-context (deterministic): checkout branch; load the Verdict artifact,
      PR-thread comments, and behind/conflict state as context pointers
  → rebase (deterministic, force-with-lease)
     → [clean AND no substantive findings] → re-push, clear label, done
     → [conflict OR substantive findings]  → implement (agentic; reads Verdict + thread)
        → review → local-ci → gates → re-push → clear label
```

`gather-pr-context` is the **one genuinely new executor entrypoint**: it replaces
`implementation`'s `query-backlog` head. Everything downstream (`implement`, `review`,
`local-ci`, the gates) is the **same shared goober + gate wiring** — reused by
referencing the same `implementer`/`reviewer` **goobers** today, and by literal
stage-reference once #155 lands (§10).

`force-with-lease` is mandatory: even in a goober-authored repo a human may push to a
branch; the lease makes Goobers lose gracefully and re-select next tick rather than
clobber the push.

## 6. Loop control — the correctness backbone

Four things must be **durable state on the PR** (label/sticky-comment/journal), because
the loop now spans many runs across two workflows. V0.5 ships all four, tuned
**liberally** per G3.

### D4 — Per-PR repass budget → escalate

Lift the in-run `DefaultMaxRepasses=3 → @escalate` to PR altitude: a durable per-PR
cycle counter. On exhaustion → `goobers:merge-escalated`, stop selecting, a human looks.
**V0.5 default is liberal** (e.g. 10 cycles, config-overridable) — per G3 we would
rather over-spend and leave a trail than starve a nearly-done PR.

### D5 — No-progress / same-diff escalation (minimal in V0.5)

If `pr-remediation` pushes a **byte-identical diff** to its previous push, it is stuck
(#316 at PR altitude) → escalate. V0.5 ships this cheap check. **Richer oscillation
detection** — hashing each verdict's finding-set and escalating on a revisited
state (A→B→A) — is **V1** (§9); it is a robustness upgrade, not a foundation, and the
liberal budget (D4) is the V0.5 backstop until it lands.

### D6 — SHA-pinned verdicts

Every verdict is pinned to `(headSHA, baseSHA)` stored with the artifact. Before acting
on `merge-ready`, re-check current head/base against the pin; on mismatch the verdict is
**void** → re-review. This is what prevents merging something reviewed against an old
base. V0.5 implements the pin as verdict-artifact metadata + a re-check; the **native
GitHub Review protocol** (which gets stale-dismissal for free) is a V1 upgrade (§9).

### D7 — Serialize merges; avoid the rebase thundering-herd

When `merge-review` merges a PR, every other open PR that is now behind goes stale —
this is the *intended* trigger for remediation, not a fault. But rebasing **all** behind
PRs after **every** merge is O(N²) churn. V0.5 keeps it simple and correct, deferring the
optimal form to V1:

- **V0.5:** `merge-review` merges **at most one PR per tick** and, on merge, labels the
  now-behind PRs `needs-remediation`. Because only one merges per tick, the herd is
  paced, not eliminated — acceptable under G3 (we tolerate the extra rebases).
- **V1:** **winner-election + lazy rebase** — elect one next-to-merge (reusing #350's
  priority/FIFO ordering), rebase and re-verify **only that PR**, merge it, then elect
  the next. Bounds rebases to N instead of N², and makes ordering deterministic. Also
  resolves circular mutual-conflict flagging by electing a single winner and marking the
  rest `blocked-on/<n>`.

Until winner-election lands, mutual same-line conflicts are caught by the budget guard
(D4): they simply escalate rather than loop forever.

## 7. Auto-merge & trust

- **New capability `github:pr:merge`.** Merging is gated by an explicit capability in the
  same registry as `github:pr:write`, granted per workflow. `merge-review` declares it;
  nothing else has it.
- **V0.5 default: auto-merge ON for the goober-authored dogfood instance** (G2), but only
  on an **independent, conjunctive** signal: `merge-review` verdict = pass **AND** CI
  green **AND** not-draft **AND** SHA-pin still valid (D6). Never a bare self-approval.
- **Branch protection stays on.** The machine merges *through* the same gate a human
  would, it does not bypass it. Configuring `merge-review` as a required check + review
  keeps a human able to hold any PR.
- **Advisory mode** (comment + label only, human pulls the trigger) is a config toggle,
  and is the recommended default for any **mixed-company** repo (V1, §9).

> **Merge queue (#631, prep stage).** Once GitHub's merge queue is enabled on `main`,
> `merge-review`/auto-merge move from merging directly to enqueuing, and "pass" verdicts
> distinguish enqueued from merged — this section's content is superseded at that point,
> pending #758's `Land(pr)` abstraction. See
> [`merge-queue-operational-notes.md`](merge-queue-operational-notes.md) for what's
> known so far; not yet reconciled into this section's actual design.

## 8. Issue lifecycle — fix #355 here

With this loop, closing the originating issue on **PR-open** (today's `close-out`
behavior) is actively wrong — the work is not done until merged. The **merge event** is
the correct trigger for `close-out`: on merge, close the issue and drop the labels;
while a PR is open and cycling, the issue sits in an in-review state. This design is the
natural home for #355's fix.

## 9. Explicitly deferred to V1 / V1.1

These build **on** the V0.5 contracts without reworking them:

- **Mixed-company mode (V1).** Author-agnostic selection so `merge-review` reviews human
  and other-agent PRs; a "full review" vs "integration-delta" mode toggle; **human-active
  backoff** (defer remediation when a non-Goobers author recently pushed/commented);
  opt-out label (`goobers:no-merge-review`). Advisory-mode default for these repos.
  > **V0.6 ladder confirmation (L5, #369 + #368):** the round-2 run confirmed this need
  > concretely — with only schedule-poll triggers and self-applied-label selection, a
  > human (or other agent) leaving a PR review drives **no** automated action, and
  > `merge-review` never reads the PR's existing review thread. The desired shape is a
  > **state-driven** monitor that reconciles on PR facts (unresolved review threads from
  > *any* author, behind-base, CI, staleness) and feeds that thread as first-class
  > remediation input. Tracked in #369 (policy) + #368 (event trigger); hard-depends on
  > L1 (label writeback) and L3 (repass context). No new issue — augmenting those.
- **Native GitHub Review protocol (V1).** Post verdicts as GitHub reviews
  (approve/request-changes) so branch-protection stale-dismissal invalidates on push for
  free, required-checks gate the merge, and human reviewers see Goobers' verdict inline.
- **Winner-election + lazy rebase (V1)** — §7, reusing #350's ordering.
- **Robust oscillation detection (V1)** — verdict-finding-set hashing + A→B→A cycle
  history (§6 D5).
- **Event-driven triggering (V1)** — fire on review-comment/synchronize webhooks; gated
  on #342 + #169 + #343.
- **Cost control (V1.1)** — skip re-review of PRs whose `(headSHA, baseSHA, sibling-set)`
  is unchanged; maintain one **sticky status comment** updated in place instead of
  re-commenting each cycle. (Efficiency, explicitly out of V0.5 scope per G3.)

## 10. Dependencies & the canonical/sample-workflow rework

- **#155** (Tier-3 DSL: parallel branches + **child workflows / stage references**) —
  when it lands, `pr-remediation` and `merge-review` should **share the `review` gate and
  `local-ci` stage by reference** instead of copied YAML, and the shipped/canonical +
  sample workflows should be reworked to adopt stage-refs. (Until then: share the
  **goober**, copy the stage wiring.)
- **#342 / #169 / #343** — event-trigger substrate for the V1 reactive mode; when they
  land, the canonical + sample workflows should gain event-triggered variants.
- **#350** — backlog/PR **ordering policy**; reused for winner-election (§7).
- **#168** — durable pause/resume human-gate; the trust seam for advisory-mode and for
  routing the first production merges through a human until confidence is established.

## 11. Dispatchable issues (V0.5 unless noted)

Foundations first; each is intended to be a single reviewable PR.

1. **[epic] V0.5 — closing the loop: PR review → remediation → (auto-)merge.**
2. **`Verdict` schema + finding classes** (§4) — the handoff contract; `pass`/
   `needs-changes`/`fail`, classed findings, SHA-pin fields. Foundation for everything.
3. **`merge-review` workflow** (§2–4) — goober-authored PR selection, cross-PR
   sibling-set context-gathering stage, holistic review stage emitting the Verdict +
   sticky comment + label.
4. **`github:pr:merge` capability + auto-merge action** (§7) — capability registration,
   conjunctive merge gate, advisory-mode toggle.
5. **Post-merge fan-out + `close-out` on merge** (§7 D7, §8) — label now-behind PRs;
   move issue close to the merge event (**closes/So-relates #355**).
6. **`pr-remediation` workflow: `gather-pr-context` entrypoint** (§5) — enter on an
   existing PR; load Verdict + thread + behind/conflict state as context.
7. **`pr-remediation`: rebase-first, finding-driven routing** (§5 D3) — deterministic
   `force-with-lease` rebase; route to the shared `implement → review → local-ci → gates`
   chain only on conflict or substantive findings.
8. **Loop-control state: per-PR repass budget + same-diff escalation** (§6 D4/D5) —
   durable counter, liberal default, escalate label.
9. **Reframe #353** as the integration-review-altitude umbrella for this design (edit).

V1 / V1.1 follow-ons (filed, milestoned V1): native review protocol; winner-election +
lazy rebase; robust oscillation detection; event-driven triggering; mixed-company mode;
cost-control (skip-unchanged + sticky comment).

## 12. V0.6 ladder live-run errata (V0.7 remediation)

Running this V0.5 lifecycle end-to-end for the first time (via `goobers up`, the V0.6
eval ladder round 2) surfaced two defects in the shipped implementation. Both are
designed as frontload fixes in
[`docs/design/v07-ladder-remediation.md`](../v07-ladder-remediation.md); recorded here
against the design they belong to:

- **L1 — `merge-review` `apply-verdict` fails 100% when a PR is eligible.** The decider
  reaches a correct `decision:pass` (§4 verdict contract works), but `apply-verdict`
  aborts with `selectedNumber is required`: `gather-sibling-context`
  (`selfhost/gaggles/goobers/workflows/merge-review.yaml:64-66`) emits `selectedNumber`
  in its result but omits it from `expectedOutputs`, so the runner never threads it to
  `apply-verdict` (`:74-82`). One-line fix + a poll→select→review→apply integration test
  (its absence let 100%-broken wiring pass unit tests). Until fixed, **no PR ever
  receives a merge-review label** — the whole §3 label handoff is inert.
- **L7 — no terminal issue state in no-merge mode → false re-eligibility.** §8 says the
  issue sits `in-review` until the merge event closes it. But `goobers:claimed` is
  removed *nowhere* (`providers/github.go:1001-1008` only swaps `goobers/status:`
  labels), the ledger releases the claim on completion, and eligibility excludes
  in-review items only via a label written at PR-open with no open-PR backstop — so a
  completed rung's issue can become eligible again and be re-implemented into a duplicate
  PR. Fix: release `goobers:claimed` on close-out, add an open-PR eligibility backstop,
  and make the durable ledger authoritative (§3.3 of the remediation doc).
