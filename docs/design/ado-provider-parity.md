# Azure DevOps Provider Parity — the PR lifecycle on ADO

> Status: **draft — for review.**
> Driving epic: #2061 (ADO end-to-end). Builds on `docs/design/provider-contract-conformance.md`
> (the capability model) and `docs/design/v0/pr-lifecycle-loop.md` (the stage contract).

## 1. Context

Goobers abstracts forges behind `providers.Provider` (`RepoProvider` + `BacklogProvider`
+ `TriggerProvider`) with optional capability interfaces so a forge-specific feature
"does not widen every backend." The provider-contract-conformance design established the
capability model — declared, not probed — and drove the ADO landing surfaces
(`DetectMergePolicy` / `EnqueuePullRequest` / `PollMergeQueueEntry` / merge / compare /
branch-delete) to implementation.

This document specifies the layer above that: how the **provider-neutral PR-lifecycle
stages run end-to-end on Azure DevOps**. The lifecycle is three lanes:

1. **implementation** — claim a work item, implement, and end at `open-pr`;
2. **merge-review** — `pr-select` → `check-issue-staleness` → `gather-sibling-context` →
   review gate → `apply-verdict` → gates → `merge-pr` → `queue-watch` → `post-merge`;
3. **pr-remediation** — `gather-pr-context` → rework → `push-remediated` /
   `remediation-checkpoint` / `rebase-pr`, with `pr-claim` guarding the loop boundary.

ADO differs from GitHub in three ways that shape the whole design: it has **no PR-comment
API** (only PR *threads*), its **PR labels** have API quirks invisible to fixture tests,
and its **identity model** is GUID/UPN/displayName rather than a single login string. The
architecture below routes each stage onto ADO-native surfaces while keeping every GitHub
code path byte-identical.

## 2. Design invariants

- **GitHub stays byte-identical.** Every ADO behavior is a *new* branch reached only when
  `repo.Provider == providers.ProviderADO`, mirroring the per-provider dispatch template
  `issue-close-out` established. On GitHub the ADO branch is unreachable; on ADO the GitHub
  helpers are gated off.
- **Capability isolation is preserved on ADO.** Merge/completion authority rides on a
  dedicated capability, `ado:pr:complete` (`capability.ADOPRComplete`) — the ADO
  counterpart to `github:pr:merge`. It is resolved fail-closed *before* the completion-
  authorized provider is constructed, so a stage carrying only `ado:pr:write` can never
  silently acquire completion authority (decider ≠ executor).
- **Mandatory methods flow through the Dispatcher; ADO-only surfaces are called
  directly.** Poll / list / compare / detect-policy / enqueue / merge all run through the
  provider-neutral `Dispatcher`, so both providers share one landing code path. The
  ADO-only transport surfaces — PR threads, PR labels, `AuthenticatedLogin`,
  single-PR `GetPullRequest` — are plain `*ADOProvider` methods the ADO branches call
  directly.
- **Never write a PR number through the work-item API.** On ADO `wit/workitems/{id}`
  addresses the *work item* whose numeric id equals the PR number. Every GitHub site that
  uses the "PR-as-issue" label trick (`UpdateWorkItem(ID: PR#, …)`) would mutate an
  unrelated work item and must stay gated off on ADO. All PR-scoped state lives on
  PR-native surfaces (labels, threads, status).

## 3. Provider construction and auth

ADO stage branches build the provider with `newADOProviderForStage(root, repo)`, which
resolves the configured auth source — PAT, Azure CLI, workload identity, or managed
identity — from the instance config, org-scoped. No `github:*` token is resolved on an ADO
branch. Work-item reads/writes route through the backlog project reference
(`backlogRepoRefForStage`) so a split code-repo/backlog-project instance addresses the
right project; PR-scoped calls use the routed code repo.

## 4. Transport: threads and labels

### 4.1 No PR comments → PR threads

ADO has no equivalent of GitHub's issue/PR-comment API, so the merge-review verdict, the
finding-set history, and the sticky remediation-state comment (carrying the pre-remediation
head SHA) all ride on **PR threads**. Three primitives form the carrier:

| Primitive | Role |
|---|---|
| `PostPullRequestThreadComment` | Open a new thread with one top-level comment — the ADO analog of posting a PR comment; the keystone of the verdict / finding / head-SHA handoff. |
| `ListPullRequestThreadComments` | Return every author-written comment across a PR's threads, oldest first. System threads (vote/status/ref events ADO synthesizes, `commentType: system`) are skipped so only real comments reach the stage layer. |
| `UpdatePullRequestThreadComment` | Edit a sticky comment in place — the ADO analog of updating a PR comment. |

The returned `Comment.ID` is the composite `"<pullID>/<threadId>/<commentId>"`. ADO's
edit/delete thread-comment endpoints need all three path segments (unlike GitHub's
repo-wide comment ids), so encoding them into the opaque id lets a later update address the
exact comment with no extra state.

Thread authors render as **displayName**, and `AuthenticatedLogin` (below) returns
displayName, so a trusted-author filter recognizes a thread the runner itself posted. (PR
*authors* and *reviewers*, by contrast, key on UPN — a known ADO identity inconsistency;
the verdict/finding transport deliberately lives on the thread surface, where displayName
is consistent end to end.)

`AuthenticatedLogin` is implemented via the ADO `connectionData` endpoint (ADO had no
authenticated-identity read before). It underpins the trusted-comment filter the
merge-review verdict trust check needs, and closes the claim-spoof gap.

### 4.2 Native PR labels carry the routing signals

ADO PRs support native labels. Two routing markers ride there — and only there:

- `goobers:needs-remediation` — route the PR into the remediation loop;
- `goobers:merge-escalated` — the PR is parked for a human, out of the automated loop.

`ListPullRequests` already surfaces PR labels, so the existing remediation selector
(`remediationPriorityFor`) fires on them unmodified — no work-item write, no wrong-object
hazard. Writes go through `AddPullRequestLabels` (ADO takes one label per POST, so one call
per name), reads through `PullRequestLabelNames`, and clears through
`RemovePullRequestLabel`.

### 4.3 The three ADO PR-label API quirks

ADO's PR-label API has three behaviors a fixture/fake-server test cannot observe. All three
break the needs-changes → remediation loop, and each is now pinned by conformance coverage
after being verified against a live ADO organization:

| # | Quirk | Symptom if unhandled | Handling |
|---|---|---|---|
| 1 | **List omits labels unless asked.** `ListPullRequests` must send the top-level `includeLabels=true` query parameter. `$expand=labels` and `searchCriteria` variants do nothing. | `pr.Labels` is always empty → the remediation selector matches nothing → the loop never fires. | Always send `includeLabels=true` on the list call. |
| 2 | **Delete-by-name 400s on a colon.** `DELETE …/labels/<name>` returns HTTP 400 when the name contains a colon (e.g. `goobers:needs-remediation`). | The marker never clears → a reworked PR is stuck in needs-remediation forever. | Resolve the label id via the `/labels` sub-endpoint and `DELETE …/labels/<id>` (GUID) — accepted. Absence is benign (mirror GitHub's 404-is-not-an-error removal). |
| 3 | **Single-PR GET carries no labels.** The PR-object GET returns `labels: []` even with `$expand=labels`. | Any label read off a single-PR GET sees nothing. | Read labels only from the dedicated `GET …/pullrequests/<id>/labels` sub-endpoint. |

A fourth, related wire fact: the PR-labels endpoint is published only under a `-preview`
api-version; a plain `7.1` is rejected. The label calls pin the preview version explicitly.

The rule of thumb for any ADO label/state read: prefer the **list** path with
`includeLabels=true` or the dedicated `/labels` sub-endpoint; never trust labels off a
single-PR GET; delete labels by **id**, not name.

## 5. `open-pr` on ADO

`open-pr` is idempotent find-or-create. On ADO the PR body carries a `Fixes #<workItemId>`
closing reference, where the number is the work-item id. This closing reference — read back
out of the body downstream — is the **durable PR↔work-item link** the rest of the lifecycle
relies on (staleness re-check, and the work-item close at post-merge). The body is length-
capped for the ADO create endpoint. The lifecycle deliberately does not depend on a native
ADO ArtifactLink between the PR and the work item; the closing reference in the body is the
single source of that linkage.

## 6. merge-review on ADO

### 6.1 `pr-select` — FIFO-only selection

The ADO branch constructs the ADO provider, wraps it in the `Dispatcher`, and reads only
**mandatory** `Provider` methods (`ListPullRequests`, `PollPullRequest`) — no optional
landing capability is probed during selection. ADO has no `RefCheckState`/`RefCheckStates`
and no cheap single-PR check-state read, so each candidate's `CheckState` — and its
open/merged `State`, which ADO's list response leaves empty — is resolved from
`PollPullRequest`'s branch-policy evaluations. The webhook-targeted PR is likewise resolved
via `PollPullRequest`.

Selection is **FIFO-only** on ADO. The sibling / foundation-coupling scan, the
per-candidate escalation/demotion gates, and the Tutor classification are all gated off:
there is no sibling election on ADO, so nothing parks a sibling and no dependent is
aging-boosted. Eligibility is the shared filter (open, correct base, not draft, check state
passing, no exclusion label); the shared fairness/aging ranker then orders the eligible set,
and the claim ledger remains authoritative for exactly-once selection.

**Identity and the advisory-mode dependency.** ADO has no login-string identity, so the
expected-author login is empty and `isOwnPullRequest` falls to the branch-prefix heuristic
against the gaggle's head prefixes. This creates the lane's subtlest correctness
dependency: **the ADO run-branch namespace must appear in the gaggle's head prefixes**,
otherwise a goobers-authored ADO PR is misclassified as third-party, enters advisory mode,
and never merges — even though every provider call succeeds.

### 6.2 `apply-verdict` — escalate-and-park routing

ADO has neither a native changes-requested review to submit nor GitHub's sticky-comment /
label verdict transport, and the GitHub path's PR-number label write would hit the wrong
work item on ADO. The verdict therefore rides three PR-native surfaces, each the ADO analog
of a GitHub handoff channel:

1. **A `goobers/validation` PR status** — the same surface `report-pr-status` publishes. A
   status-check branch policy gates the merge on it. **Pass → `succeeded`; both
   needs-changes and fail → `failed`** (the PR must not land until reworked, and a status
   genre cannot carry the needs-changes/fail split — the label below is the routing signal).
2. **The routing label, by decision**, mirroring the GitHub `verdictLabel` contract:
   - **fail →** add `goobers:merge-escalated`, clear `goobers:needs-remediation`. An
     escalation is *never* burned on the remediation budget; clearing needs-remediation and
     escalating parks the PR for a human instead of looping it forever.
   - **needs-changes →** add `goobers:needs-remediation` — **unless the PR already carries
     an active escalation**, in which case clear any stale needs-remediation and keep the PR
     parked (verdict-side escalation suppression, so a re-review cannot pull an escalated PR
     back into the loop without a human first clearing the escalation).
   - **blocked-on-sibling** has no ADO analogue (no sibling election) → routes to
     remediation.
3. **The findings + verdict-json machine payload posted to a PR thread** — the ADO analog
   of the GitHub sticky status comment, **SHA-pinned** to the reviewed head/base so
   `gather-pr-context` can trust the head/base it reads back.

`apply-verdict` emits the `decision` into its result file, so merge-review's
`published-verdict` gate routes **away from merge** on any non-pass decision, and the stage
returns success — a clean run completion rather than the earlier hard-fail. On the pass path
it publishes the `succeeded` status and emits `decision=pass`. What it must **never** do on
ADO: submit a native review (`pr.review.submit` is undeclared) or write a PR-number label
through the work-item API.

### 6.3 `merge-pr` and `queue-watch` — landing

`merge-pr` gates ADO behind an `isADO` switch. Completion authority is resolved fail-closed
first via `ado:pr:complete`; only then is the completion-authorized provider constructed and
wrapped in the `Dispatcher`. Landing then flows through the **same shared code path** both
providers use:

| Contract step | ADO behavior |
|---|---|
| `DetectMergePolicy` | Any enabled, blocking, non-deleted branch policy scoped to the target ref → `MergeQueue`; otherwise `Direct`. |
| `EnqueuePullRequest` (MergeQueue) | Arm ADO **auto-complete** (the completion job is the queue), idempotently. |
| `PollMergeQueueEntry` (`queue-watch`) | completed → `Merged`; abandoned / auto-complete cleared → `Evicted`; armed → `Pending`. |
| `MergePullRequest` (Direct) | `PATCH status=completed` with `completionOptions{mergeStrategy, mergeCommitMessage}`, SHA-pinned via `lastMergeSourceCommit`, then await the async completion job to a terminal `mergeStatus` (conflict → `ErrMergeConflict`). |

**Landed** is defined solely by the poll reporting the PR merged with a resolvable merge
commit — auto-complete *set* is not landed, *enqueued* is not landed. **Eviction is a
first-class outcome** on both providers, so merge-review's repass loop stays
provider-neutral.

The GitHub-only helpers (Tutor change classification, merged-branch cleanup) require the
concrete GitHub provider and stay nil / gated off on ADO. ADO `PollPullRequest` leaves
`Labels`, `CommentsSince`, `MergeableState`, and `HeadRepository` empty, with deliberate
consequences: the label opt-out conjuncts never fire (the ADO merge path carries no PR
opt-out labels); the advisory-check bypass never applies, so the decision falls through to
the conservative `CheckState == Passing` gate (correct and safe); and branch cleanup is
skipped (no head-repository to act on). The merge-commit message on ADO is assembled
directly from the PR title plus the body's closing references, rather than from a verdict
comment.

### 6.4 `check-issue-staleness` and `gather-sibling-context`

`check-issue-staleness` reads an issue-spec pin from the PR body and re-fetches the work
item via `GetWorkItem` against the backlog project; with no pin present it passes the gate
with zero provider mutation. The stale-write branch (a PR-number label write) is gated off
on ADO. `gather-sibling-context` returns empty sibling context on ADO (no sibling election),
so the review gate sees trivial sibling evidence and the run proceeds.

### 6.5 `post-merge` — close the work item

ADO `post-merge` reduces to a single required action: **close the work item the merged PR
resolved.** The work-item id comes from the PR body's closing reference — *not* the claim
ledger, whose lease was released back at `issue-close-out`; by the time post-merge runs the
body reference is the durable id. The stage then calls `UpdateWorkItemStatus(done)` against
the backlog project, which sets the Completed-category `System.State` and swaps the
`goobers/status:` tag. This is what stops an ADO work item parking at in-review forever. All
sibling fan-out and unpark machinery is gated off — each is a PR-number-as-work-item write.

## 7. pr-remediation on ADO

The remediation lane reuses the thread + label transport. Each remediation stage has an ADO
branch that constructs the ADO provider and calls the native thread / label / single-PR
primitives directly (the GitHub/Gitea `remediationProvider` interface stays those two
providers; ADO is a separate code path routed by provider kind):

- **`gather-pr-context`** recovers the verdict and finding-set by reading the PR threads
  back (`ListPullRequestThreadComments`), trusting the head/base because `apply-verdict`
  SHA-pinned them, and computes the remediation priority from the PR's native labels plus
  the policy-evaluation check state (needs-remediation / failing-CI / behind-base).
- **`push-remediated`** reads the pre-remediation head SHA from the sticky thread and, on a
  successful rework, clears `goobers:needs-remediation` (delete-by-id) so merge-review
  re-selects the reworked PR.
- **`remediation-checkpoint`** drives the escalate / self-heal state machine over the native
  labels: on repeated no-progress it escalates (add `goobers:merge-escalated`, clear
  `goobers:needs-remediation`); when the head advances past a stale escalation it self-heals
  (remove `goobers:merge-escalated`); an operator clearing `goobers:merge-escalated` is an
  explicit request for another review pass.
- **`rebase-pr`** clears `goobers:needs-remediation` on a clean rebase.
- **`pr-claim`** verifies the run's claimed PR is still open via `GetPullRequest`, releasing
  the claim (and returning a terminal no-work result) if it has merged or closed.

The sticky remediation-state comment (carrying the pre-remediation head SHA) is a PR thread
updated in place via the composite comment id.

## 8. Hazards and invariants (consolidated)

- **PR-number → work-item wrong-object write.** The single most dangerous ADO hazard. Every
  GitHub site that writes a label/comment addressed by PR number through the work-item API
  must stay gated off on ADO: the verdict PR-as-issue label trick, the staleness stale-write
  branch, the merge-queue eviction remediation, and the post-merge sibling fan-out / unpark
  set. On ADO these route to PR-native surfaces (labels, status, threads) instead.
- **Empty poll fields on ADO.** `PollPullRequest` leaves `Labels`, `CommentsSince`,
  `MergeableState`, and `HeadRepository` empty. Each empty field has a deliberate,
  documented consequence (§6.3): opt-out conjuncts inert, advisory bypass inert (conservative
  gate wins), branch cleanup skipped.
- **advisoryMode misfire.** The ADO run-branch namespace must be present in the gaggle's
  head prefixes, or a goobers-authored ADO PR is misclassified and never merges (§6.1).
- **Identity strings differ by surface.** The thread/verdict transport uses **displayName**
  end to end (`AuthenticatedLogin` returns displayName; thread authors render as
  displayName). PR authors/reviewers key on **UPN**, and assignee comparison uses
  displayName — so assignee-scoped PR filters and native-review vote paths are intentionally
  not used on the ADO merge path.
- **Completion authority.** `ado:pr:complete` is required for `merge-pr` and `queue-watch`
  and is resolved fail-closed before the provider is built; `ado:pr:write` must never grant
  completion.
- **Preview api-version.** The PR-labels endpoint is only published under a `-preview`
  api-version; the label calls pin it explicitly.

## 9. Conformance

This parity work sits under the provider-contract-conformance model: the ADO provider
declares the capabilities it now implements, and undeclared surfaces fail closed at the
dispatch shim rather than silently no-op. The thread transport and the three PR-label quirks
are pinned by contract/conformance coverage, and the label/thread behaviors were validated
end-to-end against a real ADO organization rather than a fixture server — the quirks are
precisely the class of defect a fake-server suite reports green on while the live loop is
dead. Under the blessed-tier rule, GitHub and ADO move in lockstep on the workflow-required
capability set: a new PR-lifecycle capability lands with contract tests and both blessed
implementations, or with an explicit, declared gap.
