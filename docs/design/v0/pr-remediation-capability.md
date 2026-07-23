# Design: making `pr-remediation` genuinely capable — evidence, policy, and response (V0.6)

> Status: **Draft for review** · Area prefix: `PRR` · Builds on: [`pr-lifecycle-loop.md`](./pr-lifecycle-loop.md) §5/§6
> Prerequisite: #392 / PR #933 (the workspace-branch handoff that lets the agentic chain run on the PR's own branch)
> Origin: the `weekend_10` observation round (2026-07-19), in which 6 of 9 PRs opened converged on the same terminal state via three unrelated root causes.

## 1. What this is for

The target is a `pr-remediation` that, at its most liberal setting, is not
meaningfully different from handing an agent a PR and saying **"get this past the
merge-reviewer."**

That target is not reached by giving one agent more autonomy. It is reached by
giving it the **evidence a human would have had**, and by letting the workflow
author **declare which problems are in scope**. Capability here is a function of
context and policy, not of latitude.

So this design holds three properties fixed, and will not trade them for capability:

- **P1 — DSL scoping.** Everything new is expressible, and *restrictable*, from the
  workflow YAML. An author who wants remediation to only ever fix rebase conflicts
  must be able to say exactly that.
- **P2 — Workflow shape.** Discrete stages, durable journaling, per-stage retry,
  per-stage capability grants, explicit gate routing. No stage becomes a black box
  that "does whatever it takes."
- **P3 — Composability.** Every new capability is its own stage. Dropping one from
  the YAML degrades the result; it never breaks the workflow. This is what makes P1
  real rather than aspirational.

## 2. Where we actually are

#392 (PR #933) fixes the *structural* blocker: the agentic `implement`/`review`/
`local-ci` chain can now run on the PR's own branch. That was necessary and it is
not sufficient. Once the agent is running, it is running nearly blind.

Below is every distinct path to `goobers:needs-remediation` in the system today,
and what a remediating agent actually receives on each. This table is the design
input; the rest of the document is a response to it.

| # | Cause | What reaches the agent |
|---|---|---|
| 1 | Substantive defect in its own diff | verdict JSON (via artifact pointer) |
| 2 | Merge conflict with base | one boolean |
| 3 | Base advanced | one boolean |
| 4 | **Failing CI** | **one boolean** |
| 5 | Cross-PR ordering (`blocked-on-sibling`) | excluded from remediation entirely |
| 6 | Sibling merged, file overlap | **exact reason computed, then discarded** |
| 7 | Sibling merged, now unmergeable | discarded |
| 8 | Provider error during triage | discarded |
| 9 | Merge queue eviction | comment prose only |
| 10 | Verdict `fail` | never remediated (correct — human judgment) |
| 11 | Budget exhausted / no progress | escalated out of the loop |
| 12 | `merge-pr` conjunct refusal | ~~no label at all~~ **since shipped**: durable per-head refusal counter → `goobers:merge-demoted` + reason trail (#950, `record-merge-refusal`) |
| 13 | Queue timeout | ~~no label at all~~ **since shipped**: `needs-remediation` + comment + reconciliation ledger (#886, `merge-queue-poll`) |
| 14 | Empty `needs-changes` verdict | labeled, but with zero findings to act on |

Four specific facts drive the design:

- **CI failure is a single bit.** `CheckDetail{Name, State, URL, Summary}` is already
  built by `combinedCheckState` (`providers/github.go:1149`) and thrown away by
  `ListPullRequests` (`github.go:898`). `gather-pr-context` reduces the whole thing to
  `hasFailingCI` (`gatherprcontext.go:266`). Nothing anywhere fetches check-run
  annotations or job logs. An agent told to fix failing CI is told nothing about what
  failed.
- **Inline review comments are never fetched.** `ListComments` hits
  `/issues/{n}/comments` (`github.go:1168`) — the issue-level thread only. The
  `/pulls/{n}/comments` inline threads and the native Review bodies are invisible,
  *including the changes-requested reviews `apply-verdict` itself submits*. The most
  specific feedback in the system is the feedback the remediator cannot see.
- **Cross-PR reasons are computed and dropped.** `triageSibling`
  (`postmerge.go:286`) derives `file-overlap:<path>` / `conflicted` /
  `mergeable-check-failed`, writes only the flat label, and discards the reason.
  `pr-remediation` has no sibling stage at all.
- **`Severity` routes nothing.** It is carried on every `Finding`
  (`api/v1alpha1/envelope.go:189`) and consumed by no routing logic —
  `verdictLabel` and `verdictHasSubstantiveFindingForPR` branch on `Class` only. An
  `info` and a `critical` finding are treated identically.

## 3. Decisions

### D1 — A remediation *brief*, assembled by discrete gatherer stages

Replace "five booleans threaded through `inputsFrom`" with a structured
`remediation-brief.json`, built up by independent gatherer stages that each own one
evidence source. Each is separately retryable, separately capability-scoped,
separately logged, and separately **omittable** (P2/P3).

| Stage | Status | Supplies |
|---|---|---|
| `gather-pr-context` | exists | verdict, thread comments, base state |
| `gather-ci-failures` | **new** | failing check names, conclusions, `output.summary`, annotations, job log tails |
| `gather-review-threads` | **new** | `/pulls/{n}/comments` inline threads + native review bodies |
| `gather-sibling-context` | exists in `merge-review`; **reuse here** | overlapping files, blocking PRs |
| `gather-issue-context` | **new** | the originating issue body + acceptance criteria, via `closingIssueNumbers` |

Non-obvious constraint, learned from `gatherprcontext.go:254-261`: `inputsFrom` drops
non-scalar values, so structured evidence **cannot** travel that way. It travels as an
artifact pointer, which is how the agentic stage already receives context. Gatherers
therefore write files; only scalars used for *gate routing* go through `inputsFrom`.

#### D1.1 — `remediation-brief.json` v1 contract

The closed schema is
`api/schemas/remediation-brief-v1.schema.json`; its wire identifier is
`goobers.dev/remediation-brief/v1`. Unknown fields are rejected. Any shape
change, including an additive field, publishes a new schema version rather than
silently widening v1. Writers emit one version and readers select support by the
wire identifier.

`gather-pr-context` owns the required top-level routing fields and the required
`gatherPrContext` section (SHA pins, full verdict or `null`, and the full
issue-level PR comment thread). Optional gatherers exclusively own their
namesake sections:

| Section | Owner |
|---|---|
| `gatherCIFailures` | `gather-ci-failures` |
| `gatherReviewThreads` | `gather-review-threads` |
| `gatherSiblingContext` | `gather-sibling-context` |
| `gatherIssueContext` | `gather-issue-context` |

Assembly is a replace-one-section merge. Each configured optional gatherer reads
the latest brief artifact, preserves the version, routing fields, required
section, and every section it does not own, replaces only its own section, and
emits the next complete brief artifact. The agentic stage consumes the artifact
from the last configured gatherer; when none are configured, it consumes
`gather-pr-context`'s brief directly. An absent gatherer means its property is
omitted — that is a valid v1 brief and never a stage failure. A gatherer that ran
and found no evidence emits its section with empty arrays, distinguishing
"checked and empty" from "not gathered."

Only `selectedNumber`, `head`, `base`, `hasSubstantiveFindings`, and
`hasFailingCI` continue through `inputsFrom`. `workspaceBranch` remains the
runner-interpreted branch-rebinding output. The full verdict, comments, and every
optional section travel only in the journal-lifted brief artifact.

### D2 — Remediation policy declared in the DSL, not compiled into Go

Today the scope of remediation is a hardcoded disjunction:

```go
needsAgent := conflict || hasSubstantiveFindings || hasFailingCI   // rebasepr.go:98
```

That is exactly the knob P1 says an author must hold. It becomes declared `inputs` —
no schema change, no new DSL concepts, per-workflow, and readable in the YAML:

```yaml
inputs:
  # Which causes this workflow will ATTEMPT. Most liberal = all of them.
  # Scope down by removing entries; an unlisted cause escalates untouched.
  remediate: "conflict,substantive,failing-ci,behind-base,sibling-overlap"
  # Findings below this severity are reported to the agent as context but do
  # not by themselves justify a remediation cycle. Severity is carried on every
  # Finding today and routed on by nothing.
  minSeverity: "warning"
  maxCycles: "3"
```

The default shipped config is the liberal one. An author who wants only mechanical
rebases writes `remediate: "conflict,behind-base"` and gets exactly that.

### D3 — Respond to the review, don't just silently re-push

A new terminal-side `respond-to-findings` stage posts a reply mapping **each finding
to what was done about it, or why it was not**. Two reasons this is a stage and not a
nicety:

1. It makes the loop legible to the human looking in — currently a remediation cycle
   is completely silent, and the only externally visible evidence that anything
   happened is a force-push.
2. It gives merge-review's next pass an explicit changelog to check against, instead
   of re-deriving intent from a diff.

This is also the concrete answer to "responding to comments" as a remediation
capability: the response is a declared, auditable stage output, not an agent side
effect.

### D4 — Honest escalation

#892 asked for this directly and the answer was deferred because nothing was ever
attempted. Once the chain is wired, `remediation-checkpoint` must record **what was
attempted** in its escalation reason, so `goobers:merge-escalated` distinguishes
*"tried, and could not converge"* from *"out of budget"* from *"this cause was not in
the declared policy."* A human triaging the escalated bucket currently cannot tell
these apart, and they call for completely different actions.

### D5 — Close the taxonomy gaps that make findings unactionable

- `FindingClass` declares `conflict` (`envelope.go:204`) and the reviewer instructions
  never say when to emit it. Either give it authoring guidance or remove it.
- Holistic mode collapses everything real into `substantive`. Single-diff mode's prose
  already distinguishes missing tests, scope creep, and changed load-bearing
  contracts — those are *different remediation tasks* and should be different classes.
- The reviewer is explicitly instructed not to evaluate CI, so CI failure can never
  appear as a finding. That is defensible, but it means the CI path needs its own
  evidence channel (D1's `gather-ci-failures`) rather than a finding class.

### D6 — Stop losing causes 12 and 13

`merge-gate: fail` (a `merge-pr` conjunct refusal) and `queue-gate: timeout` both
route to a terminal with **no label and no comment**. The PR silently drops out of
every selector with a `reason` string sitting in `merge-result.json` that no human
will ever read. Both should route to remediation, or at minimum leave a labeled,
commented trail.

## 4. What this explicitly does not do

- **No new autonomy for the agent.** It gets more evidence and a declared scope. It
  does not get merge capability, force-push outside `--force-with-lease`, or the
  ability to close PRs. `merge-review` keeps sole ownership of the merge path.
- **No god-stage.** The temptation is one agentic stage with broad capabilities and a
  "do what it takes" goal. That would trade P2 and P3 for a short-term capability
  bump, and would make every failure undiagnosable.
- **No change to the stuck-loop detector.** `remediation-checkpoint`'s 2-cycle
  no-progress escalation stays exactly as it is. With repair actually attempted, it
  starts measuring what it was designed to measure. It is the safety net that makes
  a liberal policy safe to run.
- **No lifting of the `goobers/` branch-namespace restriction** (PR #933). Remediating
  arbitrary human-authored PR branches is a real goal, but it is load-bearing for both
  a security property and the mirror's prune protection, and needs its own design.

## 5. Decomposition

Each row is independently landable as a single PR, in this order. Rows 2–5 are
mutually independent and can go in parallel once row 1 lands.

| # | Item | Depends on |
|---|---|---|
| 1 | `remediation-brief.json` contract + `gather-pr-context` emits it | #933 |
| 2 | `gather-ci-failures`: surface `CheckDetail`, annotations, log tails | 1 |
| 3 | `gather-review-threads`: inline comments + native review bodies | 1 |
| 4 | `gather-issue-context`: resolve `closingIssueNumbers`, load acceptance criteria | 1 |
| 5 | Sibling context in `pr-remediation` + persist `triageSibling`'s reason | 1 |
| 6 | Declared `remediate` / `minSeverity` / `maxCycles` policy inputs | 1 |
| 7 | `respond-to-findings` stage | 1 |
| 8 | Escalation reasons record what was attempted | 6 |
| 9 | Finding taxonomy: `conflict` guidance, split `substantive` | — |
| 10 | Route merge-refusal and queue-timeout to a labeled trail | — |

## 6. Open questions for review

1. **Log tails are unbounded.** `gather-ci-failures` needs a size discipline —
   truncate to the failing assertion? last N KB per failing job? This materially
   affects how useful the CI path is, and it is a genuine trade-off against context
   budget.
2. **Does `minSeverity` belong on the gatherer or the gate?** Filtering at gather time
   means the agent never sees sub-threshold findings; filtering at the gate means it
   sees them as context but they do not trigger a cycle. This design assumes the
   latter, but it is arguable.
3. **Should `respond-to-findings` run before or after `push-remediated`?** After is
   more honest (the response describes what actually landed) but leaves a window where
   the branch has moved and the thread has not.

## 7. Decisions taken (2026-07-19)

Recorded here so the open questions in §6 and the choices implied by §3 are not
re-litigated. Each was settled by the maintainer.

| Question | Decision |
|---|---|
| §6 Q1 — CI log volume | **Annotations + check summary, no raw log tails.** Highest signal per token, bounded by construction, no truncation heuristics to maintain. Accepted cost: an unannotated panic or build error gives less. Revisit only if that gap shows up in practice. |
| §6 Q3 — response ordering | **After `push-remediated`.** The response must describe work that actually landed. `respond-to-findings` is independently retryable and reconciles one run-scoped comment to keep the post-push visibility window bounded without duplicating replies. |
| Default remediation scope | **Maximally liberal** — conflicts, substantive findings, failing CI, stale base, sibling overlap, all on by default. Safe because the stuck-loop detector still bounds effort per cause. Scope down per-workflow if a category proves noisy. |
| Sibling overlap behavior | **Wait** — park `blocked-on-sibling` and let the existing self-heal unpark it. Do not rewrite a diff that would have been fine once the sibling landed. |
| Cycle budget shape | **Per cause, not flat** (issue #953). 2 per cause, ~6 total. A flat counter cannot distinguish three pointless rebases from three distinct problems solved in sequence. DSL-declared, not compiled in. |
| Election semantics | **Election means "those siblings stop blocking you", not merge authority.** Resolved in PR #949: `apply-verdict` now derives a genuine pass rather than `elect-gate` bypassing verdict publication entirely. |
| Human-authored PRs | **Out of scope here.** Punted to the mixed-mode epic #804 (with #805 actor classification, #807 trust policy, #369 mixed-company merge-review), where it belongs. Eventually configurable. |

### Consequences discovered while settling these

Verifying the sibling-wait decision surfaced that **cross-PR clusters have no
guaranteed forward-progress path**, which the design had assumed they did:

- A crowned lander that cannot merge re-wins its own deterministic election
  forever while its siblings stay parked behind it — no runner-up fallback, no
  timeout, no attempt counter (#950).
- Asymmetric reviewer findings can leave a cluster with **no** lander crowned at
  all, silently. The code acknowledges only the opposite hazard, double-crowning
  (#951).
- The crowned lander is on the critical path for its whole cluster and is
  currently selected with no added priority (#952).

None of these were visible from the workflow YAML; all three came out of
tracing what actually guarantees a merge. They are prerequisites for "wait" being
a safe default rather than a stall.
