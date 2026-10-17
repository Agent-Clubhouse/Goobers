# Reviewer dispositions

Reviewer decisions distinguish changes to make, waiting for siblings, terminal
rejection, and a mechanical stop. The original rationale and findings remain
part of the verdict artifact and published provider comment; operator run
summaries expose `reasonCode`, `findings`, `synthesized`, and
`legacyFailAmbiguous`.

| Decision | Meaning | Reason codes |
|---|---|---|
| `pass` | Review permits the next configured step; other merge checks still apply | None |
| `needs-changes` | Repairable implementation findings | None |
| `fail` | Terminal implementation or policy rejection | `implementation-rejected`, `policy-rejected` |
| `defer` | Withhold landing authority for sibling ordering | `ordering`, `no-lander` |
| `escalate` | Mechanical stop, not substantive rejection | `empty-diff`, `unchanged-repass`, `repass-budget-exhausted`, `finding-set-oscillation`, `remediation-evidence-not-inspected` |

Typed rejection, deferral, and escalation require a non-empty rationale.
Deferrals and mechanical stops cannot claim `elected: true`.

## Runner-synthesized verdicts

When the subject stage commits an empty diff, or a repass reproduces the diff
the reviewer already judged (unchanged repass), the runner does not invoke the
reviewer. It writes a verdict with the usual shape plus `synthesized: true`,
and the `findings` field is absent rather than an empty list. Absent findings on
a synthesized verdict mean that no review ran, not that the review was clean.
Reviewer-produced verdicts never carry `synthesized`; the runner clears the
field if a reviewer sets it. `goobers trace --verdicts`, `goobers status`, and
operator run summaries surface the marker.

## Workflow opt-in

DSL 3.0 is preview. An agentic gate declares a `defer` branch to opt into the
expanded reviewer vocabulary. The runner derives the prompt capability from
that branch, not task inputs. Declaring both `defer` and `escalate` also opts
the gate into typed runner-generated empty-diff, unchanged-repass, and
exhausted-repass-budget stops. Route those branches to distinct tasks as needed:

```yaml
branches:
  pass: publish
  needs-changes: implement
  fail: record-rejection
  defer: park-ordering
  escalate: record-mechanical-stop
```

These are branch targets, not built-in task names: the workflow must define
each referenced task. Existing three-outcome gates keep their original prompt
contract and synthesized stop vocabulary. Unexpected deferral from a producer
without a declared route fails closed. The frozen DSL 2.0 interpreter does not
accept the new deferral branch.

## PR publication and election

`apply-verdict` turns ordering-only needs-changes into `defer / ordering` after
single-lander election; actual repairable findings still remain needs-changes.
A no-lander result becomes `defer / no-lander`, not an implementation rejection.
The provider-native review is comment-only for deferral and mechanical stops.
No-lander deferrals do not queue a supposedly crowned lander.

The election reader excludes a no-lander candidate only when the trusted
canonical verdict matches the current head and base. A changed pin permits
re-evaluation; an untrusted comment cannot exclude a candidate. Neither kind
of deferral satisfies the SHA-pinned pass check required to merge.

The publication result's `reason` output carries the structured reason for
downstream routing. Finding-set oscillation is published as mechanical
escalation with its original findings, not as substantive `fail`.

## Standing findings and finding dispositions

A pull request can receive several merge-review verdicts for the same head.
This happens after a label change, after a base-branch advance, after a cache
miss, or after a remediation cycle that changed nothing. Goobers applies one
deterministic rule, in `internal/findingledger`, to decide which of the
earlier findings still stand (#5890).

### The rule

The ledger is keyed on the pull request **head**. Advancing the base branch
does not change the PR's own diff, so it does not clear a finding about that
diff. A new head is new code, and the next review judges it in full.

The ledger reads every published verdict for the head in publication order.
For each verdict:

- Its complete dispositions remove the findings they name.
- Its `standingFindings` (the ledger it carried when it was published) stand.
- If it is not a pass, its own substantive findings stand. A finding is
  substantive when it has a code-change class (`conflict`, `substantive`,
  `missing-tests`, `scope-creep`, `contract-change`) or no class, and is
  about this PR.

A disposition is complete when it is `resolved` with `evidence` or `rejected`
with a `reason`. A disposition without them answers nothing. A pass raises
nothing, and a pass that says nothing about a finding does not remove it.
Re-raising a standing id under a non-blocking class, such as `rebase-needed`
or `cross-pr-blocked`, does not answer it either. Only a disposition clears a
finding.

```json
{
  "decision": "pass",
  "headSha": "…",
  "baseSha": "…",
  "dispositions": [
    {"findingId": "unbounded-lineage-lookup", "findingGeneration": 7, "outcome": "rejected", "reason": "The scan is bounded by the caller's page size."}
  ]
}
```

### Where the ledger lives

The canonical status comment and the scheduler-state record each hold one
verdict, and every new publication replaces them. To survive that,
`apply-verdict` writes the ledger into every verdict it publishes, as the
runner-owned `standingFindings` field. This covers every decision:

- pass and needs-changes;
- deferrals and mechanical escalations;
- a bare `fail` or a typed rejection;
- a runner-synthesized verdict;
- an advisory publication.

`standingFindings` is kept apart from `findings`, so the ledger never changes
what a decision means. A typed rejection still publishes, and a fail still
does not authorize a merge. Earlier dispositions are carried forward the same
way, except a disposition for a finding that stands again.

Records are read only from the publishing identity's marked status comments
and the scheduler-state record. Other comments carrying a verdict payload are
ignored, such as a remediation response quoting agent text. On GitHub and
Gitea the canonical status comment is edited in place, so it is read as the
newest comment. On Azure DevOps the thread order is publication order. The
scheduler-state record is read last.

### Concurrent publications and generations

The runner folds each candidate against the latest scheduler-state record inside
its locked update or compare-and-swap retry. The committed verdict has a
`ledgerGeneration`; every newly raised finding has its own `ledgerGeneration`.
An unchanged finding that remains open keeps its generation. A changed finding,
or one raised again after it was answered, receives a new generation. Reviewer
input cannot assign these generations or inject `standingFindings`.

Each committed record is a complete snapshot. Readers select the highest
committed generation for the head, so delayed comments cannot resurrect findings
that a newer snapshot resolved. Before the first committed generation, the
legacy publication-order replay collects every outstanding finding for migration.
After migration, writers must use the atomic publication path; older writers
cannot extend a committed ledger through generation-zero comments.

Copy the standing finding's generation into the disposition's
`findingGeneration`. An answer for an older instance cannot clear a newer one
with the same ID. A missing generation answers only a legacy finding that also
has no generation. A pass that omits or miscopies a generation stays blocked;
rerun the reviewer with current standing-finding context to answer it.

Publication captures the previous record before checking the live PR. Replacing
another head requires that predecessor to remain unchanged. If it changed,
publication fails and a new run must check the live PR again. Concurrent reviews
of the same head instead retry and preserve each other's findings. This does
not infer chronology from commit hashes or make provider reads atomic with state.

Native reviews, comments, labels, statuses and stage results all use the committed
fold. ADO commits before choosing its pass or non-pass transport. A failed state
write submits no verdict projection. Native projection is still eventually
consistent: a delayed request already submitted to a provider can finish after a
newer commit. This transaction does not fence remote HTTP completion or establish
cross-gaggle publisher ownership; one logical publisher must own the repository
status context. Goobers-controlled landing checks authoritative findings under
its lock. Native-only landing requiring zero stale exposure needs a separate
publication-custody policy; no provider policy is changed by this feature.

### Enforcement

| Stage | What a standing finding does |
|---|---|
| `gather-sibling-context` | Lists it as `standingFindings` in the context the reviewer reads. |
| `apply-verdict` | Writes it to the published verdict's `standingFindings`. On a pass or needs-changes it is also listed in `findings`, and a pass is published as `needs-changes`. The PR is then labelled `goobers:needs-remediation` and never reaches `merge-pr`. On Azure DevOps the downgraded pass goes to the non-pass publication. |
| `update-behind-pr`, `gather-pr-context`, `rebase-pr` | The verdict they read lists it, so a current branch is not "mechanically healed". The PR goes to full remediation, and the remediation brief lists the finding. |
| `merge-pr` | Refuses to land while one stands on the live head. It reads the records under the merge lock. |

Unreadable evidence fails closed:

- `merge-pr` refuses when it cannot read the scheduler-state record or the
  Azure DevOps threads, or cannot name the publishing identity.
- `update-behind-pr` and `gather-pr-context` fail the stage instead of
  clearing a label.
- `apply-verdict` fails the publication.

### Enforced, not shadowed

The #6021 plan was detect-only first, then enforce. This check enforces from
the start, because it fixes a wrong merge that was observed: a production
instance merged an unchanged revision over an unanswered substantive finding.
A detect-only phase would allow that merge again.

Every enforcement journals a `runner.annotation` event, so blocks can be
counted the way the shadow plan meant to count would-blocks. Fields:
`annotation: merge-safety-block`, `check: undispositioned-finding`, `stage`,
`action` (`route-full-remediation`, `carry-findings`, `downgrade-pass`,
`refuse-merge`), `pullNumber`, `headSha`, `baseSha` and `findingIds`.

### Clearing a wrong finding

A standing finding clears in only two ways: a later review of the same head
dispositions it, or a new head is pushed. A later review that says nothing
about it does not clear it.

The normal path is remediation. The PR carries `goobers:needs-remediation`, so
merge-review does not select it. If remediation pushes a fix, the new head is
reviewed in full. If remediation makes no change, the head stays the same, the
label stays on, and repeated no-op cycles eventually park the PR for a person.

There is no single command that re-reviews an unchanged head on demand. A
same-head review needs two things:

1. **merge-review must select the PR again.** Route it there with
   `goobers pr retry --pr <n> --route merge-review --reason <text>`, which
   clears the remediation and escalation labels.
2. **The reviewer must actually be called.** For an unchanged head and base,
   `gather-sibling-context` replays the published `needs-changes` from its
   verdict cache, and that puts the label back. The reviewer is called again
   only when the cache key changes or the cache is bypassed:
   - **The base branch advanced since the last review.** This is often true,
     and needs no action.
   - **Bypass the cache yourself.** Add `--no-verdict-cache` to
     `gather-sibling-context`'s `run.command` in the merge-review workflow,
     run `goobers run --no-api --pr <n> merge-review` from the instance root,
     then remove the flag.

The fresh review is shown `standingFindings` and can mark each one `resolved`
or `rejected`.

`goobers rerun-stage --addendum` does not help here. It only targets an
escalated run, and a merge-review run whose pass was published as
`needs-changes` completed normally.

On Azure DevOps, `gather-sibling-context` has no sibling-evidence path, so
the reviewer is not shown `standingFindings`. Enforcement still applies there,
and a new head is the practical way to clear a finding.

### Severity floors

`merge-pr` and `apply-verdict` count a standing finding at the `info` floor,
whatever its severity. The healers' `minSeverity` input does not lower that
bar for a standing finding: every healer routes a PR with a standing finding
to full remediation, so a finding that blocks the merge is never healed past.
`minSeverity` still filters a verdict's own new findings, as before.

### Known limits

- Committed state is atomic, but provider projections remain eventually
  consistent. An in-flight HTTP request can finish after a newer publication;
  this change does not fence native status completion across publishers.
- A branch force-pushed back to an earlier head finds that head's ledger
  already replaced by the later head's records. That head is reviewed in full,
  like any other.
- A new head always starts an empty ledger, even when it only rebases the same
  diff.

## Legacy verdicts

Old producers may still emit `fail` without a reason. Such verdicts remain
readable, but provider comments and publication results identify them as
`legacy-fail-ambiguous`; operator summaries set `legacyFailAmbiguous: true`.
Do not infer implementation rejection from that legacy decision alone.
The original verdict is not rewritten to invent a rejection reason.
