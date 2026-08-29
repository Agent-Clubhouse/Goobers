# Design: needs-human label taxonomy — decision vs. status

> Status: **implemented** (2026-08-03)
> Area prefix: none (hygiene)
> Related: #2028, #1696, #1974, #2064's state-of-repo review
> Touches: `providers/model.go`, `cmd/goobers/runnerwiring.go`,
> `cmd/goobers/issuecloseout.go`, `cmd/goobers/backlogreconcile.go`,
> `reference-workflows/gaggles/goobers/workflows/implementation.yaml`,
> `config-examples/gaggles/acme-web/workflows/implementation.yaml`,
> both gaggles' `backlog-curation.yaml`

## 1. Problem

`goobers:needs-human` was documented as "a decision only a human can make," but
in practice every code path that parks an issue for *any* reason — a genuine
policy question, a self-healing dependency wait, a mechanical repass-budget
exhaustion, an infra blip — applied the same label. Measured on 2026-08-02: 71
open issues carried `goobers:needs-human`; three prior manual sweeps
([[needs-human-label-conflation]], [[needs-human-pass-2026-07-31]],
[[needs-human-sweep-2026-08-01]]) each found roughly 80-90% of the population
was not actually a decision — mostly sibling-dependency waits and
repass-exhausted execution stalls. The label stopped functioning as a decision
queue: genuine decisions were buried in status noise, and every sweep
re-accumulated the same noise because nothing in the automated pipeline applied
a more specific label.

Issue #1696 (open, unimplemented) targets one *producer* of this noise: the
backlog-curation goober's own prose judgment when it initially triages an
issue. That is a curator-quality problem — teaching the agent to recognize a
sibling-dependency or execution-stall pattern before ever applying a label —
and is out of scope here. This document instead fixes the *taxonomy itself*
(what labels exist, what each means, which code path applies which) and the
non-curator producers, so the decision queue stops re-polluting from every
other angle while #1696 is still open.

## 2. The model

Two categories, not one label:

- **Decision-required — `goobers:needs-human`.** A policy or product judgment
  only a human can make. Reserved for: an explicit reviewer `fail` verdict (the
  approach itself was rejected, not just its details); an unattributed block
  (the agent reported `blocked` but named no specific blocker in
  `outputs.blockedBy` — the runner has nothing to reason about); a detected
  circular dependency (structurally can't self-heal — a human has to break the
  cycle); and curator-flagged decision forks that genuinely have no
  mechanical resolution (e.g. #1677, #2080 — explicitly punted product
  questions).
- **Status/parked — no decision pending, someone/something needs to act.**
  - **`goobers:blocked-on-sibling`** — parked pending a specific named
    dependency (issue or PR) resolving. Self-healing in intent:
    `filterBlockedEligibility` (`cmd/goobers/blockedrecords.go`) already
    tracks the block internally and clears eligibility once every named
    blocker closes, independent of this label.
  - **`goobers:needs-remediation`** — parked after a mechanical failure: a
    repass-budget exhaustion, an identical-diff repass loop, an
    infrastructure/executor failure, or a CI-poll timeout. Someone needs to
    fix something (code, environment, or just retry); no policy question is
    open. Shared with the existing PR-lifecycle usage of the same label
    (`cmd/goobers/postmerge.go`, `remediationcheckpoint.go`) — same meaning,
    now also applied on the issue side.
  - Narrower existing PR-lifecycle-only labels — `goobers:merge-escalated`,
    `goobers:scope-gate`, `goobers:scope-drift` — are already correctly
    scoped to their specific PR scenarios (`docs/requirements/pr-lifecycle.md`
    PRL-063/PRL-064 and neighbors) and are unchanged by this document.

A quick test for which bucket a new park path belongs in: **if the only
question is "does someone need to act," it's status. If the question is "what
should happen next, and only a human can answer that," it's decision.**

## 3. Producer map (issue side)

| Producer | Trigger | Label before #2028 | Label after #2028 |
|---|---|---|---|
| `implementation.yaml`'s `park-needs-human` task | `review` gate's `fail` branch — reviewer explicitly rejected the approach | `goobers:needs-human` | unchanged — genuine decision |
| `implementation.yaml`'s `park-escalated` task | `review`/`local-gate`/`ci-gate`'s `escalate` branches, `local-gate`'s `infra` branch, `ci-gate`'s `timeout` branch — repass budget exhausted, infra failure, or a CI poll that never concluded | `goobers:needs-human` | **`goobers:needs-remediation`** |
| `runnerwiring.go`'s `buildBlockedHandler`, named-blocker case (`len(o.Blockers) > 0`, no cycle) | a stage reports `status: blocked` and names a specific blocker via `outputs.blockedBy` | `goobers:needs-human` | **`goobers:blocked-on-sibling`** |
| `buildBlockedHandler`, unattributed case (`len(o.Blockers) == 0`) | `status: blocked` with no named blocker | `goobers:needs-human` | unchanged — nothing to reason about but a human |
| `buildBlockedHandler`, self-referential case (#2961) | `status: blocked` naming only the driving issue in `outputs.blockedBy` | `goobers:needs-human` (via a spurious one-node cycle) | `goobers:needs-human` — the self-reference is dropped before persistence, so it parks as the unattributed case above with no cycle comment and no `blocked.json` record |
| `buildBlockedHandler`, circular-dependency case | a new block closes a cycle in `blocked.json` | `goobers:needs-human` (all cycle members) | unchanged — structural, can't self-heal |
| `backlog-curation.yaml`'s curator goober | curator's own prose judgment during initial triage | `goobers:needs-human` (curator's choice) | **unchanged in this PR** — #1696's scope |

`cmd/goobers/issuecloseout.go`'s `issue-close-out` command implements the
actual label swap for the first four rows via `status: "needs-human"` /
`status: "needs-remediation"` workflow inputs (`issueCloseOutStatus`,
`issueCloseOutIsParkStatus`). Only `status: "needs-human"` reads
`NeedsHumanAssignee` from instance config and assigns it
(`needshumanrouting.go`'s `withNeedsHumanAssignee` keys specifically on
`providers.LabelNeedsHuman`) — a `needs-remediation` park is never assigned to
the configured human, since no decision is pending on it.

## 4. Guardrails against re-pollution

- Both gaggles' `backlog-curation.yaml` `query-backlog` stage now excludes
  `goobers:blocked-on-sibling` and `goobers:needs-remediation` alongside
  `goobers:ready`/`goobers:needs-human`, so a parked issue is never re-offered
  for curator triage while it's still parked.
- `cmd/goobers/backlogreconcile.go`'s `ready`+park contradiction check
  (`itemHasParkLabel`) now covers all three park labels, not just
  `needs-human` — a `ready` label that somehow survives alongside any park
  disposition is stripped on the next reconciliation pass, same as before.

## 5. Known limitations (not fixed here)

- **`park-escalated` is still a single bucket for four distinct causes**
  (repass exhaustion, infra failure, identical-diff loop, CI-poll timeout).
  All four now land on `goobers:needs-remediation`, which is directionally
  correct for all four (someone needs to act, no policy question), but a
  CI-poll timeout in particular could eventually warrant its own, narrower
  disposition. Splitting `park-escalated` by originating gate would need a
  second park stage per cause and is left for a follow-up if the coarser
  bucket proves too noisy in practice.
- **No automated issue-side "unpark" exists yet.** The PR-lifecycle side has
  `unparkResolvedSiblings` (`cmd/goobers/postmerge.go`) to clear
  `goobers:blocked-on-sibling` from a PR once its blocker merges. No
  equivalent exists for *issues* — `filterBlockedEligibility`'s self-heal only
  clears the internal `blocked.json` record and re-admits the item for
  re-claim; it does not remove the label or restore `goobers:ready` on the
  issue itself. A blocked-on-sibling issue's label can therefore go stale
  (accurate at the moment it was applied, silently outdated once the blocker
  closes) until a human or curator pass notices. This is a real gap, but it
  existed identically for `goobers:needs-human` before this change — it is
  not a regression, just not yet fixed.

  Since #3355 the gap is at least *visible*: `goobers status` reports a
  "Parked backlog items" section listing every open item that carries a park
  disposition without `goobers:ready` (`parkedBacklog` in `status --json`).
  Such an item has left the instance's ready pool — `query-backlog` requires
  `goobers:ready` — and nothing configured puts it back, so on an unattended
  instance the park otherwise reads as work silently deleted from the backlog.
  Re-entry is still an operator action: triage the item and re-add
  `goobers:ready`, or close it. Automatic reconsideration (a curation resweep
  that re-grades parked items with backoff) is the follow-up.
- **#1696 (curator judgment) is still open.** The curator can still apply
  `goobers:needs-human` for a case that's actually a sibling dependency or
  execution stall at *initial triage* time, before the item ever reaches
  `implementation.yaml`'s park paths. This document's fixes stop the
  non-curator producers from re-polluting the queue; #1696 is the remaining
  producer.

## 6. Cleanup pass

The population of issues carrying `goobers:needs-human` as of 2026-08-02 (71
open) was reclassified against this model in the same PR — see the PR
description for the exact counts and dispositions.
