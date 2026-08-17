# Design: Multiple Goober instances coordinating on the same repo

> Status: **draft — for review** · Area: `RUN` / `WF` / `SEC` · Milestone: **V1 — multi-gaggle,
> teams, and repos you own**, forward-looking to **V2 — cloud scale & large teams**
> References: #1657 (the docs/design question this resolves); COORD design
> (`docs/design/v1/backlog-assignment-coordination.md`, #1818); per-gaggle
> `BranchNamespace` (#965/#1010/#1109); credential-decoupling design (G6,
> `docs/design/v1/multi-gaggle-validation.md`); Temporal runner epic (#39) and
> SCH-042 workflow-id exactly-once claiming.
> Origin: brownfield large-monorepo scenario (2026-07-29) — 10-20 teams each running
> Goobers locally against regions of a single many-million-line repo, expected to
> eventually consolidate onto a shared cloud-hosted instance, but starting and
> fit-assessing entirely local-first.

## 1. The motivating case

A many-million-line brownfield monorepo. 10-20 teams each want to run their own
Goobers instance, from their own local machine, against their own region of the
repo (e.g. `/frontend`, `/services/billing`, `/infra`) — before anyone commits to
a shared, centrally-hosted instance. Early stage is explicitly **local-only, fit
assessment** — teams need to be able to try this without waiting on a shared
cloud deployment. If it works, some or all of them may eventually lift onto a
shared tier-3 (cloud) instance.

Hard requirement, stated plainly: **these instances must not fight each other in
PRs, and must not duplicate work or claims** — on **both** GitHub and ADO.

## 2. What already exists — a real survey, not a rebuild

This is not greenfield. Confirmed against current code (2026-07-29):

- **Per-gaggle `BranchNamespace`** (#965/#1010/#1109) is a real, already-shipped,
  **structural** partition for the PR-lifecycle surface. `pr-select`,
  `gather-sibling-context`, `apply-verdict`, `elect-lander`, merge/post-merge,
  `issue-close-out`, and the mirror-fetch prune exclusion all filter by
  `providerBranchNamespace()`. Two instances configured with **distinct**
  namespaces structurally never see, review, or merge each other's PRs — not a
  soft preference, a hard filter on branch-head prefix. It's opt-in (defaults to
  `"goobers/"` for everyone) and provider-agnostic (branch-prefix matching, not a
  GitHub- or ADO-specific mechanism).
- **`backlog-query`'s label filters** (`trustLabel`/`requireLabels`/
  `excludeLabels`) already let a gaggle require a specific label to be eligible.
  Nothing stops two teams from each requiring their own disjoint ownership label
  (e.g. `area:frontend` vs. `area:billing`) today — this is available, just not
  documented as a multi-instance pattern.
- **Contested-file dispatch awareness** (#1085) already deprioritizes claiming an
  issue whose referenced files are contested by open PRs, so `implementation`
  doesn't pile new work into an overlap cluster faster than review can drain it —
  soft, best-effort, reorders candidates but never drops one.
- **COORD** (#1818, partially shipped) adds a configured "self" identity and an
  opt-in assignee-based eligibility filter for `backlog-query` — a second,
  independent partitioning axis (who an issue is assigned to, vs. what label it
  carries).
- **The claim ledger is per-instance-local.** `internal/localscheduler/claim.go`'s
  `ClaimKey` lives in each instance's own `SchedulerDir()` — two instances never
  share it. The only cross-instance signal is the **provider-visible
  `goobers:claimed` label/assignee mirror**, and that mirror is explicitly
  documented in its own code as non-authoritative — a reflection for human
  visibility, not claim truth. Label/assignee mutation on GitHub or ADO is not an
  atomic compare-and-set. **This is the real, unresolved gap**: two instances can
  both observe an item unclaimed, both write the claim marker, and both proceed —
  a genuine race, not a hypothetical one.
- **Tier-3 (Temporal, #39/#41) already solves this correctly, for a different
  deployment shape.** Temporal workflow-ID uniqueness is a real atomic claim
  primitive — starting a workflow under a deterministic ID derived from
  repo+issue is atomic, and a duplicate start attempt fails cleanly (SCH-042).
  This is the contrast #1657 already flags: distributed workers on a shared
  Temporal deployment get a hard guarantee; independent local daemons do not.

## 3. The actual gap

Region/path ownership within **one** repo is not a first-class concept —
teams can approximate it today with disjoint required labels, but nothing
validates that two independently-configured instances' label scopes are
actually disjoint, and nothing hardens the underlying claim race beyond the
non-atomic label/assignee mirror. Two teams' gaggle configs are independently
authored (often on different machines, by different people) — there is no
built-in cross-check that they don't quietly overlap.

## 4. Recommended shape: two phases, matching the deployment arc the request itself describes

### Phase 1 — local, tiers 1-2: soft partitioning by design, hardened where cheap, honest about residual risk

The right posture for the local/fit-assessment phase is **primarily prevent
overlap by construction** (so the race window is never entered), not lean on a
mutex that doesn't exist yet:

- **Region ownership via disjoint required labels** is the primary mechanism —
  reuses existing `backlog-query` label filters, provider-neutral (GitHub and
  ADO both support required labels the same way), zero new schema needed to use
  it today. Document this as the recommended pattern, not a incidental
  possibility.
- **Distinct `BranchNamespace` per instance** for the PR-lifecycle side — already
  shipped, needs to become an explicit, paired recommendation rather than an
  unrelated knob operators might not think to combine with label partitioning.
- **COORD's assignee-based filtering** as a complementary, not competing, second
  partition axis — useful when a region has enough churn that label-only
  partitioning isn't granular enough (e.g. one team's region has issues that
  individual members want routed to themselves specifically).

### Configuration granularity — where each mechanism actually lives

Confirmed inconsistency worth fixing rather than leaving implicit: `BranchNamespace`
already has a clean **gaggle-level default, per-task override** pattern (every
PR-selecting stage falls back to the gaggle's configured namespace unless its own
task declares a different `headPrefix`). Region labels have **no such default
today** — `requireLabels`/`excludeLabels`/`respectAssignee` are declared
independently on each workflow's own `backlog-query` task
(`implementation.yaml` and `backlog-curation.yaml` each set their own). For a
10-20-team deployment that means repeating the same required label in every one
of a team's workflow files, with real drift risk (a new workflow added later
forgets the label and silently rejoins the shared pool).

**Region-label scoping should get the same shape `BranchNamespace` already has**:
a gaggle-level default required label, inherited by every workflow's
`backlog-query` task unless a task explicitly overrides it — not a
workflow-only concept. This is MIRC-2's job, not a separate issue: sibling-scope
declaration and the gaggle-level region-label default are the same piece of
config surface (a gaggle's own declared scope *is* what gets checked for overlap
against its declared siblings).

Claim-race detection (MIRC-3) is deliberately **not** a per-workflow opt-in —
it's a pure safety hardening in the shared claim path (`backlog-query --claim`)
with no legitimate reason to disable, so it applies unconditionally to any
workflow that claims items, once it ships.

### Sibling identity — what actually makes two gaggles "the same coordination domain"

A gaggle's name (`GaggleSpec.DisplayName`) is **purely local** — `ConfigSet.Gaggles`
is a plain slice loaded from one instance's own config directory, not a shared
registry or namespace. Confirmed: nothing in the system today compares gaggle
names across two different instance *processes*, because no instance has any
way to know another instance exists. Two unrelated teams both naming a gaggle
`"backend"` in their own separate `instance.yaml` is pure coincidence and
carries **zero** weight — the string match means nothing.

So sibling matching in MIRC-2 **must not key on gaggle name**. The thing that
actually identifies "these two gaggles are operating on the same coordination
domain" is `GaggleSpec.Project` — the target repo (provider + owner + name).
Two gaggles are only meaningfully siblings if they target the same repo; a
sibling declaration should describe *what repo the other gaggle targets and
what its effective scope is* (required label(s), branch namespace), with the
other side's gaggle/instance name carried along only as a human-readable label
for the warning message — never as the match key.

- **New: sibling-overlap validation + gaggle-level region-label default.** A
  gaggle declares its own default required label (inherited by its workflows'
  `backlog-query` tasks unless overridden) and can optionally declare known
  "siblings" — identified by **target repo** (provider/owner/name), not gaggle
  name — and their scopes; `goobers validate`/`lint` warns when two declared
  siblings targeting the **same repo** have effective required-label sets that
  are not disjoint. Catches the misconfiguration case (the likely dominant
  failure mode with 10-20 independently-configured teams) before it produces a
  live collision, without requiring a shared coordination service.
- **New: claim-race detection, not elimination.** Harden the claim path with a
  write-then-reread pattern: after writing the claim marker, re-fetch the item
  shortly after and confirm the marker still names this instance's claim. If a
  second instance's claim overwrote it in the interim, the loser detects the
  collision and backs off (releases its own claim, does not proceed to
  implement) instead of silently duplicating work. This does not make the
  operation atomic — a true TOCTOU window remains between the initial read and
  the write — but it converts "both instances silently duplicate the work and
  both open PRs" into "one instance detects it lost the race and stands down,"
  which is the actual failure mode #1657 asks to be enumerated and mitigated,
  not eliminated outright.
- **State this plainly, not silently:** Phase 1 has a residual, non-zero race
  window. It is not a hard guarantee. Teams whose regions are genuinely disjoint
  by design essentially never hit it (the race only matters for overlapping or
  misconfigured scopes); the sibling-validation and claim-detection hardening
  above are defense in depth, not a substitute for correct partitioning.

### Phase 2 — shared/cloud: the real hard guarantee, once instances share a substrate

Once teams lift onto a shared tier-3 deployment (or any shared coordination
substrate), a genuine atomic claim becomes available and should replace, not
supplement, the soft mechanisms above for that deployment:

- **Temporal workflow-ID claiming** (already the design for tier-3, #39/SCH-042)
  is the natural answer if/when the Temporal runner lands — deterministic
  workflow IDs derived from `(repo, issue)` give a real compare-and-set via
  Temporal's own start semantics.
- **Lighter-weight alternative, worth leaving as an open question**: a shared
  claim store (even something as simple as one shared database table reachable
  by every instance) could deliver the same atomic guarantee sooner than a full
  Temporal migration, for teams who want the hard guarantee before tier-3 is
  ready. Not designed here — flagged as a real fork in the road (see Open
  Questions).

## 5. GitHub / ADO parity

Everything Phase 1 depends on is already symmetric across both providers:
required-label filtering, assignee (COORD's `providers.WorkItem.Assignee` is
already read/filter/create-plumbed for both GitHub and ADO per the COORD
design's own research), and `BranchNamespace`'s branch-prefix matching (a
git-branch-name mechanism, not provider-specific at all). No provider-specific
design decisions are needed for Phase 1; Phase 2's shared-claim-store option
would need equivalent treatment for both if that path is chosen, since Temporal
workflow-ID claiming is itself provider-agnostic (it keys on `repo+issue`
identity, not on which provider hosts the issue).

## 6. Decomposition — dispatchable work items

| ID | Item | Risk | Status |
|---|---|---|---|
| MIRC-1 (#1900) | Authoring guide: combine disjoint required-label partitioning + distinct `BranchNamespace` as the recommended local multi-instance pattern, with the brownfield 10-20-team monorepo as the worked example. Resolves #1657's documentation ask directly. Shipped: [`docs/guides/multiple-instances-one-repo.md`](../../guides/multiple-instances-one-repo.md). | Low (docs-only) | **shipped** |
| MIRC-2 (#1901) | Sibling-scope declaration + `goobers validate`/`lint` overlap warning: a gaggle can declare known sibling instances/gaggles and their label scopes; validation warns (does not fail closed) on non-disjoint required-label sets between declared siblings. | Low-Med (additive, new optional config surface, warn-only) | **shipped**: `GaggleSpec.RequireLabels`/`Siblings` (`api/v1alpha1/gaggle_types.go`), `checkGaggleSiblingLabelOverlap` (`api/validate/validate.go`, SIB001) |
| MIRC-3 (#1902) | Claim-race detection: write-then-reread hardening in the claim path (`backlog-query --claim`) — detect a lost race against another instance's claim and back off instead of proceeding. | Med (touches the claim path directly) | supervised — not auto-approved |
| MIRC-4 (#1903) | Conformance test: two independently-configured instances (disjoint labels + distinct namespaces) against a shared fixture repo never claim the same item; a second fixture with a *deliberately* overlapping scope proves MIRC-3's detection actually fires and the loser backs off rather than duplicating a PR. | Low (test-only) | **approvable** once MIRC-3 lands |
| MIRC-5 (#1904) | Phase-2 stub: shared-claim-substrate or Temporal-workflow-ID cross-instance exactly-once claim, for teams who lift onto a shared deployment before full tier-3 Temporal migration is ready. Genuinely unscoped — the "lighter-weight alternative" fork in the road from §4 needs a real decision before this becomes implementable. | — | **Future milestone, not approved** |
| MIRC-6 (#1905) | ADO-specific parity verification for MIRC-1/2/3's mechanisms (required labels, assignee, branch-prefix matching) — confirm no ADO-specific gap once the above land, since this design asserts symmetry but the actual verification is implementation-time work. | Low (verification, not new design) | **approvable** once MIRC-1-3 land |

## 7. Open questions (PO)

- **OQ-1 — Phase 2 substrate:** for a team wanting the hard cross-instance
  guarantee before a full Temporal/tier-3 migration, is a lightweight shared
  claim store (e.g. one shared database table) worth designing as an
  intermediate step, or should Phase 2 always mean "lift onto tier-3," full
  stop? *(No recommendation offered here — this is a real fork depending on how
  much appetite there is for a second coordination substrate alongside the
  planned Temporal one.)*
- **OQ-2 — sibling declaration shape:** should sibling-scope declaration (MIRC-2)
  live in each instance's own config (each gaggle lists its known siblings,
  possibly drifting out of sync), or does it want a shared, centrally-maintained
  registry even in the local-only phase (e.g. a config-repo-hosted manifest all
  instances read)? *(Leans toward per-instance declaration for Phase 1 — it
  needs no shared infrastructure — but flagging since a shared registry would
  also make MIRC-2's overlap check more reliable than each instance's own
  possibly-stale view of its siblings.)*
- **OQ-3 — claim-race backoff behavior (resolved 2026-08-16):** automatically
  delete/close artifacts belonging to the losing claimant. MIRC-3 enforces this
  before artifacts can escape: `backlog-query` confirms provider ownership
  before committing its result, and a mismatch releases the local lease and
  returns no-work. Because this is the workflow's first stage, no downstream
  branch push or PR creation can occur; normal terminal workspace cleanup
  removes the unpushed run branch.
