# [EPIC] Azure DevOps merge-review, lifecycle-close, and identity parity

Suggested labels: area:providers, area:ado, type:epic, goobers, customer-blocking

## Problem

A prospective customer running Azure DevOps can today have Goobers claim
work items, implement, open a PR, publish a native PR status, and poll the
pipeline — the implementation loop is wire-proven end to end (testbed PR
359, work item 1456, 2026-08-09). What they **cannot** do is land the work:
no stage of the merge-review chain is wired for ADO, the work item never
closes after merge, and assignee-based routing silently misbehaves. Every
provider-side landing primitive exists and is declared conformant — the gap
is entirely in the stage layer and three identity/lifecycle defects. This
is the difference between "ADO demo" and "ADO in production."

## Evidence

Full audit: `docs/audits/2026-08-08-gaggle-reliability/ado-conformance/`
(`code-truth.md` = file:line, `wire-truth.md` = live REST JSON).

- Merge chain GitHub-hardwired: `mergepr.go`, `prselect.go`,
  `gatherprcontext.go`, `mergequeuepoll.go`, `postmerge.go`,
  `prremediationlifecycle.go` — none dispatch by provider the way
  `issuecloseout.go:242-276` does. Provider primitives that go unused:
  `providers/ado_landing.go` `MergePullRequest`/`EnqueuePullRequest`/
  `PollMergeQueueEntry`/`DetectMergePolicy` (CONF-3 #2076, all declared).
- Work item never closes after merge: `postmerge.go:217-228` is
  GitHub-only; wire-confirmed WI 1456 sits `System.State: New` while
  "closed out" — the whole lifecycle lives in tags, not state.
- Assignee routing broken: `respectAssignee` compares
  `System.AssignedTo` displayName vs a GitHub-login-shaped string
  (`backlogquery.go:287-301`, `ado_workitems.go:850`); server WIQL and
  client re-verify can disagree and drop everything.
- Verdict-label wrong-object hazard: label application reuses the
  PR-number-as-issue trick (`applyverdict.go:789-799`), safe only because
  GitHub-gated — on ADO `UpdateWorkItem(prNumber)` mutates the work item
  with that ID.
- No native PR↔work-item link: `open-pr` writes `Fixes #N` prose, not
  ADO's `AB#N` grammar, and no ArtifactLink (`ado_pullrequests.go:58-73`);
  `completionOptions.transitionWorkItems` absent from `adoCompletionOptions`
  (`ado_landing.go:334-338`) — ADO's own auto-transition machinery has
  nothing to act on and is never requested.
- No authenticated-identity primitive: no `AuthenticatedLogin`/connectionData
  equivalent for ADO — blocks trusted-comment filtering and self-review
  vote, and leaves a claim-trust question open.
- No policy-read 403 degrade: `DetectMergePolicy` on ADO has no analogue to
  GitHub's new entitlement-403→Direct fallback (`github.go:1076-1088`) — a
  PAT without policy-read fails detection outright.
- Seeding: no ADO `EnsureWorkItemLabels` (`github.go:2705` GitHub-only) — no
  supported way to bootstrap the tag vocabulary (why `connect --seed`
  can't seed ADO).

## Proposed direction (sub-issues)

1. **Stage dispatch + capability plumbing** — per-provider switches in the
   six merge-chain commands, resolving through `adoauth.Provider`
   (pat/azure-cli/workload/managed) instead of literal `github:pr:*` grants.
2. **Verdict transport** — recommend option (c): `report-pr-status`
   publishes the verdict as PR status `goobers/validation`, gated by a
   status-check branch policy; merge-pr's verdict conjunct reads policy
   evaluations. (Alternatives: ADO reviewer vote — ADO permits self-vote,
   unlike GitHub #870; or PR-thread handoff.)
3. **Lifecycle close** — ADO `post-merge` sets the work item to a
   Completed-category state and requests `transitionWorkItems`; `open-pr`
   emits `AB#N` linkage so the native machinery works.
4. **Identity parity** — `AuthenticatedLogin`/connectionData for ADO;
   `respectAssignee` and needs-human routing compare on the ADO identity
   descriptor, not a login string; forbid the PR-as-work-item label trick
   on ADO.
5. **Robustness** — policy-read 403→Direct degrade; `deleteSourceBranch`
   on ADO completion; ADO `EnsureWorkItemLabels` (enables `--seed`).

Zero-config: none of this changes GitHub behavior; ADO gaggles gain a
working merge path where today they fail closed.

## Also here: fixed en route (not part of the epic)

The claim-marker mistranslation (park/close removed `goobers/status:claimed`
while the claim writes plain `goobers:claimed`, leaking a permanent tag on
every ADO claim — wire-confirmed WI 1456) is already fixed on this branch.

## Duplicate search

2026-08-09: BL-033 (curation/reconcile on ADO), the transitions-V1 note,
CONF-3 #2076 (landing primitives, merged). None covers the stage-layer
wiring, lifecycle close, or the assignee/identity defects. File as an epic
under the ADO conformance milestone; re-search at filing time.

## Size and risk

L (epic). Additive per provider; the wire evidence and provider primitives
de-risk it — this is wiring and identity-shape correctness, not new
protocol work.
