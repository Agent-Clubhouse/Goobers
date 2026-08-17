# Design: Decomposition workflow

> Status: **draft — proposed for staged implementation** (2026-08-02)
> Area prefix: `DEC`
> Related: #318, #419, #415, #489, #491
> Builds on: `implementation`, `backlog-curation`, the claim ledger, and the
> non-retryable escalation disposition in `docs/stage-contract.md`

## 1. Purpose

The implementation workflow is an executor: it should turn one scoped issue into
one pull request. It is correct for the implementer to refuse an issue that cannot
meet that contract. Since #415, an implementer can return `failure` with
`retryable: false` and `ISSUE_OVER_SCOPE` or `NEEDS_DECOMPOSITION`; the runner
bypasses review and ends the run `escalated`.

That terminal state is useful only if another workflow consumes it. The
`decomposition` workflow is the inverse of the executor:

1. select one unconsumed L6 decomposition disposition;
2. design a coherent breakdown of its parent issue;
3. publish independently implementable child issues; and
4. make those children `goobers:ready` so ordinary implementation runs can claim
   them.

Decomposition is curation, not implementation. It mutates issues but never code or
pull requests, and it does not weaken the implementation workflow's one-issue,
one-PR boundary.

## 2. Source signal and eligibility

For reactive selection, the source of truth is the implementation run journal,
not a forge label or the wording of an issue comment. `goobers:needs-human` is
shared by several unrelated dispositions and is therefore too broad to select on.

A run is eligible only when all of these facts hold in its current, unrecovered
terminal segment:

- its terminal phase is `escalated`;
- a stage finished with `status: failure`, `error.retryable: false`, and
  `error.code` equal to `ISSUE_OVER_SCOPE` or `NEEDS_DECOMPOSITION`;
- the runner took the non-retryable escalation route defined by #415; and
- the run's claimed input resolves to an open, maintainer-approved parent issue.

Repass-budget exhaustion, CI timeouts, dependency blocks, reviewer rejection, and
other escalated runs do not qualify. A matching error code on a run that later
resumed and completed also does not qualify. The selector must derive the current
terminal segment in the same way as the run-operator surfaces rather than grep old
events.

The immutable selection artifact records `mode: escalation` plus the source run
ID, workflow and stage, error code and message, parent
provider/repository/ID, the parent's observed revision, and the digest of the
claimed issue snapshot. The error message is evidence for the decomposer, not
executable instruction.

Parameterized, explicitly invoked runs add a second selection mode (§3.1). Their
authority is the operator-supplied target plus the parent's maintainer approval,
not a fabricated L6 event. They record `mode: pointed` and the manual invocation
ID instead of an escalation code.

### 2.1 Exactly-once ownership

Selection and mutation use two identities:

- **source identity:** `(gaggle, source run ID)` identifies an escalation
  disposition, while `(manual invocation ID, parent ID)` identifies pointed work;
- **batch identity:** `(provider, repository, parent ID, plan digest)` identifies
  the decomposition that is published.

The selector acquires the existing claim-ledger lease for the parent before
returning it. It then re-reads the live issue and fails closed if it is closed,
unapproved, in review, already claimed by another run, or already in a different
decomposition batch. Concurrent selector runs cannot own the same parent.

Several old escalation runs may point to the same parent. The oldest eligible run
owns the first pass. Once the parent has a prepared or published batch marker, all
matching source runs for that parent resolve to that batch rather than creating a
second one. The selector may scan a bounded window repeatedly; durable parent
markers, not a fragile high-water cursor, determine whether an item was consumed.

## 3. Workflow shape

The conceptual workflow is:

```text
select-source -> design-slices -> validate-plan -> publish-slices
                    ^                |
                    | needs-changes  | exhausted/conflict
                    +----------------+---------------------> park-for-human
```

### 3.1 Entry modes

The checked-in V0 definition declares one `schedule` trigger. That gives two valid
ways to enter the same state machine:

- the scheduler periodically looks for an eligible L6 disposition; and
- an operator runs `goobers run decomposition`, which invokes the same selector
  immediately.

This is intentionally **not** expressed as `manual` plus `schedule`. The current
workflow schema requires `type: manual` to be the only trigger, so that pair does
not compile. Every workflow can already be invoked explicitly with `goobers run`;
the schedule declaration controls autonomous firing.

Initially, both entry paths select the next eligible disposition. Pointing a
manual run at a specific issue or milestone requires parameterized run inputs
(#491). When that contract lands, an optional target puts the selector in
`pointed` mode:

- an issue target selects that issue;
- a milestone target selects one approved, unclaimed, not-yet-decomposed issue
  from the milestone per run; and
- pointed inputs are accepted only when the invocation provenance is manual, never
  on a scheduled firing.

Pointed mode is deliberately not required to invent or already have an L6
escalation: its purpose is to design and slice known-large work before wasting an
implementation attempt. It still requires the maintainer-applied approval label,
an open issue, and the same claim and conflict checks. A manual target must never
turn an arbitrary unapproved issue into decomposition work.

### 3.2 Stage contracts

| Stage | Kind | Responsibility | Side effects |
|---|---|---|---|
| `select-source` | deterministic | Select an exact L6 disposition or admitted pointed target, claim one parent, and emit the immutable selection artifact | claim only |
| `design-slices` | agentic | Read the parent, source rationale, linked issues, and architecture/design context; produce a decomposition plan | none |
| `validate-plan` | deterministic | Validate schema, trust inheritance, single-PR boundaries, dependency graph, labels, and observed parent revision | none |
| `publish-slices` | deterministic | Resume or create the batch, link children, update the parent, and cross the publication barrier | issue mutations only |
| `park-for-human` | deterministic | Explain an invalid or conflicting plan, remove `goobers:ready`, add `goobers:needs-human`, and release the claim | parent disposition only |

The agentic stage has read-only issue and repository context plus `agent:model`; it
does not receive an issue-write capability. All issue creation, editing, linking,
labeling, and commenting is performed by the deterministic publisher from a
validated plan. This makes retries reproducible and prevents a model session from
publishing half of a batch before its output has been checked.

Design and slicing remain one agentic stage. The boundaries of the children depend
on the chosen technical shape, so splitting design-authoring from slicing would
introduce a second lossy handoff without making publication safer. A future model
tiering feature may use a stronger model for this stage, but model selection is
not part of this workflow contract.

A structurally valid but inadequately scoped plan returns the validator's bounded,
deterministic findings to `design-slices` through the ordinary repass context. A
schema-invalid artifact fails closed without publication. Only a parent conflict,
an explicitly unresolved product decision, or exhausted design repasses reaches
`park-for-human`.

## 4. Decomposition plan

`design-slices` emits one versioned JSON artifact. At minimum it contains:

```json
{
  "schemaVersion": "v1",
  "selection": {
    "mode": "escalation",
    "sourceRunId": "run-id"
  },
  "parent": {
    "provider": "github",
    "repository": "Agent-Clubhouse/Goobers",
    "id": "419",
    "observedRevision": "provider revision"
  },
  "summary": "Why these slices form one coherent delivery plan.",
  "unresolvedDecision": "",
  "children": [
    {
      "key": "selector",
      "title": "Add decomposition disposition selection",
      "body": "Problem, scope, acceptance criteria, and parent link.",
      "labels": ["area:workflows", "type:feature"],
      "dependsOn": []
    }
  ]
}
```

`key` is stable within the plan and is used in child idempotency markers and
dependency edges. The plan digest is computed only after canonical serialization
and validation.

The validator requires:

- `unresolvedDecision` to be empty before publication; a non-empty question is
  emitted as a scalar routing signal and parks the parent for a human decision;
- at least two children unless the plan explicitly rewrites the parent into one
  smaller replacement and explains why no split is needed;
- unique, stable child keys and non-empty titles, bodies, and acceptance criteria;
- each child to describe one coherent change that can produce one PR and name a
  plausible validation boundary;
- an acyclic dependency graph using only child keys or existing issue IDs;
- no child that merely says "finish the rest", duplicates another child, or
  requires another child to edit the same contract atomically;
- only allowlisted area/type labels in the plan; trust and readiness labels are
  publisher-owned and cannot be requested by the model;
- the selection mode and its source identity, parent identity, parent observed
  revision, and issue snapshot digest to match the selector artifact; and
- a parent summary/checklist that accounts for every child exactly once.

If the live parent changed after selection, validation does not silently regenerate
the plan. A semantically irrelevant claim breadcrumb may be ignored, but a title,
body, label, state, hierarchy, or dependency change is a conflict. The workflow
parks the parent with the exact conflict and leaves the plan artifact for review.

## 5. Crash-safe deterministic publication

Forge APIs do not offer a transaction spanning several issues. Publication
therefore uses a recoverable prepare/publish protocol and a single parent-side
eligibility barrier. No child is claimable merely because its shell exists.

### 5.1 Stable markers

The publisher owns machine-readable, versioned records on the parent and markers
on the children:

- parent prepared record: source IDs, plan digest, and ordered child keys/IDs;
- parent published record: the verified plan digest and ordered child IDs;
- child marker: parent ID, plan digest, and child key.

The parent records are append-only comments. Appending the published record is the
one-operation commit point; changing a body and a label cannot be treated as an
atomic forge transaction. Comment and child lookup confirm exact markers rather
than relying on eventually consistent free-text search. Provider creation must
accept per-action idempotency keys for child creation and record comments. Reusing
the run ID for every child is invalid because the existing create-item idempotency
footer identifies only one item per run.

Every mutation is guarded by the parent/child revision observed immediately before
the write and runs under the shared target lease. A retry first reads live state,
adopts an exact marker match, and rejects a conflicting match. It never guesses,
blindly repeats a POST, or deletes an unexpected issue.

### 5.2 Prepare

1. Mark the parent `tracking` and `goobers/status:decomposing`, remove
   `goobers:ready` and `goobers:needs-human`, and append the prepared batch record.
2. Create or adopt every child shell with its stable key. Shells inherit
   `goobers:approved`, but do **not** receive `goobers:ready`.
3. Attach native parent/child relationships where supported and apply declared
   dependency links.
4. Replace the parent body with the design summary and complete ordered checklist,
   preserving unrelated human-authored context below the tracking section.
5. Re-read the complete batch and verify every title, body digest, label, link,
   dependency, and checklist entry against the plan.

Any crash in this phase leaves the parent in `decomposing` and all created children
quarantined. A retry resumes by stable key. An irrecoverable conflict leaves the
same quarantine in place and parks the parent for a human; partial shells are not
deleted because deletion is destructive and would lose audit history.

### 5.3 Publish

After prepare verification:

1. add `goobers:ready` to every child and verify the complete set;
2. append idempotently keyed explanatory comments to the parent and children;
3. append the parent published record as the single batch commit point;
4. remove `goobers/status:decomposing`; then
5. release the parent claim.

Implementation's deterministic backlog selector must treat a decomposition child
as ineligible while its parent published record is absent or conflicting, even if
the child already carries `goobers:ready`. The published record is the batch-wide
commit point. Consequently, a crash while adding ready labels cannot expose a
partial batch. A crash after the published-record POST but before its response is
received is also safe: the retry finds the exact record, verifies the batch,
finishes label cleanup, and releases the claim without re-commenting. A stale
`decomposing` label after the commit is cleanup drift, not a reason to hide an
otherwise verified batch.

The batch marker and issue hierarchy are forge-resident because the backlog is the
system of record. The run journal records ordinary stage results, artifacts, and
external refs; this design does not add a new journal event or alter the result
envelope.

## 6. Resulting issue lifecycle

On successful publication:

- the original issue remains open as an approved `tracking` parent;
- the parent has neither `goobers:ready` nor `goobers:needs-human`;
- every child inherits `goobers:approved`, has exactly one type label, carries the
  relevant area labels, and becomes `goobers:ready` at the publication barrier;
- dependencies order children where order is real, but independent children remain
  independently claimable; and
- ordinary backlog curation keeps the tracking checklist synchronized as children
  land.

The decomposition workflow does not implement, merge, or close children. The
existing implementation and merge-review workflows own those transitions.

If the source issue is already correctly decomposed, the publisher adopts and
verifies the existing batch, records the source run as consumed, and performs no
visible mutation. If the issue closed or became obsolete before selection, the
selector records `no-work`. If a human-authored decomposition conflicts with the
plan, the workflow preserves both and asks for a specific decision rather than
overwriting either.

## 7. Security and failure behavior

- SEC-047 is unchanged. A source parent must already have the maintainer-applied
  approval used by the implementation run or required by pointed selection.
  Children inherit that approval only from this verified parent.
- Issue text, escalation messages, and linked issue content remain untrusted data.
  They inform the plan but never change workflow instructions, capabilities, or
  mutation targets.
- The decomposer cannot write issues. The publisher accepts only the closed plan
  schema and mutates only the selected parent plus children named by stable keys.
- Provider errors fail the stage with their typed retryability. A business conflict
  is non-retryable and parks the batch; a transient transport failure resumes the
  protocol.
- Releasing a claim is the final action. Terminal cleanup may release a failed
  run's lease, but it must not remove the `decomposing` quarantine or mark the
  source consumed.

## 8. Delivery slices

Each row is one independently reviewable, single-PR implementation seed. The
children should be filed from this design and linked back to #419.

| Slice | Depends on | Acceptance boundary |
|---|---|---|
| **DEC-1 — Select and claim L6 decomposition dispositions** | #415 | A deterministic command derives current terminal segments, accepts only non-retryable `ISSUE_OVER_SCOPE`/`NEEDS_DECOMPOSITION`, resolves and claims the parent, deduplicates multiple source runs, and emits the immutable selection artifact. Fixtures cover resumed runs and every excluded escalation class. |
| **DEC-2 — Versioned decomposition-plan schema and validator** | DEC-1 | Add canonical plan types and validation for source binding, single-PR child criteria, label allowlists, acyclic dependencies, stable keys, and parent revision conflicts. Invalid plans produce no provider mutations. |
| **DEC-3 — Idempotent child issue and hierarchy primitives** | — | Provider-neutral issue creation and marker comments accept distinct stable idempotency keys per action, exact marker lookup, native parent attachment where supported, revision guards, and target leases. Lost-response and concurrent-create tests prove one child and one record per key. |
| **DEC-4 — Prepared-batch publisher and eligibility barrier** | DEC-2, DEC-3 | A deterministic publisher creates quarantined shells, resumes every crash boundary, verifies the complete batch, and commits through one parent published record. `backlog-query` cannot claim any marked child before that commit. Tests inject a failure after every provider mutation. |
| **DEC-5 — Ship the self-hosted workflow and decomposer persona** | DEC-1, DEC-2, DEC-4 | Add the schedule-triggered workflow, read-only design goober, capabilities, policy actions, bounded readiness, release/park paths, and definition validation tests. Both scheduler firing and `goobers run decomposition` traverse the same selector. |
| **DEC-6 — Pointed manual decomposition** | #491, DEC-5 | An optional issue or milestone run input selects approved work without requiring a prior implementation escalation. Inputs are admitted only for manual invocation provenance and never bypass open-state, approval, claim, or existing-batch checks; malformed and unapproved targets fail closed. |

DEC-1 through DEC-5 deliver reactive escalation consumption and immediate
unpointed operator invocation. DEC-6 is additive and must not block that path.

## 9. Non-goals

- General re-curation of repeatedly failing work; the backlog-curation engine owns
  that broader feedback loop.
- Treating every `escalated` or `goobers:needs-human` item as decomposable.
- Automatically approving work that did not originate from an approved parent.
- Dynamic workflow fan-out or child workflows. The decomposition batch is data
  published by one linear workflow, not runtime branch width.
- Implementing child issues, opening PRs, or deciding merge order.
- Per-stage model selection.
- Changing the run-journal event schema, result envelope, or claim-ledger contract.
