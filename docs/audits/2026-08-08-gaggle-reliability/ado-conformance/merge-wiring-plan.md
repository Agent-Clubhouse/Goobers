The stale entries are prior-session to-dos, not background tasks — I'll leave them and deliver the plan, which is my actual deliverable.

---

# Implementation Plan — Wire the Goobers merge chain to land one clean PR on Azure DevOps

**Scope root:** `/Users/masonallen/source/Goobers/.clubhouse/agents/Goobers-Special-Agent` (all `file:line` below are in this worktree; they match the audit's numbering).
**Target scenario (from wire-truth §3/§5):** ADO PR 359 — build-policy satisfied, zero approvals, `mergeStatus: succeeded`, no conflicts, auto-complete path, then close its work item (WI 1456).
**Hard invariant:** every GitHub site stays byte-identical. Each ADO behavior is a *new* branch reached only when `repo.Provider == providers.ProviderADO`, mirroring the dispatch template at `cmd/goobers/issuecloseout.go:246-276`.

## 0. Ground truth you must internalize before touching code

- **The 5 files the task names are NOT the actual merge-review chain end-to-end.** The real chain (from `reference-workflows/gaggles/goobers/workflows/merge-review.yaml`) is: `reconcile-post-merge` → `pr-select` → **`check-issue-staleness`** → `gather-sibling-context` → review gate → `apply-verdict` → gates → `merge-pr` → `merge-queue-poll` (queue-watch) → `post-merge`. **`gatherprcontext.go` (`gather-pr-context`) is the pr-remediation entrypoint (`gatherprcontext.go:57` docstring, issue #362), not a merge-review stage.** Treat it as a remediation-lane file that must stay GATED OFF on ADO, and read item 4 as covering the actual staleness stage, `check-issue-staleness` (`checkissuestaleness.go`).
- **For PR 359 the land goes through the queue path, not direct merge.** The repo has one enabled/blocking Build policy scoped to `refs/heads/main` (wire-truth §3), so `DetectMergePolicy` → `MergePolicyMergeQueue` (`providers/ado_landing.go:79-107`) → `EnqueuePullRequest` (arm auto-complete) → merge-pr reports `landOutcome=enqueued` → `merge-gate` routes to `queue-watch` → `PollMergeQueueEntry` → `completed`→Merged → `post-merge`. So **`merge-queue-poll.go` is on PR 359's critical path**; `MergePullRequest` (direct) is only reached on a policy-free branch.
- **The Dispatcher already makes the landing surfaces provider-neutral.** `providers.NewDispatcher(p)` (`providers/capability.go:187`) embeds the `Provider` interface and capability-gates the optional landing methods. `*ADOProvider` satisfies `Provider`, declares `CapPRMerge, CapPRLandingDetectPolicy, CapPRLandingEnqueue, CapPRLandingPoll, CapPRCompare, CapBranchDelete` (`providers/ado.go:133-140`), and implements each. **Everything the core land needs is reachable through a dispatcher wrapping an ADO provider — the work is at the cmd/ stage layer, which never constructs one.**

---

## 1. Per-file GitHub-provider construction / concrete-type inventory

Legend: **(A) CORE** = must dispatch to ADO for a single-PR land; **(B) PERIPHERAL** = sibling/demotion/remediation/branch-cleanup, must be a documented no-op on ADO; **(C) IRRELEVANT** to the merge path.

### 1a. `cmd/goobers/mergepr.go` — stage `merge-pr` (CORE stage)

| Line | Site | Class | Disposition on ADO |
|---|---|---|---|
| `116` | `providerToken(capability.GitHubPRMerge)` | **A** | Replace with ADO provider construction (see §3). No github token resolved. |
| `121` | `provider := newCachedGitHubProvider(root, token, …)` → `*providers.GitHubProvider` | **A** | Build `newADOProviderForStage(root, repo)` (`adoprovider.go:53`) instead; hold it behind a var of an interface type both providers satisfy (see §8, "concrete-type traps"). |
| `125` | `dispatcher := providers.NewDispatcher(provider)` | **A** | `NewDispatcher(adoProvider)` — the landing calls flow through here unchanged. |
| `195` | `provider.PollPullRequest(...)` | **A** | `PollPullRequest` is a mandatory `Provider` method (`providers/provider.go:43`); call it via the dispatcher (`dispatcher.PollPullRequest`) so both providers work. ADO impl: `ado_pullrequests.go:135`. |
| `199,207` | `hasAnyLabel(poll.Labels, {noMergeReviewLabel})` / `{abortedRunLabel}` | **B** | ADO `PollPullRequest` leaves `Labels` empty (`ado_pullrequests.go:154-170`) → opt-out never fires. Documented no-op for the clean-PR scope; do NOT try to read PR labels via `wit/workitems`. |
| `230` | `baseMovementIntersectsPR(ctx, dispatcher, …)` | **A** | Dispatcher path: `PullRequestFiles` (mandatory) + `CompareCommits` (`CapPRCompare`, ADO `ado_landing.go:292`). Works. Only invoked if `BaseSHA` moved. |
| `241-251` | `isTutorBranch(...)` + `classifyRemoteTutorChanges(ctx, provider, …)` | **B/C** | `classifyRemoteTutorChanges` takes concrete `*GitHubProvider` (`tutorprpolicy.go:405`). Guard the whole block behind `!ADO` (tutor lane is GitHub-only; PR 359 is not a tutor branch). |
| `259` | `pinnedPassVerdict(poll, verdictAuthor)` (sibling-overlap election conjunct) | **B** | Reads `poll.CommentsSince` (empty on ADO) → returns `false` → the `ok && …` guard is skipped. **Safe no-op by construction** — sibling election is out of scope and correctly never enforced. |
| `284` | `structuredMergeCommitMessage(poll, verdictAuthor)` | **A — BLOCKER** | Calls `pinnedPassVerdict` → not found on ADO → returns error → sets `commitErr` → hard `return 1` at `340-343`. **This is the single hard failure on the ADO pass path.** Fix: on ADO build the commit title/message directly from `poll.Title` + `closingIssueNumbers(poll.Body)` (the same non-verdict parts `structuredMergeCommitMessage` already assembles at `mergepr.go:443-453`), bypassing the verdict comment. Alternatively, but less correct, have the ADO merge-review.yaml supply a non-empty `commitMessage` input so the `if mergeCommitMessage == ""` guard at `279` never enters the verdict lookup. |
| `300` | `detectMergePolicy(ctx, dispatcher, …)` | **A** | Dispatcher `DetectMergePolicy` (`CapPRLandingDetectPolicy`). ADO `ado_landing.go:79`. Cache is provider-agnostic (`mergepolicycache.go:59`). |
| `304-314` | `mergepolicy.ForPolicy(policy)` + `lander.Land(ctx, dispatcher, …)` | **A** | `internal/mergepolicy/mergepolicy.go` calls only `dispatcher.EnqueuePullRequest`/`MergePullRequest`. ADO `ado_landing.go:117/241`. For PR 359 → enqueue. |
| `379` | `cleanupMergedBranch(ctx, poll.HeadRepository, poll.HeadBranch, provider)` | **B** | `cleanupMergedBranch` (`mergepr.go:462`) takes concrete `*GitHubProvider`, and ADO `PollPullRequest` never sets `HeadRepository` (confirmed absent), so it fails `"did not report a head repository"` (`mergepr.go:473-475`). **Gate OFF on ADO** — skip branch cleanup, or (optional, later) route through `dispatcher.DeleteBranch`/`deleteSourceBranch:true`. |
| `462-500` | Inside `cleanupMergedBranch`: `prProvider.ListPullRequests` (477), `providerToken(GitHubBranchDelete)` (490), `newGitHubProvider` (494), `branchProvider.DeleteBranch` (495) | **B** | Entire helper is GitHub-only branch cleanup; not called on ADO. |

### 1b. `cmd/goobers/prselect.go` — stage `pr-select` (CORE stage)

| Line | Site | Class | Disposition on ADO |
|---|---|---|---|
| `92` | `providerToken(capability.GitHubPRWrite)` | **A** | ADO provider construction (§3). |
| `97` | `provider := newCachedGitHubProvider(root, token)` → `*GitHubProvider` | **A** | `newADOProviderForStage`. |
| `121` | `daemonIdentityAuthorLogin(ctx, root, provider)` → calls `provider.AuthenticatedLogin` (`prselect.go:425`) | **A/trap** | ADO has **no `AuthenticatedLogin`** (confirmed). `daemonIdentityAuthorLogin` takes concrete `*GitHubProvider` (`prselect.go:414`). On ADO force it to return `""` so `isOwnPullRequest` falls to the branch-prefix heuristic. **See §8 — this feeds an advisoryMode misfire that silently blocks the merge.** |
| `128,316` | `pullRequestsForSelection(ctx, provider, …)` — concrete `*GitHubProvider` (sig `316-327`); calls `provider.ListPullRequests` (328), `provider.GetPullRequest` (336), `provider.RefCheckState` (343,358) | **A** | `ListPullRequests` mandatory — ADO `ado_pullrequests.go:344`. **`GetPullRequest` and `RefCheckState`/`RefCheckStates` do NOT exist on ADO** (confirmed). ADO CI state comes from policy evaluations inside `PollPullRequest` (`ado_pullrequests.go:175`, `pollPullRequestPolicies`). On ADO resolve each candidate's `CheckState` via `PollPullRequest` instead of `RefCheckState`; resolve the webhook-targeted PR via `PollPullRequest` or list-filter instead of `GetPullRequest`. |
| `340,355` | `identityFilters.MatchesIdentityFields(...)` | **A/trap** | UPN-vs-login misfire (`providers/model.go:822`), but empty in the clean scenario (no author/assignee/requestedReviewer inputs) → matches trivially. |
| `133-180` | blocked-on-sibling + foundation-coupling scan: `liveBlockedOnSiblingBlockers` (139), `loadFoundationCouplings` (156), `flagFoundationCoupling` (164) | **B** | `liveBlockedOnSiblingBlockers` takes `remediationProvider` (ADO does **not** satisfy it — lacks `AuthenticatedLogin`, `ListPullRequestReviewThreads`, `RefCheckState`, `UpdateBranch`, `CIFailures`; `remediationprovider.go:16-41`). `flagFoundationCoupling` takes concrete `*GitHubProvider`. **Gate OFF**: on ADO produce empty `siblingBlocked`/`liveSiblingBlockers`/`couplings` maps. |
| `210,224,237` | `escalationStillBlocks` (210), `demotionStillHolds` (224), `siblingBlocked[...]` skip (237) | **B** | Both take `remediationProvider`. **Gate OFF** — on ADO treat as never-blocked/never-demoted. |

### 1c. `cmd/goobers/gatherprcontext.go` — stage `gather-pr-context` (pr-remediation; PERIPHERAL to this epic)

Whole file is the remediation lane. It already fails closed on ADO: `remediationStageProvider` returns `default-error` for `providers.ProviderADO` (`remediationprovider.go:53-64`), and CONF-6 preflight refuses the workflow. **Leave it that way.** Enumerated GitHub sites for completeness (all **B/C**, none on the single-PR merge path): `providerToken(GitHubPRWrite)` (89), `providerToken(GitHubIssuesWrite)` (100), `providerToken(RepoPush)` (104), `remediationStageProvider(...)` (109), `provider.ListPullRequests` (120), `provider.ListComments` (250), `provider.AuthenticatedLogin` (254). Do not wire ADO here in this epic.

### 1d. `cmd/goobers/mergequeuepoll.go` — stage `queue-watch` (CORE-conditional; on PR 359's path)

| Line | Site | Class | Disposition on ADO |
|---|---|---|---|
| `71` | `providerToken(capability.GitHubPRMerge)` | **A** | ADO provider construction (§3). |
| `76` | `provider := newGitHubProvider(token, …)` → `*GitHubProvider` | **A** | `newADOProviderForStage`; wrap in dispatcher for the poll. |
| `146` | `provider.PollMergeQueueEntry(...)` | **A** | `CapPRLandingPoll`; ADO `ado_landing.go:171`. Call via dispatcher. `completed`→Merged. |
| `156` | `hasAnyLabel(result.Labels, …)` | **B** | ADO `PollMergeQueueEntry` DOES populate `Labels` from PR labels (`ado_landing.go:182-185`); PR 359 has none → opt-out never fires. Safe. |
| `166` | `provider.DequeuePullRequest(...)` | **B** | No ADO `DequeuePullRequest` (opt-out+pending path only). Guard behind `!ADO`. |
| `261` | `mergeQueuePollMerged(ctx, provider, …)` — concrete `*GitHubProvider`; calls `provider.PollPullRequest` (263) + `cleanupMergedBranch` (267) | **A + B** | Reporting merged is CORE (ADO `PollPullRequest` works). Branch cleanup is PERIPHERAL — same nil-`HeadRepository` failure as §1a; gate OFF. |
| `294-324` | eviction/timeout remediation: `providerToken(GitHubIssuesWrite)` (307), `newGitHubProvider` (312), `labelProvider.UpdateWorkItem(ID: pullNumber, AddLabels…)` (313) | **B — HAZARD** | On ADO `UpdateWorkItem` addresses `wit/workitems/{id}`, so passing a **PR number** mutates the unrelated work item with that numeric id. **Must never run on ADO.** Not hit for a clean merge, but guard it. |

### 1e. `cmd/goobers/postmerge.go` — stage `post-merge` (CORE stage, but mostly peripheral machinery)

| Line | Site | Class | Disposition on ADO |
|---|---|---|---|
| `217` | `providerToken(capability.GitHubPRWrite)` | **A** | ADO provider (§3) for the PR poll. |
| `222` | `providerToken(capability.GitHubIssuesWrite)` | **A** | ADO work-items provider (must address **backlogRepo**, see §6). |
| `227` | `provider := newCachedGitHubProvider(root, prToken, …)` | **A** | `newADOProviderForStage`. |
| `228` | `issuesProvider := newCachedGitHubProvider(root, issuesToken, …)` | **A** | ADO provider bound to `backlogRepoRefForStage(root, repo)` (`adoprovider.go:97`). |
| `252` | `provider.PollPullRequest(...)` | **A** | ADO works; supplies `poll.Body`, `poll.BaseBranch`, `poll.Number`. |
| `280` | `fanOutNeedsRemediation(...)` | **B** | Sibling fan-out; concrete `*GitHubProvider`; uses `UpdateWorkItem(ID:pr.Number, label)` (546) and `AuthenticatedLogin` (524). **Gate OFF.** |
| `286,292,298` | `unparkResolvedSiblings`, `unparkSelfHealedEscalations`, `unparkSelfHealedDemotions` | **B** | All sibling/demotion unpark; all `UpdateWorkItem(ID:pr.Number,…)`. **Gate OFF.** |
| `304` | `closeReferencedIssues(ctx, issuesProvider, repo, poll.Body, pullNumber)` | **A** | **The one required post-merge action.** On ADO must target `backlogRepo`, not routed `repo` (see §6). Uses `GetWorkItem` (649), `UpdateWorkItemStatus` (655), `ListComments` (665), `UpdateWorkItem` comment (674) — all implemented on ADO (`ado_workitems.go:178/338/389/458`). |

---

## 2. Minimal interface the CORE land actually calls, and the ADO gaps

The single-clean-PR path calls exactly these provider methods (all reachable via `*Dispatcher` embedding `Provider`, or via a base-`Provider` interface var):

| Method | On base `Provider`? | ADO impl | Status |
|---|---|---|---|
| `PollPullRequest` | yes (`provider.go:43`) | `ado_pullrequests.go:135` | ✅ but empty `Labels`/`CommentsSince`/`MergeableState`/`HeadRepository` |
| `ListPullRequests` | yes (`provider.go:50`) | `ado_pullrequests.go:344` | ✅ |
| `PullRequestFiles` | yes (`provider.go:54`) | ✅ (mandatory) | ✅ (only used by `baseMovementIntersectsPR`, base-move only) |
| `CompareCommits` | `CapPRCompare` | `ado_landing.go:292` | ✅ |
| `DetectMergePolicy` | `CapPRLandingDetectPolicy` | `ado_landing.go:79` | ✅ |
| `EnqueuePullRequest` | `CapPRLandingEnqueue` | `ado_landing.go:117` | ✅ (PR 359 path) |
| `MergePullRequest` | `CapPRMerge` | `ado_landing.go:241` | ✅ (direct-policy path) |
| `PollMergeQueueEntry` | `CapPRLandingPoll` | `ado_landing.go:171` | ✅ |
| `GetWorkItem` / `UpdateWorkItemStatus` / `ListComments` / `UpdateWorkItem` | `BacklogProvider` (mandatory) | `ado_workitems.go` | ✅ (must target backlogRepo) |

**Methods the CORE path calls that ADO does NOT implement (must be replaced/skipped):**

1. **`RefCheckState` / `RefCheckStates`** — called by `pr-select` (`prselect.go:343,358,466`) to populate each candidate's `CheckState`. No ADO method. **On ADO, resolve `CheckState` from `PollPullRequest` (policy evaluations, `pollPullRequestPolicies` → `ado_pullrequests.go:227`).**
2. **`GetPullRequest`** — `pr-select` webhook-targeted path (`prselect.go:336`). No ADO method. **On ADO, use `PollPullRequest` (or list-filter) for the targeted PR.**
3. **`AuthenticatedLogin`** — `pr-select` (`prselect.go:425`), `post-merge` fan-out (`postmerge.go:524`), `gather-pr-context` (`gatherprcontext.go:254`). No ADO method (there is no `AuthenticatedLogin` on ADO — GitHub `github_issues.go:190`, Gitea only). **On ADO return `""`/skip** (drops to branch-prefix identity; see §8 trap).

**`PollPullRequest` empty-field consequences — which merge conjuncts read them, and the correct default:**

- **`Labels` empty** → `hasAnyLabel(poll.Labels, …)` opt-out conjuncts (`mergepr.go:199,207`) never fire. Default: never-opted-out. Acceptable no-op for the clean scenario; do not synthesize labels via `wit/workitems`.
- **`MergeableState` empty** → `ciReadyForMerge` (`mergepr.go:82-87`) advisory-check bypass (`MergeableStateUnstable`) never applies → falls through to the conservative `CheckState == CheckStatePassing` gate. **Correct and safe** (audit code-truth §6). ADO `CheckState` = Passing when the Build policy evaluation is `approved` (wire-truth §5).
- **`CommentsSince` empty** → (a) `pinnedPassVerdict` sibling-election conjunct (`mergepr.go:259`) → `false` → conjunct skipped (safe); (b) `structuredMergeCommitMessage` (`mergepr.go:284`) → **hard error → must be defaulted** from `poll.Title` + `closingIssueNumbers(poll.Body)`.
- **`HeadRepository` nil** → `cleanupMergedBranch` fails; branch cleanup gated OFF on ADO.

---

## 3. The exact capability-grant swap

**Where each command resolves the literal grant:** every stage calls `providerToken(cap)` (`cmd/goobers/providercmd.go:213`), which reads `os.Getenv(executor.CredentialEnvVar(string(cap)))` and fails closed if absent. The concrete grants: `merge-pr` and `merge-queue-poll` → `capability.GitHubPRMerge` (`= "github:pr:merge"`, `internal/capability/capability.go:76`); `pr-select`, `post-merge`, `check-issue-staleness` → `capability.GitHubPRWrite` (`= "github:pr:write"`, `capability.go:60`); `post-merge`/`merge-queue-poll` eviction → `GitHubIssuesWrite`.

**The ADO equivalent (mirror `issuecloseout.go:246-276`):** on `case providers.ProviderADO`, do **not** call `providerToken(github:*)`. Build the provider via `newADOProviderForStage(root, repo)` (`cmd/goobers/adoprovider.go:53` → `adoauth.Provider` → `adoauth.Source`, `internal/adoauth/source.go:17-46`), which resolves the configured auth source (PAT / azure-cli / workload / managed) from `instance.yaml`'s `repo.Auth`, org-scoped. Work-item calls in `post-merge`/`check-issue-staleness` must use `backlogRepoRefForStage(root, repo)` (`adoprovider.go:97`) so they hit the backlog project.

**Capability-registry gap to resolve (design decision, flag to the implementer):** there is **no `ado:pr:merge` / `ado:pr:complete` capability** — the registry has `ADOPRWrite` (which explicitly "does not grant completion/merge authority", `capability.go:95`), `ADOPRStatus`, `ADOWorkItemsWrite`, `ADOPRComment`, `ADOCodeRead`. On ADO the completion authority rides on the config-sourced auth, so `github:pr:merge`'s narrow-grant *capability isolation* (the pr-lifecycle-loop §7 "decider ≠ executor" principle) is **not preserved** unless you add a dedicated `ado:pr:complete` capability and gate merge-pr/merge-queue-poll on it. **Recommendation: add `ado:pr:complete`** to `internal/capability/capability.go` (and `All()`), have the ADO merge-review.yaml declare it on `merge-pr`/`queue-watch`, and construct the ADO provider only after that grant is present — otherwise merge-pr silently acquires completion authority from ordinary `ado:pr:write`, which is exactly what github:pr:merge was designed to prevent.

---

## 4. `check-issue-staleness` (the real staleness stage) + the work-item-link question

**Does the ADO PR carry a native work-item link?** No. Wire-truth §1: `GET /pullRequests/359/workitems` → `{count:0}`; the WI↔PR relationship exists only as (a) the close-out **comment text** on WI 1456 (rev 6) and (b) the PR body's `Fixes #N` line (open-pr writes `Fixes #<itemID>`, not ADO's `AB#N` grammar — code-truth §6). There is no ArtifactLink for native machinery to traverse.

**What `check-issue-staleness` actually needs** (`checkissuestaleness.go`): it does **not** use a native link — it reads an **issue-spec pin embedded in the PR body** (`parseIssueSpecPin(poll.Body)`, `checkissuestaleness.go:91`) and re-fetches the issue via `GetWorkItem` (`:102`). Sites: `newCachedGitHubProvider` at `:80-81`; `prProvider.PollPullRequest` `:86`; `issuesProvider.GetWorkItem` `:102`; and on the stale branch `prProvider.UpdateWorkItem(ID: pullNumber, AddLabels…)` `:140`.

**Minimal correct ADO handling:**
- If the PR body has **no pin** (`havePin == false`) — PR 359's case — the stage sets `stale=false` and passes the gate with zero provider mutation. So the least-risk path is: **on ADO, dispatch the two providers and let the no-pin fast-path pass.** GetWorkItem, if a pin exists, must resolve against `backlogRepoRefForStage` (the pin's `IssueID` is the ADO work-item id, in the backlog project), not routed `repo`.
- The **stale branch must never run on ADO as written**: `UpdateWorkItem(ID: pullNumber, …)` (`:140`) is the **PR-as-work-item hazard** — a PR number into `wit/workitems`. On ADO, if genuinely stale, either publish the "issue changed" signal via `PublishPullRequestStatus` (failed) / route to remediation out-of-band, or skip. For this epic's clean-PR goal, gate the stale-write branch OFF on ADO and always emit `issueStale=false` when no pin is present.

**Net:** staleness on ADO = read the body pin only, resolve any pin against backlogRepo, never write a PR-number label through `wit/workitems`.

---

## 5. Verdict transport for the PASS case

**Confirmed: merge-pr does NOT need the sticky-comment / label verdict machinery for a pass.** The pass decision reaches merge-pr as the **static workflow input** `verdict: "pass"` (`merge-review.yaml:324` → `mergepr.go:132`). The sticky comment / `CommentsSince` is used only for (i) the sibling-overlap election conjunct (`pinnedPassVerdict`, `mergepr.go:259` — skipped on ADO, safe) and (ii) the commit-message rationale (`structuredMergeCommitMessage`, `mergepr.go:284` — must be defaulted on ADO, §1a). No verdict *transport* is required into merge-pr.

**What `apply-verdict` must do on ADO for the pass path:** today it **refuses every non-moot verdict, including pass** (`applyverdict.go:589-592,605-608` "publishing a non-moot verdict is not supported") → the stage errors → the run dies before merge-pr is reached. The load-bearing change: on ADO, a pass verdict must **succeed and emit `decision=pass`** into its result file (so the `published-verdict` gate, which reads `decision==pass` from Outputs — `merge-review.yaml:573-582` — passes) and **publish the verdict as a PR status** via `PublishPullRequestStatus` (`ado_pullrequests.go:304`, genre `goobers`, name `validation` — the ruled/working alternative, wire-proven on PR 359 carrying `goobers/validation = succeeded`). This is the same surface `report-pr-status` uses.

**What apply-verdict must NOT do on ADO:**
- **No native GitHub review** (`pr.review.submit` undeclared; the only vote code is `RequestReview`'s reset-to-0, `ado_pullrequests.go:111-128`).
- **Never the PR-as-work-item `UpdateWorkItem(ID: PR#, labels)` trick** (`applyverdict.go:789-799`) — on ADO the numeric PR id collides with a real work item (wrong-object hazard, code-truth §2). This is currently safe only because that path is GitHub-gated (`applyverdict.go:589-608`); the ADO branch must keep it unreachable.

**Recommended: publish the status iteration-scoped** (wire-truth §2 note: our status is `iterationId:null`, so a post-verdict push does not invalidate it — a stale-approval hole GitHub's dismiss-on-push covers natively). And note (wire-truth §3) the status only *gates* if the customer adds a Status branch policy for `goobers/validation`; absent that, the merge gate is the Build policy alone, which is sufficient for PR 359.

---

## 6. Post-merge minimal — required vs skip

**REQUIRED (the only mandatory post-merge action for a single non-overlapping PR):** close the work item. Mechanism on ADO:
1. Resolve the WI id from **`closingIssueNumbers(poll.Body)`** (`postmerge.go:39` → the `Fixes #N` the ADO open-pr body carries; `N` *is* the work-item id, code-truth §6). **Do not rely on the claim ledger here** — the ledger lease was released at `issue-close-out` (in-review, `issuecloseout.go:482-488`), so by the (later) merge-review run that runs post-merge there is no live claim entry; the durable WI↔PR link is the PR body's closing ref (and the WI comment). This corrects the task's "via the claim ledger's item id" hypothesis: the ledger is gone by post-merge; the body ref is the durable id.
2. Call `UpdateWorkItemStatus(backlogRepo, id, providers.WorkItemStatusDone)` against **`backlogRepoRefForStage(root, repo)`** — ADO sets the Completed-category `System.State` and swaps the `goobers/status:` tag (`ado_workitems.go:338,356-367`). Today `closeReferencedIssues`→`GetWorkItem`/`UpdateWorkItemStatus` use routed `repo` (`postmerge.go:649,655`), which on a split-project ADO instance reads/writes the wrong project — **must switch to backlogRepo**. This is what stops WI 1456 parking at `in-review`/`New` forever (code-truth §3 gap).
3. The dedupe "Merged in pull request #N" comment (`postmerge.go:664-679`) via `ListComments`/`UpdateWorkItem` also targets backlogRepo — ADO `ListComments` addresses work-item comments (`ado_workitems.go:389`), correct.

**SKIP on ADO (documented no-ops):** `fanOutNeedsRemediation` (280), `unparkResolvedSiblings` (286), `unparkSelfHealedEscalations` (292), `unparkSelfHealedDemotions` (298) — all sibling/demotion machinery, all `UpdateWorkItem(ID: pr.Number, …)` PR-as-work-item writes and `AuthenticatedLogin`-dependent. Gate the whole `performPostMerge` body on ADO down to just the work-item close.

---

## 7. Recommended implementation order + make-ci-green checkpoints

Build the land core inside-out so each layer is unit-testable before the one above it depends on it. **Introduce a base-`Provider` interface var** (or route through the dispatcher) in each stage so the concrete-`*GitHubProvider` call sites compile for both providers (see §8).

1. **`merge-pr` (`mergepr.go`)** — the actual land. (a) ADO provider construction + dispatcher; (b) call `PollPullRequest` via dispatcher; (c) default the commit message on ADO (bypass `structuredMergeCommitMessage`); (d) guard the tutor block and `cleanupMergedBranch` behind `!ADO`. **Checkpoint:** `go test ./cmd/goobers/ -run 'TestRunMergePR|TestMergePR'` + `go test ./internal/mergepolicy/…` + `go test ./providers/ -run 'ADO.*(Merge|Enqueue|DetectMergePolicy|Poll)'`.
2. **`merge-queue-poll` (`mergequeuepoll.go`)** — ADO dispatch of `PollMergeQueueEntry`; gate OFF `DequeuePullRequest`, eviction/timeout `UpdateWorkItem(PR#)`, and branch cleanup. **Checkpoint:** `go test ./cmd/goobers/ -run 'TestMergeQueuePoll'`.
3. **`post-merge` (`postmerge.go`)** — ADO dispatch; reduce to the work-item close against **backlogRepo**; gate OFF all sibling/unpark. **Checkpoint:** `go test ./cmd/goobers/ -run 'TestPostMerge|TestCloseReferencedIssues'`.
4. **`pr-select` (`prselect.go`)** — ADO dispatch; `CheckState` via `PollPullRequest`; identity via branch-prefix fallback; gate OFF sibling/foundation scans; targeted-PR via `PollPullRequest`. **Checkpoint:** `go test ./cmd/goobers/ -run 'TestPRSelect|TestPullRequestsForSelection'`.
5. **`apply-verdict` (pass path) + `check-issue-staleness`** — make apply-verdict emit `decision=pass` + publish `goobers/validation` status on ADO (stop refusing); staleness no-op on ADO with backlogRepo pin resolution. **Checkpoint:** `go test ./cmd/goobers/ -run 'TestApplyVerdict.*ADO|TestCheckIssueStaleness'` (an `applyverdict_ado_test.go` already exists, code-truth §6).
6. **`gather-sibling-context`** (not one of the 5, but on the chain) — on ADO return empty sibling context so the review gate has trivial evidence (no siblings), keeping the run moving. `reconcile-post-merge` (first stage, `continueOnError:true`) is GitHub-hardwired but non-fatal — ideally ADO-dispatch or skip, but its failure won't block the run.

**Global gates after each step and at the end:** `make build` (canonical path — memory: never bare `go build -o`); `make ci` (memory: needs Go 1.26.5); `golangci-lint run` with `GOOS=windows` cross-check (platform files); run gates in a **scratch worktree** to avoid the cross-worktree golangci-lint cache false-positive (memory). Watch for **deadcode/SA4032** on any helper that becomes GitHub-only behind a build/provider guard.

---

## 8. Every trap

**Concrete-type assertions / signatures that block ADO substitution (won't compile if `provider` becomes ADO):**
- `cleanupMergedBranch(…, prProvider *providers.GitHubProvider)` — `mergepr.go:462`.
- `mergeQueuePollMerged(…, provider *providers.GitHubProvider, …)` — `mergequeuepoll.go:261`.
- `pullRequestsForSelection(…, provider *providers.GitHubProvider, …)` — `prselect.go:316`.
- `daemonIdentityAuthorLogin(…, provider *providers.GitHubProvider)` — `prselect.go:414`.
- `classifyRemoteTutorChanges(…, provider *providers.GitHubProvider, …)` — `tutorprpolicy.go:405`.
- `flagFoundationCoupling(…, *providers.GitHubProvider, …)` — `foundationcoupling.go:317`; `blockedOnSiblingStillBlocks(…, *providers.GitHubProvider, …)` — `blockedonsibling.go:99`.
- All `postmerge.go` helpers (`fanOutNeedsRemediation`, `unpark*`, `persistPostMergeRemediationHandoff`, `triageSibling`) take concrete `*providers.GitHubProvider`.
- `remediationProvider` (`remediationprovider.go:16`) is **not** satisfied by `*ADOProvider` (missing `AuthenticatedLogin`, `RefCheckState`, `ListPullRequestReviewThreads`, `UpdateBranch`, `CIFailures`) — its compile-time assertion (`:38-41`) only lists GitHub/Gitea. Do NOT try to pass an ADO provider where a `remediationProvider` is expected; gate those call sites OFF.
- **Mitigation:** call the mandatory-`Provider` methods (`PollPullRequest`, `ListPullRequests`, `PullRequestFiles`) through the `*Dispatcher` or a small local interface both providers satisfy; keep the `*GitHubProvider`-typed helpers unreachable on ADO via the provider switch.

**nil-provider panics:** follow the `issuecloseout.go:247-253` template — check `newADOProviderForStage` error and `return 1` *before* any use; never let a failed construction fall through to a nil `provider` deref.

**PR-as-work-item wrong-object writes (numeric PR-id → `wit/workitems/{id}`):** `mergequeuepoll.go:313`, `checkissuestaleness.go:140`, `postmerge.go:546` (and the whole fan-out/unpark set). All mutate an unrelated ADO work item that happens to share the PR's numeric id. Must be unreachable on ADO.

**Branch-cleanup nil `HeadRepository`:** ADO `PollPullRequest`/`PollMergeQueueEntry` never populate `HeadRepository` (confirmed) → `cleanupMergedBranch` fails `"did not report a head repository"` (`mergepr.go:473-475`). Gate OFF.

**GitHub-shaped identity/login comparisons that misfire in the CORE path:**
- **The silent merge-blocker:** `pr-select` sets `advisoryMode = authorScope=="any" && !isOwnPullRequest(...)` (`prselect.go:274`). On ADO `daemonIdentityAuthorLogin` returns `""` (no `AuthenticatedLogin`), so `isOwnPullRequest` (`prselect.go:388-393`) falls to the **branch-prefix heuristic** against `headPrefixes` (`goobers/implementation/,…`). PR 359's branch is `goobers/tb-ado-implementation/…` (wire-truth §1) — **does not match** `goobers/implementation/` → `isOwnPullRequest=false` → `advisoryMode=true` → merge-pr enters advisory mode (`mergepr.go:262-264`) and **never merges**. Fix: the ADO gaggle's `BranchNamespace`/`headPrefixes` (`providerBranchNamespace()`, `providercmd.go:240`) must match the ADO run-branch namespace, or classify "own" some other way. This is the subtlest correctness bug in the whole path and easy to miss because every provider call succeeds.
- `MatchesIdentityFields` (`prselect.go:340,355`; `providers/model.go:822`) — UPN-vs-login; benign only because the clean scenario passes empty author/assignee/requestedReviewer filters.
- `isTrustedMergeReviewAuthor` / `isTrustedMergeReviewStatusComment` (`applyverdict.go:992,1674`, `EqualFold` vs `AuthenticatedLogin`) reached from `pinnedPassVerdict` (`mergepr.go:409`), `gatherPRVerdict` (`gatherprcontext.go:369`), and the post-merge handoff (`postmerge.go:143`) — ADO has no `AuthenticatedLogin` and empty `CommentsSince`, so these paths are inert on ADO *provided* they stay gated OFF; do not "fix" them by inventing an ADO login here (that's the separate identity-parity epic).

**Non-obvious control-flow trap:** `structuredMergeCommitMessage`'s failure sets `commitErr` inside the merge flock closure (`mergepr.go:281-287`), which surfaces as a hard `return 1` at `340-343` — i.e. an ADO pass with a green Build policy *fails the stage* today, not "not merged". The commit-message default (§1a/§5) is what converts this from a run-killing error into a clean land.