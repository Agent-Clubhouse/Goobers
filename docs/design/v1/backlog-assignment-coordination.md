# Design: Assignment-aware backlog coordination (COORD)

> Status: **draft — for review** · Area: `RUN` / `WF` / `SEC` · Milestone: **Backlog curation
> engine — continuous, reliable, agile-inspired** (epic #1224)
> References: mixed-mode epic **#804** / **#369** (actor-aware workflows — a different axis,
> see Terminology); UNOP-7 **#1295** / **#1779** / **#1780** (distinct daemon identity);
> credential-decoupling design `multi-gaggle-validation.md` §4 G6 (a related but separate
> "the provider model needs another dimension" gap, same 2026-07-27 review).
> Origin: a mixed-mode coordination gap — a repo worked by multiple humans and/or multiple
> Goober daemon instances has no way to divide backlog work; everyone sees the same eligible
> pool and could pick up the same item.
>
> Operator guide: [Coordinate a shared backlog with assignees](../../guides/assignment-aware-backlogs.md).

## Terminology (do not conflate with mixed-mode / #804)

- **Mixed-mode (#804/#369)** = a repo has **diverse authors** (goobers, humans, outside
  agents) and `merge-review`/`pr-remediation` must be actor-aware about *who opened a PR*.
  That's a different axis from this doc.
- **This doc (COORD)** = who a **backlog item is assigned to**, and whether an actor
  (human or goober instance) should even attempt it. A mixed-mode repo very plausibly wants
  COORD *and* #804 together, but a purely goober-authored, single-instance repo can also want
  COORD (e.g. two goober instances on different owners' forks of one project dividing a
  shared backlog) — they're orthogonal, not dependent.
- **"Self"** — the identity a running `implementation`/assignment-engine evaluates against:
  either a real human GitHub/ADO login, or a distinct per-goober-instance identity (a
  configured login string today; ideally UNOP-7's distinct bot/App identity once that lands
  — see G3).

## 1. The gap

Two independent facts, confirmed against the current code (2026-07-27 review):

1. **`Assignee` is already a real, provider-native field Goobers partially understands** —
   `providers.WorkItem.Assignee` (`providers/model.go:78`) is populated on read for both
   GitHub (`providers/github.go`, mapped from `issue.Assignees[0].Login`) and ADO
   (`providers/ado.go:1446`, `System.AssignedTo`), and `ListWorkItemsRequest.Assignee` already
   filters both providers' list calls. It's even settable **at creation**
   (`CreateWorkItemRequest.Assignee`, `providers/model.go:896`).
2. **Nothing in the scheduler ever looks at it, and nothing can change it after creation.**
   `cmd/goobers/backlogquery.go` has zero references to `Assignee` — eligibility today is
   purely label/field-predicate driven, exactly as if the field didn't exist.
   `UpdateWorkItemRequest` (`providers/model.go:753-765`) has no `Assignee` field at all —
   there is no way to (re)assign an already-filed issue through the provider abstraction.

So today: two humans, or two goober instances, working the same repo's backlog see and can
claim the exact same set of eligible items. The claim ledger
(`internal/localscheduler/claim.go`) prevents two *runs* from double-processing the *same*
item simultaneously — it is a dispatch-time exclusive lease, not a priori work division — but
says nothing about steering different actors toward different items in the first place, and a
human working the backlog by hand has no ledger to consult regardless.

## 2. Scope: three related, independently-shippable features

- **COORD-A — Assignment-aware dispatch.** `implementation` (and optionally
  `backlog-curation`) gains an opt-in mode where eligibility additionally requires
  `Assignee == self`. Off by default — a gaggle that never configures this behaves exactly as
  today, unassigned items included. Once a gaggle opts in, an unassigned item is not eligible
  for *anyone* running that gaggle — it waits to be assigned.
- **COORD-B — An assignment engine that actually assigns ("scrum-master").** A new canonical
  workflow (mirrors `work-nomination`/`backlog-curation`'s existing shape — periodic,
  goober-or-deterministic, dispatchable) that scans unassigned-but-otherwise-eligible items
  and assigns them per a configured strategy. V1 ships exactly two strategies, deliberately
  no more (see [[canonical-workflows-stay-simple]] precedent — don't over-scope the first
  cut):
  - **Constant-cap:** assign up to N open items to a named actor, stop once at cap.
  - **Round-robin / load-balance:** spread across a configured roster, weighted by each
    actor's current open-assigned count.
  Both write the real provider assignee field (needs G1 below). An unconfigured gaggle simply
  doesn't run this workflow — COORD-B is opt-in per gaggle, same as COORD-A.
- **COORD-C — needs-human attention routing (separate, as you flagged).** When
  `goobers:needs-human` is applied to an item, optionally auto-assign it to a configured
  human so it surfaces in their native GitHub/ADO "assigned to me" view, not just a label a
  human has to know to search for. Fully decoupled from COORD-A/B — a gaggle can adopt this
  alone, or not at all.

A one-gaggle instance that configures none of this is byte-for-byte unaffected — every piece
here is additive and opt-in.

## 3. Designs for the gaps

### G1 — Provider gap: assign/reassign an existing item (foundational, small)

`UpdateWorkItemRequest` needs an `Assignee *string` field (nil = leave unchanged, matching
the existing `*string`/`*int` optional-field convention already used for `Title`/`Body`/
`Milestone` in that struct). `providers/github.go` and `providers/ado.go` implement it:

- GitHub: `PATCH /issues/{id}` with `assignees: [name]` (clearing: `assignees: []`).
- ADO: `PATCH` work item with a `System.AssignedTo` op (clearing: `null`/remove op — ADO's
  exact clear-assignee semantics need confirming against a live work item during
  implementation, not assumed from docs).

No scheduler/eligibility behavior changes here — this is pure provider-capability plumbing
that G2/G4 build on. Fully additive: a new optional field on an existing request type.

### G2 — `backlog-query` opt-in assignment-aware mode

New declared inputs on the `backlog-query` stage (mirrors how `trustLabel`/`requireLabels`/
`excludeLabels` already work): `assignedTo` (the "self" identity to filter on) and an explicit
opt-in flag (e.g. `respectAssignee: "true"`). When set:

- `ListWorkItemsRequest.Assignee` is set to the configured "self" value.
- An item with no assignee, or a different assignee, is not eligible — same posture as any
  other required-label check already in that stage, fail-closed by omission rather than
  silently falling through to today's behavior.
- When unset (the default), behavior is **byte-identical to today** — this is the backward
  compatibility guarantee the whole design depends on.

**Blast radius:** this touches the eligibility core every `implementation`/`backlog-curation`
run exercises, same class of stage as MGV-13-15. Even though the *default* behavior is
unchanged, the *opted-in* path is new dispatch logic in a high-consequence stage. Supervised,
not auto-approved — same posture, same reasoning.

### G3 — "Self" identity configuration

A gaggle (or the instance, as a default) declares its own "self" identity — a login string —
used by G2's `assignedTo` and by COORD-B/C when assigning *to* a goober instance rather than a
human. This is deliberately just a configured string in V1, not a new identity primitive: if
UNOP-7 lands a real distinct bot/App identity for a gaggle, that identity's login is exactly
what gets configured here — G3 doesn't need to wait on UNOP-7 to be useful (a shared PAT's own
account name works as "self" today), but is strengthened once UNOP-7 ships. No new auth
mechanism, no new credential — purely a string used as a filter/write value.

The concrete fields are `instance.yaml`'s top-level `selfIdentity` default and
`Gaggle.spec.selfIdentity` override. The effective value becomes a `backlog-query` task's
`assignedTo` input only when that input is not explicitly declared; omitting both configuration
fields leaves the input absent and preserves the opt-out behavior. Once UNOP-7 provides a
distinct bot/App login, operators configure that login in the same field without any code or
authentication coupling.

### G4 — Backlog-assignment canonical workflow ("scrum-master")

A new canonical workflow, `backlog-assignment`, alongside the existing `work-nomination`/
`backlog-curation` shipped examples (`config-examples/gaggles/*/workflows/`). Periodic
(same trigger shape as `backlog-curation`). Scans items matching the gaggle's normal
trust/require/exclude labels (reuses G2's existing query shape) that are **unassigned**, and
assigns per the configured strategy:

- **Constant-cap**: `roster: [{assignee: "alice", maxOpen: 5}]` — assign to `alice` until she
  has 5 open-assigned items in this gaggle's backlog, then stop (leaves remaining items
  unassigned rather than erroring — a human or a later tick handles the overflow).
- **Round-robin / load-balance**: `roster: [{assignee: "alice"}, {assignee: "bob"}]` —
  assign the next unassigned item to whichever roster member currently has the fewest
  open-assigned items (ties broken by roster order, deterministic).

Writes via G1's `UpdateWorkItem` with `Assignee` set. **Deliberately no declarative
constraint DSL, no per-item routing rules, no priority weighting beyond current-open-count**
in V1 — two strategies, both simple, matching this codebase's own established
"canonical workflows stay simple" correction from a prior review. A more expressive strategy
is a V2 concern if the simple ones prove insufficient in practice.

**Blast radius:** this is a new workflow that **mutates** real GitHub/ADO issues (assigns
them) — a new class of daemon-initiated mutation, same category of concern TBH-1 exists for.
Supervised, not auto-approved, regardless of how simple the two strategies are.

### G5 — Needs-human attention routing (COORD-C)

When the `goobers:needs-human` label is applied to an item (any of the existing code paths
that currently do this), optionally also call `UpdateWorkItem` with a configured assignee (a
human, via G3-style configuration specific to this feature — e.g. `needsHumanAssignee:
"mason"`). Purely additive: if unconfigured, behavior is unchanged (label-only, exactly as
today). If configured, the human sees it natively assigned to them, not just label-searchable.

Does not depend on COORD-A/B/G2/G4 at all — only on G1 (the provider `UpdateWorkItem`
assignee capability). Genuinely separable, as flagged.

### G6 — Conformance coverage: prove the actual coordination claim

A fixture test (mirrors MGV-9/MGV-18's precedent) with two configured "self" identities and a
shared backlog: assert (a) an item assigned to identity A is never eligible when
`backlog-query` runs as identity B, (b) an unassigned item is eligible for neither once
`respectAssignee` is on, (c) the load-balance strategy converges to an even split across a
multi-item batch, (d) constant-cap stops exactly at the configured ceiling and leaves the
remainder unassigned rather than erroring.

## 4. Decomposition — dispatchable work items

| ID | Item | Depends on | Risk | Status |
|---|---|---|---|---|
| COORD-1 (#1819) | G1 — `UpdateWorkItemRequest.Assignee` + GitHub/ADO `UpdateWorkItem` support | — | Low (additive field + method impl) | **approvable** |
| COORD-2 (#1820) | G2 — `backlog-query` opt-in `respectAssignee`/`assignedTo` mode | COORD-1 (for reassignment paths; read/filter already exists) | **Med-High** (eligibility core, opt-in but new dispatch logic) | supervised — not auto-approved |
| COORD-3 (#1821) | G3 — configured "self" identity (gaggle/instance-level) | — | Low (a config string + plumbing) | **approvable** |
| COORD-4 (#1822) | G4 — `backlog-assignment` canonical workflow, constant-cap + round-robin strategies | COORD-1, COORD-2, COORD-3 | **High** (new mutation-writing workflow) | supervised — not auto-approved |
| COORD-5 (#1823) | G5 — needs-human → configured assignee routing | COORD-1 only | Low (additive, optional, independent of COORD-2/4) | **approvable** |
| COORD-6 (#1824) | G6 — conformance test: cross-identity exclusion + load-balance/cap correctness | COORD-1..4 | Low (test-only) | **approvable** once COORD-2/4 land |
| COORD-7 (#1825) | Docs — authoring guide for assignment-aware gaggles, worked multi-instance example | COORD-1..4 | Low (docs-only) | **approvable** once COORD-2/4 land |

## 5. Recommended sequencing

1. **Now (approvable, low-risk):** COORD-1 (provider plumbing) and COORD-3 (self-identity
   config) — independent, foundational, nothing else can build without COORD-1 specifically.
   COORD-5 (needs-human routing) can also land now — it only needs COORD-1.
2. **Supervised, PO review:** COORD-2 (opt-in dispatch filtering) — the actual eligibility
   change, even though default-off. Then COORD-4 (the assignment engine itself) once COORD-2
   exists to filter against.
3. **Once COORD-2/4 land:** COORD-6 (conformance proof) and COORD-7 (docs) — both approvable,
   sequenced last since they test/document the finished behavior.

## 6. Open questions (PO)

- **OQ-1 — single vs. multi-assignee:** V1 assumes single-assignee-only (matching
  `WorkItem.Assignee string`, not a list) to avoid "who's actually on point" ambiguity for
  both COORD-A eligibility and COORD-B's load-balance accounting. Confirm, or is native
  GitHub/ADO multi-assignee wanted from the start?
- **OQ-2 — unconfigured-roster failure mode:** if a gaggle enables `backlog-assignment`
  (G4) with an empty/misconfigured roster, should the workflow fail closed (error, no
  assignments made) or no-op quietly? *(Recommend: fail closed — a misconfigured assignment
  engine should be loud, not silently leave everything unassigned indefinitely.)*
- **OQ-3 — cross-gaggle roster reuse:** can one human/goober-instance identity appear in more
  than one gaggle's roster (e.g. a person working across two projects), or is a roster
  strictly per-gaggle? *(Recommend: per-gaggle for V1 — a cross-gaggle load-balance view is a
  real feature but a materially bigger one; revisit if needed.)*
- **OQ-4 — ADO assignee-clear semantics:** ADO's exact API shape for *removing* an assignment
  (vs. GitHub's `assignees: []`) needs confirming against a live work item during COORD-1's
  implementation — flagged here so it isn't assumed from documentation alone and quietly
  wrong (recurring gotcha class in this codebase's ADO work, see #772/#1751-adjacent
  findings).
