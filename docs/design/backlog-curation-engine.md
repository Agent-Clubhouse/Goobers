# Design: Backlog curation engine — continuous, reliable, agile-inspired

> Status: **Approved for staged implementation** (2026-07-22)
> Area prefix: `CURE`
> Milestone: **Backlog curation engine — continuous, reliable, agile-inspired**
> Related workflow: `backlog-curation` · Related goober: `curator` (the backlog's scrum master)
> Builds on shipped: `cmd/goobers/backlogquery.go`, the `curator` persona, `work-nomination`

## 1. Why this exists

The curator goober is already a capable autonomous scrum master: for a claimed
batch of `goobers:approved` items it dedupes, stale-labels, applies `area:`/`type:`
tags, splits epics, and biases hard toward moving `approved → ready` so the
implementation workflow always has something to pull. That is the right spine and
this design keeps it.

But living with it surfaced a consistent shape: curation today is a **one-shot,
forward-only triage pass** wrapped almost entirely in persona prose, with no way
to keep the *rest* of the backlog true. The engine can move an item to `ready`,
but it cannot keep the backlog continuously correct — reconcile its own labels,
revisit items whose world has changed, organize work into milestones, or prove
that the "ready" pile is genuinely shovel-ready. The ambition here is agile
backlog curation in the real sense: **a continuously-running engine that keeps the
whole backlog's state and metadata true, and guarantees the team a supply of
relevant, shovel-ready work** — not just a labeler that runs once per item.

## 2. Honest current state

Grounded in the code and a live self-hosting instance:

- **Curation is a thin deterministic shell around an all-LLM persona.** `query-backlog`
  (claim), `curate` (agentic curator), `release-claim` (token-free ledger release).
  Dedupe, staleness, tagging, splitting, and ready/needs-human marking are 100%
  the curator's prose judgment — nothing is deterministic or verifiable beyond the
  claim mechanics.
- **Forward-only, on uncurated items only.** Selection uses
  `excludeLabels: goobers:ready,goobers:needs-human`, and the persona is a
  deliberate no-op on anything already curated. So once an item is `ready`, nothing
  ever re-examines it — a `ready` item whose blocker just merged, whose goal went
  obsolete, or that should be re-scoped is never revisited.
- **The label mirror leaks.** `goobers:claimed` is added by the claim path but
  removed in exactly one place — implementation's `issue-close-out`. Curation's
  `--release` is deliberately token-free and never clears it, so any item curation
  claims but that does not carry through to a merged PR keeps `goobers:claimed`
  forever. Observed live: of 43 `goobers:claimed` labels, ~34 were leaked (label
  present, no ledger claim). The ledger stays correct; the visible backlog does not.
  (Tracked: #1003.)
- **No milestone/roadmap capability exists at all.** The providers layer parses
  milestones read-only; `UpdateWorkItemRequest` has no milestone field. Nothing in
  the CLI or provider surface can assign or remap a milestone.
- **Inert knobs.** The persona references `staleAfterDays` and stale-auto-close
  stage inputs that no code reads — staleness is entirely the model's 90-day
  eyeballing, unverifiable and unconfigurable in practice.
- **Curation is nearly unobservable.** A run emits a scalar `summary` with counts
  in a comment; there is no durable, queryable record of what curation did, so
  "is the pipeline healthy / is curation effective" cannot be answered.

None of this is a criticism of the persona spine — it is the set of things a
**persona alone structurally cannot do**: reconcile state it isn't handed, act on
items outside its claimed batch, write a capability the provider lacks, or produce
a verifiable/queryable record.

## 3. Principles

1. **Ground truth is the ledger, the journal, and the forge — never a label.**
   Labels are a mirror; the engine reconciles the mirror to reality, it never
   trusts the mirror as the source. (This is why #1003 is a *symptom*, and §5.B is
   the general fix.)
2. **Deterministic where verifiable; persona for judgment.** Move mechanical,
   checkable work (label reconciliation, staleness thresholds, duplicate-candidate
   surfacing, milestone assignment rules) into deterministic code or structured
   pre-passes, and reserve the agentic curator for genuine judgment (is this really
   a dup? is this really obsolete? is this really ready?). This makes curation
   testable and its failures legible.
3. **Continuous, not one-shot.** The engine keeps re-sweeping the whole backlog —
   not only uncurated items — to keep state true as the world changes.
4. **Every mutation is explained and idempotent.** Keep the curator's existing
   contract: a plain-language comment on every change, and re-running over an
   already-correct backlog is a no-op.
5. **Bias to `ready`, conservative on the irreversible.** Keep the calibrated
   defaults: decisive on resolvable cases (satisfied gates, additive contracts,
   implementer latitude, clear dedupe), conservative on genuine human-decision and
   destructive cases.
6. **The engine is measurable.** Curation actions and pipeline health are durable,
   queryable telemetry — curation that cannot be measured cannot be trusted to run
   unattended.

## 4. What "shovel-ready" must mean

`goobers:ready` today asserts "deduped, tagged, scoped to a single change `make ci`
can validate." The engine tightens the guarantee: a `ready` item should also be
**still true** (its gates still satisfied, not obsolete, not a now-known-duplicate)
and **still buildable** (not chronically failing implementation for a reason
curation should have caught). Ready is not a write-once stamp; it is an invariant
the engine continuously maintains.

## 5. The five capabilities

### A. Continuous re-curation
Extend selection and the persona so curation periodically re-examines already-`ready`
(and, read-only, in-flight) items on a bounded re-sweep — separate from, and
non-starving to, the forward `approved → ready` path. On re-visit it checks for the
drift the forward pass can't see: a gate that has since merged/closed, a goal made
obsolete by landed main, a now-obvious duplicate, or scope that grew. Forward
curation stays first-priority; re-curation consumes leftover budget.

### B. Label & state reconciliation
A deterministic reconciliation sweep that makes the visible backlog match ground
truth: clear `goobers:claimed` with no backing ledger lease (subsumes #1003 as its
first and simplest case), retire orphaned `tracking`/`stale` labels whose condition
no longer holds, and flag label contradictions (e.g. `ready` + `needs-human`
together). Complements the broader `goobers doctor/reconcile` state-drift work
(#522) but is scoped to **backlog issue metadata**, and it self-heals continuously
rather than requiring a human to run a command.

### C. Milestone & roadmap mapping
This is net-new capability, in two layers:
- **Provider foundation** — add milestone assignment to the providers surface
  (`UpdateWorkItemRequest` milestone field + implementation + capability) and a CLI
  path, since none exists today.
- **Curator roadmap mapping** — the curator assigns and remaps items into
  milestones as part of curation (grouping related work the agile way), and keeps
  epic/tracking checklists current as children land. Depends on the provider layer.

### D. Pipeline health & shovel-ready guarantee
Two feedback mechanisms so the engine actively guarantees a supply of good work:
- **Ready-pool health signal** — durable telemetry on the depth *and quality* of
  the `ready` pool (how many ready, how long they age before being claimed, how
  often a `ready` item bounces back), surfaced so a starving or low-quality pipeline
  is visible and can trigger curation attention.
- **Implementation-outcome feedback** — an item that repeatedly fails or escalates
  in implementation (beyond a threshold) is automatically routed back to curation
  for re-scoping / `needs-human`, instead of being retried blindly. This closes the
  loop between the implementation and curation workflows and connects to the
  semantic-staleness work (#983) and escalation drainage.

### E. Engine reliability & verifiability
Make the persona's mechanical work deterministic and legible:
- **Wire the staleness knobs** (`staleAfterDays`, stale-auto-close) into real code
  with a deterministic staleness pre-pass, so staleness is configurable and testable
  rather than model-eyeballed. Feeds #983.
- **Deterministic dedupe candidate-surfacing** — a pre-pass that hands the curator
  structured likely-duplicate candidates (title/body similarity, shared refs) rather
  than relying on the model to find them unaided; the model still makes the final
  keep/close call.
- **Curation observability** — durable, queryable per-run action records (counts of
  ready / needs-human / closed / deduped / split / stale / reconciled), the data
  layer under §D's health signal.

## 6. Delivery order and issue map

Independently reviewable slices. Existing issues are folded in where they already
cover a slice; new `CURE-*` issues cover the whitespace the survey confirmed has no
existing issue.

| Slice | Issue | Depends on | Boundary |
|---|---|---|---|
| Label mirror leak (`goobers:claimed`) — the seed case of §B | **#1003** (exists, `ready`) | — | Go/workflow |
| Semantic/goal staleness vs landed main | **#983** (exists) | — | Workflow + persona |
| Selection priority beyond FIFO | **#509** (exists) | — | Design + Go |
| GitHub-native `blocked_by` before claim | **#751** (exists) | — | Go |
| CURE-1 · Continuous re-curation re-sweep | new | #509 | Go selection + persona |
| CURE-2 · Deterministic label & state reconciliation sweep | new | #1003 | Go |
| CURE-3 · Milestone write capability (provider + CLI) | new | — | Go provider surface |
| CURE-4 · Curator milestone/roadmap mapping | new | CURE-3 | Persona + workflow |
| CURE-5 · Deterministic staleness pass (wire the knobs) | new | — | Go + persona; feeds #983 |
| CURE-6 · Deterministic dedupe candidate-surfacing pre-pass | new | — | Go + persona |
| CURE-7 · Curation observability + ready-pool health signal | new | — | Telemetry |
| CURE-8 · Implementation-outcome feedback → re-curate | new | CURE-7 | Cross-workflow |

## 7. Non-goals

- **Deciding what enters the backlog.** Nomination (`work-nomination`) owns
  admitting new work and `goobers:approved`; curation only refines the approved set.
  The engine never grants `goobers:approved`.
- **Writing code or touching PRs.** The curator remains issues-only; PR-side
  disposition stays with merge-review / pr-remediation.
- **Replacing human product decisions.** Priority-to-do-at-all, breaking-contract,
  destructive-default, and product-policy calls stay `needs-human`.
- **A general cross-subsystem reconciler.** §B is scoped to backlog issue metadata;
  worktree/run/claim state drift stays with #522.
- **Untrusted-content trust changes.** SEC-047 (trust-label gating, treat item text
  as data) is unchanged.
