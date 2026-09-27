# Design: ADO parity on DSL 2.0 — near-term plan for v0.5.0

> Status: **draft** — for PO review. A near-term, non-disruptive plan that makes the
> existing DSL 2.0 work the same way on Azure DevOps (ADO) as on GitHub for v0.5.0,
> with **no new DSL vocabulary**.
> Tracking: #2061
> Verified: 47de1f0d6 (2026-09-25)
> Builds on: [`ado-provider-parity.md`](ado-provider-parity.md) (the ADO PR lifecycle),
> [`provider-contract-conformance.md`](provider-contract-conformance.md) (provider
> feature capabilities, CONF-6) and
> [ADR 0002](../adr/0002-provider-neutral-capability-namespaces.md) (capability namespaces).
> Follow-on: the DSL 3.0 provider design, which unifies providers and credential
> mapping ([PR #5661](https://github.com/Agent-Clubhouse/Goobers/pull/5661), being rewritten).
> Done/in-flight: credential containment (#5664).

## 1. Summary

ADO support works at the forge-adapter level, but a gaggle on ADO does not drop in
the way a GitHub gaggle does. Against `origin/main` at `47de1f0d6`:

- **The shipped reference gaggle does not validate on ADO.** `pr-remediation`
  derives `pr.review.threads` and `pr.update-branch`, which ADO does not declare
  (`internal/instance/providercapability.go:21-32`).
- **Shipped `merge-review` validates on ADO and then fails at runtime.** `merge-pr`
  needs `ado:pr:complete` on ADO (`cmd/goobers/mergepr.go:121-137`), admission marks
  it optional (`internal/providerstage/manifest.go:301-322`), and no shipped workflow
  declares it.
- **`report-pr-status` cannot be configured at all.** Its policy action is missing
  from the workflow schema enum (`api/schemas/workflow.schema.json:318-358`).
- **Only `azure-cli` auth reaches a stage process**, and only with an Entra-backed
  account. PAT, workload identity and managed identity are resolved *inside* the
  default-deny stage process from `instance.yaml` (`cmd/goobers/adoprovider.go:21-61`),
  which strips their environment. `azure-cli` has no `repo:push` grant, so
  remediation fails (#5656). An MSA-backed org rejects every Goobers call (live probe F1).
- **Several PR-lifecycle calls are wrong against live ADO:** PR-level statuses are
  rejected by "reset on push" status policies (F2), auto-complete is set as the PR
  creator (F3), verdict threads block comment-resolution policies (§2 of the probe),
  and prefix-scoped policies are missed (F7).
- **The shipped implementation workflow cannot claim on ADO.** It requires
  `goobers:ready`, and ADO has no label-transition read (#5554).

None of this is a regression: the v0.4.1 binary and `main` behave byte-identically
on every ADO configuration tested. These are pre-existing gaps that the weekly
read-only live smoke never reached.

This plan fixes them inside DSL 2.0 in **32 single-PR items for v0.5.0** plus 10
for v0.5.x, ordered by impact against risk (§10). The plan has three rules:

1. **Rebinding rule.** In DSL 2.0, a `github:*` capability on a provider-dispatched
   stage means that operation on whatever provider the stage routes to. This is
   already how Gitea works and what `policy_actions.go` says. This plan documents it,
   tests it and makes ADO obey it (§3).
2. **Credentials are minted by the daemon.** Every ADO auth kind resolves in the
   daemon and reaches stages and pods through the existing credential plane, so the
   declared capability selects the credential on ADO as it does on GitHub (§4).
3. **Honour branch policy.** Goobers never bypasses policy and never votes to
   approve. It completes a PR only through auto-complete or through a completion the
   server accepts as policy-satisfied (§5).

## 2. Goals and non-goals

### Goals

- **G1.** Every shipped workflow in `reference-workflows/` and `config-examples/`
  validates unchanged against an ADO gaggle and runs end to end on ADO. The same
  holds for GitHub. Gitea is kept working where that is cheap.
- **G2.** A gaggle entirely on ADO (topology a) works with `azure-cli` on a developer
  machine, and with workload or managed identity in Kubernetes. No PAT is required.
  A store-backed PAT is the fallback.
- **G3.** Goobers honours a policy-protected default branch: required approval and
  green CI, no direct pushes, no completion before policy is met, and no approval by
  a Goobers identity.
- **G4.** Work tracking works on custom, non-uniform Agile-derived processes.
- **G5.** `goobers init --provider=ado` produces an instance that works without
  hand edits.
- **G6.** A live ADO write leg in CI and a soak run gate the v0.5.0 release.

### Non-goals (for DSL 3.0; see the follow-on design)

- New capability names, provider-neutral renames, or aliases.
- Several credentials for the same provider in one gaggle or scope, or choosing
  between credentials inside one stage.
- One gaggle coordinating code on both ADO and GitHub (topology c).
- Unifying the provider interfaces and removing the forked `*ADO` stage functions (#2747).
- Azure DevOps Server (on-premises). Only `dev.azure.com` is in scope, plus legacy
  `*.visualstudio.com` URLs when that is cheap.
- ADO service-hook triggers. Polling stays.

### Customer context (generic)

The driving customer is a large organisation that uses both GitHub and ADO.

| Aspect | Constraint |
|---|---|
| Hosts | Developer machines, with `az` CLI acting as the user. Kubernetes, with managed or workload identity. No PATs. |
| Process | Agile-like but custom, and different from team to team |
| Branch policy | Approval and green CI are required on `main`, with no direct merges. Goobers must not complete a PR before policy is met. A non-personal Goobers identity must not approve. |
| Topologies | (a) Entirely GitHub or entirely ADO: this plan. (b) Backlog on GitHub, code on ADO: a guard in v0.5.0 and support in v0.5.x (§7.2). (c) A client on ADO and a service on GitHub in one gaggle: DSL 3.0. |

## 3. DSL 2.0 capability semantics on ADO

### 3.1 The rebinding rule

DSL 2.0 is supported and frozen (`internal/supportmatrix/supportmatrix.go:132-191`).
Its capability names were written GitHub-first. The codebase already treats them as
provider-neutral on stages that dispatch through the provider seam:

- **What `policy_actions.go` says.** `internal/workflow/v_2_0/policy_actions.go:51-56`
  describes the backlog and PR actions as having a "canonical capability name [that]
  is a GitHub name the credential seam rebinds per provider".
- **Gitea already works this way.** It resolves `GOOBERS_CRED_<github:*>` for every
  operation (`cmd/goobers/stageprovider.go:335-345`).
- **ADO does not.** It ignores the declared capability entirely
  (`stageprovider.go:328-333`) and has two exceptions: `open-pr` with a PAT, and the
  `ado:pr:complete` presence check.

**Rule (DSL 2.0 only).** A `github:*` capability declared on a stage whose command
dispatches through `newProviderForStage` authorizes **the same operation on the
provider that the stage routes to**. It also selects the credential for that
provider. The routing works like this:

| Capability family | Routes to | ADO operation |
|---|---|---|
| `github:issues:read`, `github:issues:write`, `github:issues:approve`, `github:milestones:write` | the gaggle's **backlog** provider | Boards work items, tags, comments, state |
| `github:pr:write`, `github:pr:review`, `github:branch:delete` | the gaggle's **project** provider | PR threads, labels, statuses, source-branch deletion |
| `github:pr:merge` | the **project** provider, as **landing authority** | PR completion and auto-complete |
| `provider:pr:write`, `provider:ci:cancel`, `repo:push` | the project provider | already neutral |

This names what the code already does. It adds no vocabulary, and no GitHub
behaviour changes.

**Enforcement.**

- **Credential.** On ADO the declared capability selects the credential through the
  same injector as GitHub (§4, ADO-N17 and ADO-N18). "An undeclared capability means
  no credential" (ARCHITECTURE §5) then holds on ADO too.
- **Conformance test.** It extends `cmd/goobers/provider_dispatch_conformance_test.go`
  (#5664). For every manifest row that names a `github:*` capability on a
  provider-dispatched command, it asserts that the ADO path consumes that capability's
  credential and no other.
- **Inert names warn.** `ado:code:read`, `ado:pr:comment`, `ado:work-items:write` and
  `ado:pr:write` have no consumer on ADO today. In DSL 2.0 they are accepted and draw
  an advisory warning that names the `github:*` name that authorizes the operation.
  `goobers validate --strict` treats the warning as neutral. This does not break
  configurations that followed the older docs.
- **Compile-matrix gate** (§8.1). Every shipped workflow is validated against an ADO
  gaggle.

**Why this is consistent with ADR 0002.** ADR 0002 says names are not aliases
(`docs/adr/0002-provider-neutral-capability-namespaces.md:49-51`). It governs how
*new* provider-neutral names are introduced and migrated, "fail[ing] validation" on
stale spellings (`:55-63`). This plan introduces no name and migrates nothing.

DSL 2.0 cannot take new names without breaking frozen configs
(`docs/design/dsl-version-lifecycle.md`), and it already depends on rebinding for
Gitea and for every ADO backlog stage. Making ADO obey the documented 2.0 meaning
removes an inconsistency. It does not add an alias.

The ADR's neutral names (`provider:*`) are the DSL 3.0 answer (PR #5661).

### 3.2 `report-pr-status` (ADO-N23)

Today the stage needs `github:pr:write` from the manifest
(`internal/providerstage/manifest.go:357-361`) and `ado:pr:status` from the policy
action (`v_2_0/policy_actions.go:57`). The schema enum does not list the action, so
no working configuration exists.

**Fix:**

- Add `report-pr-status` to the policy-action enums: `api/schemas/workflow.schema.json`
  and both enums in `api/schemas/goober.schema.json`.
- Make the policy action require `github:pr:write`, so there is one requirement that
  agrees with the manifest and the Gitea path.
- Accept `ado:pr:status` as optional, so declaring it is harmless.
- Mirror the change in the `v_3_0` table.

The change is purely additive: nothing that validates today changes.

### 3.3 Landing: `merge-pr` and `merge-queue-poll` (ADO-N2)

**Rule: `github:pr:merge` is landing authority on every provider in DSL 2.0.**
`ado:pr:complete` is accepted and never required.

At runtime, the ADO branch of `merge-pr` (`mergepr.go:121-137`) and of
`merge-queue-poll` (`mergequeuepoll.go:533-537`) resolves `ado:pr:complete` if the
stage declared it. Otherwise it resolves `github:pr:merge`. Either way, the resolved
token authenticates the completion call (ADO-N18), so it is no longer just a
presence check.

**SEC-053 is preserved** (`docs/requirements/security.md:246`, merge authority is a
separate, conjunctive grant):

- **The landing name stays distinct.** `github:pr:merge` is not `github:pr:write`.
  A stage that holds only PR-write authority still cannot complete a PR. That is the
  property the `mergepr.go` comment protects ("a stage carrying only ado:pr:write
  must never silently acquire completion authority").
- **The revocation fence still covers both names.** The config-generation fence
  already lists both (`cmd/goobers/configgeneration_revocation.go:131`), so revoking
  either name still takes effect before the next land.
- **Admission is unchanged.** The existing audits still keep landing authority off
  implementation, remediation and agentic stages.
- **Branch deletion rides completion.** `github:branch:delete` stays required. On ADO
  it sets `completionOptions.deleteSourceBranch` (ADO-N25).

**Alternative rejected.** A provider-aware manifest row that *requires*
`ado:pr:complete` on ADO. It would fail every existing ADO `merge-review`, including
the shipped one, and the PO's goal is drop-in.

### 3.4 `pr-remediation` on ADO (ADO-N14, N15, N20, N22)

Four gaps keep the shipped `pr-remediation` from running on ADO. The plan fixes each
one natively rather than shipping an ADO variant:

| Stage | Gap | Fix |
|---|---|---|
| `gather-review-threads`, `resolve-review-threads` | `pr.review.threads` not declared | **Implement ADO review threads** (ADO-N20). ADO has first-class threads. |
| `pr-claim` | verify step refuses ADO (#5655) | Verify the PR state and source head through the ADO poll (ADO-N14) |
| `update-behind-pr` | `pr.update-branch` not declared | An ADO override that derives no capability. The stage reports `not-applicable` (ADO-N15). |
| `gather-ci-failures` | refuses ADO (#5652) | Minimal native evidence from policy evaluations (ADO-N22) |

**Why `update-behind-pr` is not applicable on ADO.** ADO always computes the merge
against the current target (`mergeStatus`), and there is no "branch must be up to
date" policy. A behind-but-clean PR therefore needs no update. A conflicting PR goes
to `rebase-pr`, which already has an ADO branch.

This is exactly what the `stageProviderCapabilityOverrides` mechanism is for
(`providercapability.go:53-57`): the override has a real implementation behind it.

**ADO review threads (ADO-N20).** Implement `ListPullRequestReviewThreads`,
reply-to-thread and resolve-thread on `ADOProvider`, then declare
`pr.review.threads` and `pr.review.resolve`:

| Operation | ADO call and mapping |
|---|---|
| List | `GET …/pullRequests/{id}/threads` (all pages). Skip `commentType: system` threads, deleted threads, and the Goobers identity's own threads (matched by GUID, ADO-N5). |
| Resolved state | `active` and `pending` are unresolved. `fixed`, `wontFix`, `closed` and `byDesign` are resolved. |
| Position | `threadContext.filePath` and `rightFileStart.line`. A thread without a context is a general comment. |
| Outdated | From `pullRequestThreadContext.iterationContext`. When it is absent, treat the thread as live: this fails open, as Gitea does. |
| Reply | `POST …/threads/{id}/comments` |
| Resolve | `PATCH …/threads/{id}` `{status: "fixed"}` (live probe §2: `fixed` clears a comment-resolution policy) |

## 4. ADO auth that works everywhere, without PATs

### 4.1 Target shape

Every auth kind resolves **in the daemon**. The daemon registers the repository's
ADO credential source as a minting source for the repository's grants, the same way
it does for a GitHub App (`cmd/goobers/runnerwiring_credentials.go:69-84`). At stage
start it mints a token and delivers it:

- to a local stage, as `GOOBERS_CRED_<declared capability>`, through the injector;
- to a pod, through `POST /v1/credentials/resolve`
  (`internal/httpapi/credentialplane.go:15-93`).

The stage's ADO factory builds a static credential from the declared capability's
token (`withStageProviderCapability`). It never re-reads `repos[].auth` from
`instance.yaml`. As a result:

- No `runner.envPassthrough` is needed.
- `repo:push` exists for every auth kind (#5656).
- A PAT held in a secret store works inside stages.
- ADO stages can run in pods.

| Kind | Daemon source (exists, `internal/adoauth/source.go:17-46`) | Header | Delivered as |
|---|---|---|---|
| `azure-cli` | `az account get-access-token --resource 499b84ac-…` | `Bearer` + `X-VSS-ForceMsaPassThrough: true` | minted bearer |
| `workload-identity` | `azidentity.NewWorkloadIdentityCredential` (optional `clientId`, #5591) | `Bearer` + passthrough header | minted bearer |
| `managed-identity` | `azidentity.NewManagedIdentityCredential` (optional user-assigned `clientId`) | `Bearer` + passthrough header | minted bearer |
| `pat` (fallback) | env, file or **secret store** (`credentials.StoreResolver`) | `Basic` | resolved PAT |

The daemon sends the header scheme (`bearer` or `basic`) with the token as a
non-secret internal stage variable. The stage must not guess it from the token's
shape.

**Token lifetime.** Entra tokens last about 60–90 minutes. Deterministic ADO stages
are short. A stage that runs past expiry gets a 401. The existing single 401 retry
then re-resolves through the credential plane for pods, and fails with a clear
"credential expired" error locally. Long-lived agentic stages do not hold ADO
credentials at all (credential containment, #5664).

**Harness.** Remove the ADO exception that tolerates a missing grant
(`internal/harness/environment.go:170-173`). Once every kind backs its grants, ADO
fails closed like GitHub.

### 4.2 The MSA passthrough header (ADO-N4)

On an org that is not Entra-backed, and for MSA accounts, a valid Entra bearer token
gets a 302 to sign-in or a 401 (TF400813). This happens for REST and for git. It
works only if the request also sends `X-VSS-ForceMsaPassThrough: true` (live probe F1).
Microsoft's own `az devops` SDK sends that header on every request.

The fix:

- Send the header on every ADO REST request that uses `Bearer`
  (`providers/ado.go:572`).
- Send it as a second `http.<url>.extraheader` value in `adoGitAuthEnv`
  (`providers/ado_auth.go:109-130`). Git accepts multiple values for that key.

Before v0.5.0 is tagged, verify on an Entra-backed org that the header is harmless
there (§8.4). Fallback: send it only for `azure-cli`.

### 4.3 Git auth, push and scrubbing

- **Git auth.** Git always authenticates through the child-only
  `GIT_CONFIG_COUNT`/`http.<url>.extraheader` env. The credential never goes on argv
  or into a persisted config.
- **push-branch (ADO-N3).** #5555 fixes the slot-1 collision in `composeGitEnv` that
  drops the header in `push-branch`. Header slots are then allocated, not hard-coded,
  so the passthrough header and the safe-directory entry can coexist.
- **Scrubbing.** Every minted or resolved token is registered with the daemon's
  `SecretRegistrar` when it is minted: the raw value, the `Bearer` form and the
  base64 `Basic` form. Gitea already registers both of its forms. Stage providers
  stop passing a nil registrar. This corrects the overstatement in
  `docs/guides/ado-authentication.md`.

### 4.4 Identity (ADO-N5)

`AuthenticatedLogin` returns `providerDisplayName` (`providers/ado_prthreads.go:268,403`),
and display names are not unique. The stable key is `connectionData.authenticatedUser.id`.
The same GUID appears as `AssignedTo.id`, `createdBy.id` and `autoCompleteSetBy.id`
(live probe §5).

ADO-N5 adds an identity read that returns `{id, uniqueName, displayName}`:

- `id` comes from `authenticatedUser.id`.
- The UPN comes from `properties.Account.$value`, because `connectionData` has no
  `uniqueName`.

The result is cached per credential. Every "is this me" check uses `id`: claim
breadcrumbs, own threads, auto-complete and the audit.

### 4.5 Operator requirements (documented, not coded)

**Service principal or managed identity.** It must be added to each org explicitly
at **Basic** access; Stakeholder access cannot use Repos. It lives in the org's
connected tenant and uses a paid seat. It is authorized by ADO groups and
permissions, not by Entra app permissions.

**Permissions the identity needs:**

- On the repo: Contribute, Contribute to pull requests, and Create branch.
- Force push, but only when branch deletion is wanted.
- On the backlog area path: permission to edit work items, plus Create tag
  definition, or pre-created `goobers:*` tags.

**Permissions it must not hold:** either "Bypass policies" permission (for completing
PRs or for pushing).

**PATs.** Global PATs stop working on 2026-12-01. Docs and `init` recommend
`azure-cli`, workload identity or managed identity. The fallback is an org-scoped PAT
held in a secret store.

## 5. ADO PR lifecycle correctness

The customer's policy maps onto server behaviour as follows:

- **No completion before policy.** Enforced by the server, as long as the identity
  lacks bypass. A direct completion is refused with 403 (F4).
- **No approval.** Enforced by Goobers behaviour. The API lets any identity vote for
  itself (F5). Whether a self-vote counts depends on the customer's
  `creatorVoteCounts` and `blockLastPusherVote` settings.

Goobers never sets `bypassPolicy` and never sends a non-zero vote (checked:
`git grep -i bypassPolicy` finds nothing, and the only vote write is `vote: 0` in
`RequestReview`, `providers/ado_pullrequests.go:130`). ADO-N9 makes both a
conformance assertion so they stay true.

| # | Behaviour | Today | Change | Evidence |
|---|---|---|---|---|
| ADO-N7 | Status publish | PR-level `POST …/statuses` (`ado_pullrequests.go:326`) | Post to `…/iterations/{latest}/statuses`. A status policy with reset-on-push rejects PR-level statuses with 403. | F2 |
| ADO-N6 | Auto-complete identity | `autoCompleteSetBy` = PR creator (`ado_landing.go:156`) | The caller's `authenticatedUser.id`. Any other id returns 400. | F3 |
| ADO-N8 | Informational threads | posted `active` (`ado_prthreads.go:69`) | Post verdict and status threads `closed`. `active` blocks comment-resolution policies. | probe §2 |
| ADO-N19 | Merge readiness | config match on `refName` equality, first page only (`ado_landing.go:79-106`) | Readiness comes from **policy evaluations** for the PR (below) | F4, F7 |
| ADO-N9 | Head pin on direct completion | client-side compare, then PATCH (`ado_landing.go:290-299`) | Send `lastMergeSourceCommit`. Map 409 TF401192 to "head moved" and 403 `GitPullRequestUpdateRejectedByPolicyException` to "policy not met". Never retry with bypass. Keep the client-side check for auto-complete, which can't be pinned. | F6 |
| ADO-N26 | Push into a protected ref | a generic push failure | Classify TF402455 / `GitRefUpdateRejectedByPolicyException` as "branch is policy-protected", not an auth failure, in `push-branch` and in remediation pushes | F8 |
| ADO-N25 | Source-branch cleanup | `deleteSourceBranch` defined, never set (`ado_landing.go:395`) | Set it when the stage holds `github:branch:delete`. Verify that a non-admin identity may delete (§8.4). | probe §4 |
| ADO-N37 | `RequestReview` | puts a UPN in the path, which returns 400 (`ado_pullrequests.go:118`) | Resolve the identity GUID first, keep `vote: 0`. It has no caller today, so this is v0.5.x. | F5 |
| ADO-N11 | Label and tag compare | PR labels lowercased; work-item tags compared exactly | Compare case-insensitively everywhere. ADO returns the casing of whoever wrote the tag first, and PR labels share the tag namespace. | F10, F11, #2750 |
| — | Description cap | 4,000 characters, footer preserved (`ado_pullrequests.go:25`) | Keep it. Confirmed live on both create and update. The live leg asserts it. | probe §2 |

### 5.1 Merge readiness from policy evaluations (ADO-N19)

`GET _apis/policy/evaluations?artifactId=vstfs:///CodeReview/CodeReviewId/{projectId}/{prId}`
(pinned to `7.1-preview.1`; `7.1` returns 400) is the per-PR truth. It already
applies prefix scopes, path filters and lazy evaluation.

**Enqueue or merge.**

- If any enabled, blocking evaluation is not `approved` or `notApplicable`, arm
  auto-complete. Otherwise complete directly with a head pin.
- If a policy was added after the PR existed, its evaluation may appear late (probe
  §2). Fall back to scanning the configurations when the evaluations list is empty:
  - page through `x-ms-continuationtoken`;
  - match `Prefix` scopes by ref folder;
  - treat a policy with an empty scope as repo-wide.

**Classification** (feeds `ci-poll` and `merge-queue-poll`):

| Evaluation | Meaning in Goobers |
|---|---|
| Build or status policy `queued` / `running` | CI pending |
| Build or status policy `rejected` / `broken` | CI failed. The build link is in `context.buildId`. |
| Minimum-reviewers or required-reviewers policy `queued` | **Waiting on a human.** Not CI pending, and never a remediation trigger. It stays `queued` even after a self-vote (F5). |
| Comment-resolution policy `rejected` | Unresolved threads, which feed `pr-remediation` |
| Work-item-linking policy `rejected` | A missing link. `open-pr` adds `workItemRefs` when the backlog is ADO. |

With auto-complete armed and a human approval still missing, the PR stays `active`
(F4). `merge-queue-poll` reports "awaiting human approval" instead of timing out or
treating the PR as evicted.

## 6. ADO backlog correctness

| # | Item | Change | Evidence |
|---|---|---|---|
| ADO-N10 | Claim breadcrumb authorship | Count only breadcrumbs whose `createdBy.id` equals the authenticated id. The code comment admits that authorship is not checked (`providers/ado_workitems.go:686`). | probe §5 |
| ADO-N21 | `goobers:ready` / label transitions (#5554) | Implement `ListWorkItemLabelTransitionsForItem` (`ado_workitems.go:799`) from `GET workitems/{id}/updates`: `System.Tags` old/new diffs timed by each update's `System.ChangedDate` new value (an update's `revisedDate` is when that revision was superseded, not when it was made), paged with `$top`/`$skip`, and a fail-closed error past the 10,000-revision cap. | features §3.9 |
| ADO-N32 | Blockers | Today any `Dependency-Reverse` link excludes the item, even when the predecessor is closed (`adoBlockedByCount`, `ado_workitems.go:965`). Implement the blocker checker: hydrate the predecessors and count one as blocking unless its category is Completed, Removed or Resolved (see below). Declare `backlog.blockers` (#2061). | probe §4 |
| ADO-N33 | Hydration | Replace the per-item N+1 GET with `POST _apis/wit/workitemsbatch` (200 ids per call, `$expand: Relations`) | F12 |
| ADO-N27 | Custom processes | Key states by type **name**, never `referenceName`. This is already true for `adoWorkItemStateCategories` (`ado_workitems.go:1106-1141`); keep it and test it against an inherited process. When no type is given, the create type is the project's **Requirement-category default type** from `workitemtypecategories`, not a hard-coded `"Issue"` (`ado_workitems.go:278`). Also send `multilineFieldsFormat` and `format=markdown`. | probe §4 |
| ADO-N11 | Tags | Tags are case-insensitive on read. A `,` or `;` is already refused on write (`validateADOTags`, `ado_workitems.go:1058`). Humans' comma tags are split by the server, which is harmless. | probe §4 |
| ADO-N28 | Idempotent close | Before closing, re-read the state category. Already Completed is success. Resolved (reached through `transitionWorkItems` or `Fixes #`) moves on to Completed, or stops at Resolved for a type without a Completed transition. A 412 on `test /rev` re-reads and retries. | F9 |
| ADO-N29 | Terminal claim cleanup (#5648) | Release the provider claim epoch and the `goobers:claimed` tag on terminal runs for ADO. Today this is skipped for non-GitHub providers (`cmd/goobers/terminalclaimmarker.go:82-84`). | #5648 |
| ADO-N38 | Legacy claim tag (#1990) | Stop reading and clearing `goobers:claim-run:<b64>` | #1990 |

**Resolved counts as done for blockers.** In stock Agile, a Bug is `Resolved` in the
Resolved category, while a User Story's `Resolved` state is in the InProgress category
(live probe §4). PR completion with `transitionWorkItems` leaves Bugs in Resolved
(F9). So "the predecessor's code has landed" is Resolved *by category*.

**Done states are configurable per gaggle (PO decision).** Custom, non-uniform
processes need an override, so the gaggle's `backlog` block gains an optional
setting. It is gaggle configuration, not workflow vocabulary:

```yaml
backlog:
  provider: ado
  project: example-project
  doneStates:
    categories: [Resolved, Completed, Removed]   # default when omitted
    byType:                                      # optional per-type state names
      Bug: [Closed]                              # a team that uses Resolved as "awaiting QA"
```

- **Categories** give a process-agnostic default. `byType` matches state names for
  one work item type and takes precedence for that type.
- **Uses.** The same setting decides when a predecessor stops blocking (ADO-N32) and
  when a claimed item counts as already done (ADO-N28). Goobers' own close still
  targets the Completed category.
- **Validation.** Unknown category names are errors. Unknown state names warn, and
  are checked against the project's real per-type states by
  `validate --check-repos` (ADO-N34).
- **Other providers.** GitHub and Gitea map closed to Completed, so the setting is
  accepted but has no effect there.

**Iterations: a declared, unsupported gap in v0.5.0.** `set-milestone` on ADO now
refuses cleanly with "use an Azure Boards iteration" (credential containment, #5664).
The capability matrix records milestones as "not modelled on ADO". `init
--provider=ado` stops copying curator instructions that call `set-milestone`
(ADO-N12). Implementing `set-milestone` as an `System.IterationPath` write is
ADO-N41 (v0.5.x), and only if a customer workflow needs it. Iteration paths are
per-team trees, so a numeric milestone does not map onto them cleanly. That belongs
in the DSL 3.0 "planning bucket".

## 7. Onboarding and topology

### 7.1 `init --provider=ado` (ADO-N12)

Today `init` generates GitHub-spelled workflows, no `merge-review`, a PAT by default,
and curator and nominator instructions copied verbatim from the GitHub example
(`internal/instance/guided.go:895-910`). ADO-N12 changes this:

- **Workflows.** The default module set includes `merge-review`, which validates as
  shipped once ADO-N2 lands. `work-nomination` is refused on ADO with a clear message,
  because `file-issues` is GitHub-only (`cmd/goobers/fileissues.go:195`).
- **Auth.** The default is `azure-cli`, and `--repo-auth-kind` accepts `azure-cli`,
  `workload-identity`, `managed-identity` and `pat`. The help text currently says
  `azcli` (`cmd/goobers/init.go:149`). The scaffold adds no `envPassthrough`.
- **Instructions.** ADO instruction variants drop `set-milestone`, `gh issue edit` and
  GitHub blocked-by, and describe tags, iterations as unsupported, and predecessor links.
- **Acceptance.** `init` followed by `validate --strict` passes, and the compile-matrix
  gate (§8.1) covers the scaffold's output.

### 7.2 Topology (b): backlog on GitHub, code on ADO

**It can be done in DSL 2.0 without the "multiple same-provider credentials" feature.**
The gaggle uses two *different* providers, with one credential each.

**What already works.**

- `GaggleSpec.Project` and `GaggleSpec.Backlog` are independent
  (`api/v1alpha1/gaggle_types.go:34-39`).
- CONF-6 checks `backlog.*` features against the backlog provider
  (`providercapability.go:136-190`).
- The portal's work-item lookup builds a backlog-provider ref
  (`cmd/goobers/statusprovider.go:13-40`).

**What is broken.** Every backlog stage opens the **routed project provider**
(`cmd/goobers/backlogquery.go:249-258,313-330`). `backlogRepoRefForStage` swaps only
the ADO project tier (`cmd/goobers/adoprovider.go:83-155`). A GitHub backlog on an
ADO gaggle would therefore run WIQL against ADO Boards, and validation accepts it
without warning.

**v0.5.0 (ADO-N13).** Validation fails closed when `backlog.provider !=
project.provider`, naming v0.5.x as the release that adds support. The ADO-project /
ADO-backlog project split keeps working.

**v0.5.x (ADO-N31): what (b) needs.**

1. **Route by role.**
   - Generalize `backlogRepoRefForStage` and `backlogRepoRefForGaggle` to return the
     backlog provider's ref, following the `statusprovider.go` pattern.
   - Backlog stages open that provider: `backlog-query`, `issue-close-out`,
     `post-merge`'s issue half, `backlog-assignment`, `backlog-health`,
     `check-issue-staleness`, and the park and failure handlers.
2. **Route credentials by capability family, per §3.1.**
   - `github:issues:*` and `github:milestones:write` bind to the backlog repository's
     credential. This is a `repos[]` entry for the GitHub backlog repo (App or
     token), matched by the `Backlog.Project` owner/name.
   - PR and repo capabilities bind to the project repository's credential.
   - `RunnerGrants` (`internal/credentials/scoping.go:33-69`) gains a role-aware
     binding choice. It needs no new config keys.
3. **Stop cross-provider closing references.**
   - `open-pr` appends `Fixes #<id>` (`cmd/goobers/openpr.go:132`). On ADO, `#<id>`
     means *ADO work item* `<id>`, and a squash commit message resolves it (F9).
     That would transition an unrelated ADO work item.
   - When the backlog provider differs from the project provider, write the full
     GitHub issue URL instead. `post-merge` then closes the claimed item from the
     claims ledger through the backlog provider, not by parsing the PR body.
4. **Key claims by the backlog provider.** `ClaimKey{Gaggle, Provider, ExternalID}`
   (`internal/localscheduler/claim.go:19-41`) must carry the backlog provider.
5. **Skip GitHub PR-coupled extras.** In `backlog-query`, the open-PR backstop and
   contested-file dispatch use the project provider's PR list, or are skipped.
6. **Gate it.** The compile-matrix gate gains a (b) gaggle, and the live leg gets one
   (b) scenario: a GitHub issue in the test repo and an ADO PR.

The reverse arrangement (backlog on ADO, code on GitHub) uses the same machinery.
Topology (c) needs two *project* providers in one gaggle, which is DSL 3.0.

### 7.3 `validate --check-repos` on ADO (ADO-N34, v0.5.x)

This adds read-only checks against configured endpoints only (SEC-048, no phone-home):

| Check | ADO call |
|---|---|
| Identity | `connectionData` (with the passthrough header). Report the id and UPN. |
| Repo read | Repo read and `git ls-remote` (exist today) |
| Permissions | `_apis/security/permissionevaluationbatch` for GitRepositories Contribute, PullRequestContribute, CreateBranch and ForcePush |
| Bypass | **Warn** if the identity holds PullRequestBypassPolicy or PolicyExempt. That violates the customer rule. |
| Policies | Read policy configurations on the default branch, and warn when a blocking **Prefix** policy covers `refs/heads/` (F8: Goobers cannot push run branches) |
| Boards | The per-type states read for the resolved create type (ADO-N27), plus a one-item WIQL (exists today) |

## 8. Conformance and test plan

### 8.1 Compile-matrix gate (ADO-N1)

A merge-tier test that loads every shipped workflow and every `init --template=standard`
scaffold. For each, it synthesises a gaggle per provider, `github` and `ado`, and runs
full validation, including CONF-6. Gitea runs too but only reports.

- The gate starts with an **explicit expected-failure list**: `pr-remediation` on ADO,
  citing ADO-N14, N15, N20 and N22. Each fixing PR deletes its own entry.
- An entry that unexpectedly passes fails the gate, so the list cannot rot.
- After ADO-N31 the gate adds a topology (b) gaggle.

This single gate would have caught every §1 validation finding.

### 8.2 Live ADO write leg in CI (ADO-N16)

A new workflow runs `-tags=liveadowrite` weekly, on `workflow_dispatch`, and on
labelled PRs that touch `providers/ado*`. It targets the PO's test org through repo
secrets and vars: the existing `vars.ADO_ORG_URL`, `vars.ADO_PROJECT` and
`secrets.ADO_PAT`, plus a new `vars.ADO_WRITE_REPOSITORY`. The credential is an
**organization-scoped** PAT (`vso.code_write`, `vso.code_status`, `vso.threads_full`,
`vso.work_write`; at most 90 days). The org is MSA-backed, so service principals and
OIDC federation are not available there (§8.4).

| Rule | Detail |
|---|---|
| Scratch repo only | A dedicated repo. A provisioning script creates its policies once; tests only read them. No test sets a blocking Prefix `refs/heads/` policy, which would block every push (F8). |
| Namespaced | Branches under `goobers-live/<run_id>/`, the tag `goobers-live`, and a run-id footer in PR and work-item bodies |
| Idempotent | Find-or-create by run id. A re-run of the same id converges. |
| Cleanup | Abandon the run's PRs. Move its work items to Removed, or Closed where the type has no Removed. Delete only its own branches. A janitor abandons live-prefixed PRs older than 24 hours. |
| Never delete shared objects | The repo, policies, tag definitions, other runs' items and the testbed repo |
| Serialised | A `concurrency:` group |

**Coverage:**

- **Labels:** add, remove by id, mixed case.
- **Threads:** post `closed`, then list, reply and resolve.
- **Statuses:** an iteration status satisfies a reset-on-push policy.
- **Auto-complete:** armed as self; the PR stays `active` while a reviewer is required.
- **Direct completion:** a stale head returns 409, an unmet policy returns 403, and an
  unpoliced target completes with `deleteSourceBranch`.
- **Pushes:** a TF402455 refusal is classified.
- **Claims:** a claim race and the author-GUID filter.
- **Tags:** CAS with a 412 retry.
- **`goobers:ready`:** the transition is read from the work-item updates.
- **Blockers:** a Resolved predecessor and a Completed predecessor.
- **Work items:** create with the default type on an inherited process, then close
  after a server-side transition.
- **Descriptions:** 4,000 characters is accepted and 4,001 is rejected.

This closes #2753 for ADO, and provisions #4602 (fixture drift).

### 8.3 Soak (release gate for v0.5.0)

**Setup.**

- **Length and auth.** 7 days on the PO's test org, on a developer machine, using
  `azure-cli` with the passthrough header.
- **Gaggle.** Entirely on ADO, running `implementation`, `merge-review`,
  `pr-remediation` and `backlog-curation`.
- **Policies on the soak repo's `main`:**
  - minimum reviewers 1, with `creatorVoteCounts: false` and `blockLastPusherVote: true`;
  - a status policy on Goobers' genre, with reset-on-push;
  - comment resolution;
  - work-item linking;
  - build validation, if a pipeline can be provisioned.
- **The human reviewer.** A second test identity, driven by a script. It votes only
  after Goobers' status succeeds, and it leaves active threads at random to drive
  remediation.
- **Disruption.** The soak must cross at least three token expiries and one daemon
  restart in the middle of a run.

**Pass criteria.**

- **Zero** PRs completed with a `bypassReason`, and zero completed other than by
  auto-complete or a policy-satisfied completion.
- **Zero** non-zero votes by the Goobers identity.
- **No** stuck claims, orphaned `goobers:claimed` tags or leftover run branches after
  terminal runs.
- **100%** of merged PRs have their work item in Completed.
- **No** runs that end in an auth error.
- **No throttling:** no `Retry-After` or `X-RateLimit-Delay`, and the summed
  `x-ratelimit-cost` is reported.

### 8.4 An Entra-connected test org (SP, workload identity, managed identity)

Service principals and managed identities can only join Entra-connected orgs, so the
PO's MSA-backed org **cannot test them** (live probe §7). Customer orgs will be
Entra-backed. Needed:

1. **An Entra tenant** controlled by the PO, and an **ADO org connected to it**. Its
   project uses an **inherited, customised Agile process** (renamed states and a
   custom type), so the process tests run there too.
2. **One app registration or service principal.**
   - Give it a federated credential for this repo's GitHub Actions OIDC subject,
     restricted to a protected environment.
   - Add it to the org at **Basic**, with Contribute on the scratch repo and no
     bypass permissions.
3. **A workload-identity leg without a cluster.** The workflow writes the Actions OIDC
   token to a file and sets `AZURE_FEDERATED_TOKEN_FILE`, `AZURE_CLIENT_ID` and
   `AZURE_TENANT_ID`, which runs the exact `WorkloadIdentityCredential` path. The same
   service principal covers `azure-cli` through `az login --service-principal
   --federated-token`.
4. **One user-assigned managed identity** on a small Azure VM or AKS node pool, for a
   managed-identity leg that is run manually before each release. GitHub-hosted
   runners have no IMDS.
5. **One or two Basic seats.** They may fit within the org's free Basic allowance;
   this needs confirming.

This leg (ADO-N36) also records three checks:

- the passthrough header is harmless on an Entra-backed org (§4.2);
- a non-admin identity can use `deleteSourceBranch`;
- a non-personal identity's self-vote does not count when `creatorVoteCounts` is false.

### 8.5 Gitea (advisory)

Gitea stays experimental and is report-only in the matrix gate. §3.1 already describes
how it works. No ADO item changes a Gitea path, and ADO-N23 keeps its
`github:pr:write` requirement. Live Gitea legs (#2441) are out of scope.

## 9. Docs plan

Each doc change ships in the PR that makes it true. Generated docs change at the
source and are regenerated with `make docs`. The worst wrong docs are in **bold**.

| Workstream (items) | Docs to fix |
|---|---|
| A. Capability semantics (N2, N23, N24) | **`docs/requirements/pr-lifecycle.md` PRL-040/PRL-072** and **`docs/design/ado-provider-parity.md` §2, §6.3, §8, §10.3**. Both say `ado:pr:complete` replaces `github:pr:merge` and is required; state the §3.3 rule instead. **`docs/reference/workflow-primitives/capabilities.md` and `skills/goobers-dsl-author/references/dsl-reference.md`**: the rebinding rule, the inert `ado:*` names, and the missing declarable capabilities. Refresh the dslauthor captures byte for byte. Add a status banner to ADR 0002 pointing to this plan (#2726). |
| B. Auth (N3, N4, N17, N18) | **`docs/guides/ado-authentication.md`**, rewritten: a table of kind, where it resolves (daemon), and grants; the passthrough header; SP/MI as an org user with a Basic license; minimum and forbidden permissions; the 2026-12-01 global PAT retirement; store-backed PATs. **ARCHITECTURE §5 and `docs/stage-contract.md`**: "credential non-injection" is now true on ADO; add a "credential delivery" subsection. `docs/guides/github-token-scopes.md`: describe which repository credentials each harness variable may carry (#5664). `deploy/reference/README.md`: workload identity for the daemon pod. |
| C. PR lifecycle (N6–N9, N19, N20, N25) | `ado-provider-parity.md` §10 (branch deletion, PRL-082 stale), and a new "Merge review on Azure DevOps" section: policy evaluations, the human wait, no bypass, no approval. `docs/cli` via `applyverdict.go` and `reportprstatus.go` help (generated). |
| D. Backlog (N10, N21, N27–N29) | **`ado-provider-parity.md` §4.1**: it claims the claim-spoof gap is closed. The claims section of `ado-authentication.md` still describes the pre-#1979 owner tag. `docs/requirements/backlog-providers.md` BL-010/BL-033. The capability matrix "not modelled" notes (iterations, PR labels) in `providers/capability_matrix.go`. |
| E. Onboarding (N12, N13) | **`examples/ado-onboarding/README.md`**: what `init` actually produces. `docs/cli` via `init.go:149`, `connect.go` and `fileissues.go` help (generated). A new `docs/guides/ado-limitations.md` listing the §2 non-goals and topology (b) status. |
| F. Tests (N1, N16) | `docs/design/provider-contract-conformance.md` §5 (live write leg). `docs/design/README.md` (regenerate with `go run ./test/designstatus -write`). |

## 10. Work breakdown

Impact (I) and risk (R) are H, M or L. There are **32 v0.5.0 must-haves** and 10
v0.5.x items. Within each band, rows are ordered by impact, then risk. Every PR runs
`go test -race` on the packages it touches, and `make ci` locally where the gates
apply.

| ID | Title | WS | I | R | Deps | Acceptance criteria | Test plan | Rel | Issues |
|---|---|---|---|---|---|---|---|---|---|
| ADO-N1 | Compile-matrix gate: shipped workflows and scaffolds × {github, ado}, Gitea advisory | F | H | L | — | The gate runs in merge-tier CI. The expected-failure list names only `pr-remediation`/ADO, and an unexpected pass fails. | The gate is its own test. Mutation: drop `github:pr:merge` from a copy and expect a failure. | 0.5.0 | #2753 |
| ADO-N2 | Landing rule: `github:pr:merge` completes on ADO; `ado:pr:complete` optional | A | H | L | N18 for token use | Shipped `merge-review` passes validation and lands on ADO with only GitHub-spelled grants. Both names stay in the revocation fence. | Unit tests: the mp-gh / mp-ado / mp-both matrix, and the fence test for both names. Live: a completion. | 0.5.0 | #2726 |
| ADO-N3 | `push-branch` git-config slot collision | B | H | L | — | `push-branch` pushes to ADO with the header present. Slots are allocated, not fixed. | Unit test on the composed env. Live: push a run branch. | 0.5.0 | #5555 |
| ADO-N4 | `X-VSS-ForceMsaPassThrough` on Bearer REST and git | B | H | L | — | `azure-cli` works against an MSA-backed org for REST, clone and push. | Unit tests on headers and extraheader. Live: a manual `azure-cli` run. Entra check in N36. | 0.5.0 | — |
| ADO-N5 | ADO identity by `authenticatedUser.id` | C | H | L | — | The provider exposes `{id, uniqueName, displayName}`. All "is me" checks use `id`. | Fixture test on `connectionData`. Live: the id equals `createdBy.id` on a created PR. | 0.5.0 | — |
| ADO-N6 | `autoCompleteSetBy` = the caller | C | H | L | N5 | Enqueue succeeds on a PR created by someone else. | Unit test on the request body. Live: arm auto-complete on a PR created by another identity. | 0.5.0 | — |
| ADO-N7 | Iteration-level PR statuses | C | H | L | — | A status satisfies a reset-on-push status policy. A new push re-queues it. | Unit test on the endpoint. Live: policy evaluation `approved`. | 0.5.0 | — |
| ADO-N8 | Informational threads posted `closed` | C | H | L | — | Goobers verdict threads never make a comment-resolution policy fail. | Unit test on the body. Live: the policy stays `approved`. | 0.5.0 | — |
| ADO-N9 | Head-pinned direct completion; 409 and 403 mapping; no-bypass / no-vote conformance | C | H | L | — | Completion sends `lastMergeSourceCommit`. 409 maps to head-moved and 403 to policy-not-met. A static test fails if `bypassPolicy` or a non-zero vote appears. | Unit and conformance tests. Live: the 409 and 403 cases. | 0.5.0 | — |
| ADO-N10 | Claim breadcrumbs filtered by author GUID | D | H | L | N5 | A breadcrumb from another identity never wins a claim. The parity doc §4.1 is corrected. | Unit test with a forged breadcrumb. Live: a two-identity race in the Entra org (N36). | 0.5.0 | — |
| ADO-N11 | Case-insensitive tag and PR-label compare; partial label add | D | H | L | — | `requireLabels`, markers and PR labels match regardless of first-writer casing. A multi-label add reports partial success. | Unit tests with mixed case. Live: create `GOOBERS:READY` first. | 0.5.0 | #2750 |
| ADO-N12 | `init --provider=ado` produces a working instance | E | H | L | N2 | The default modules include `merge-review`. Auth defaults to `azure-cli`, and every kind is accepted. ADO instructions are used, and `work-nomination` is refused. `validate --strict` passes. | Golden scaffold test. Covered by the N1 gate. | 0.5.0 | — |
| ADO-N13 | Guard: backlog provider ≠ project provider fails validation | E | H | L | — | A mixed gaggle is refused with a diagnostic that names v0.5.x. An ADO→ADO project split still passes. | Validation unit tests. | 0.5.0 | — |
| ADO-N14 | `pr-claim` verifies PR state and head on ADO | C | H | L | — | `pr-claim` verifies an ADO PR through the provider poll. | Unit test with an ADO fake. Covered by the N1 gate. | 0.5.0 | #5655 |
| ADO-N15 | `update-behind-pr` is not-applicable on ADO | C | H | L | — | The ADO override derives no capability, and the stage returns `not-applicable`. The chain continues. | Derivation and stage unit tests. | 0.5.0 | — |
| ADO-N16 | Live ADO write leg in CI (§8.2) | F | H | L | N3–N9 | Weekly green. The run is idempotent, namespaced and cleans up after itself, with no shared-object deletes. The fixture-drift leg is provisioned. | The leg itself. A re-run with the same id converges. | 0.5.0 | #2753, #4602 |
| ADO-N17 | Daemon-side ADO credential source backs repo grants (every kind, store PATs, scrubber) | B | H | M | — | Every ADO auth kind backs every credentialed capability, including `repo:push`. The token is registered with the scrubber at mint time. | Unit tests per kind with fakes. Scrubber test. Live: an `azure-cli` remediation push. | 0.5.0 | #5656 |
| ADO-N18 | ADO stage factory consumes the declared capability's credential; drop the harness ADO tolerance | B | H | M | N17 | No ADO stage reads `repos[].auth`. An undeclared capability means no credential on ADO. It works in pods. | The dispatch conformance test (§3.1). The harness fails closed for ADO. A pod stage test. | 0.5.0 | #5656, #2726 |
| ADO-N19 | Merge readiness from policy evaluations | C | H | M | N5 | Prefix and empty scopes are honoured. Configurations are paged when used as the fallback. A queued reviewer policy reports "awaiting human", not CI pending. | Fixture tests per evaluation shape. Live: prefix policy and human-wait cases. | 0.5.0 | — |
| ADO-N20 | ADO review threads: list, reply, resolve | C | H | M | N5, N8 | Declares `pr.review.threads` and `pr.review.resolve`. `gather-review-threads` and `resolve-review-threads` run on ADO. | Contract-corpus fixtures. Declared⇔implemented test. Live: resolve clears the policy. | 0.5.0 | — |
| ADO-N21 | Label transitions from work-item updates (`goobers:ready`) | D | H | M | N11 | The shipped `implementation` claims on ADO with `requireLabels: goobers:ready`. It fails closed past the revision cap. | Fixture tests on updates paging. Live: tag, then claim. | 0.5.0 | #5554 |
| ADO-N22 | `gather-ci-failures` minimal ADO evidence | C | H | M | N19 | Rejected policy evaluations appear as findings, with a build link from `context.buildId`. Log fetch is out of scope. | Fixture test. Live: a rejected status policy. | 0.5.0 | #5652 |
| ADO-N23 | `report-pr-status`: add it to the schema enums; one requirement (`github:pr:write`) | A | M | L | — | The rs-* matrix cases validate. Gitea is unchanged. | Schema and compile tests. `make ci` schema checks. | 0.5.0 | #2726 |
| ADO-N24 | Rebinding rule: inert `ado:*` advisory; conformance test | A | M | L | N18 | A warning names the authorizing `github:*` name. The strict verdict is unchanged. | Validation unit test. Dispatch conformance test. | 0.5.0 | #2726 |
| ADO-N25 | `deleteSourceBranch` on completion | C | M | L | N2 | Run branches are deleted on completion when `github:branch:delete` is held. | Unit test on the body. Live: the branch is gone. | 0.5.0 | — |
| ADO-N26 | Classify TF402455 policy-protected pushes | C | M | L | N3 | Push errors name the policy, not the auth. There is no auth retry. | Unit test on the error text. Live: push to protected `main`. | 0.5.0 | — |
| ADO-N27 | Create type from the Requirement category; markdown fields | D | M | L | — | No hard-coded `"Issue"`. Works on Agile, Scrum, Basic and an inherited process. Descriptions and comments are markdown. | Fixture tests per process. Live: create on an inherited process. | 0.5.0 | — |
| ADO-N28 | Idempotent close over server transitions | D | M | L | — | Already-Completed counts as success. Resolved advances. A 412 re-reads. | Unit tests. Live: close after `transitionWorkItems`. | 0.5.0 | — |
| ADO-N29 | Terminal claim cleanup on ADO | D | M | L | N10 | Terminal runs release the epoch and the `goobers:claimed` tag. | Unit test. Live: abort a run, and the tag is gone. | 0.5.0 | #5648 |
| ADO-N30 | Enforce `readiness.maxOpenPRs` on ADO | E | M | L | — | The ADO count is enforced. Like every provider, a count that cannot be read admits (fails open). | Unit test with an ADO fake. | 0.5.0 | #5649 |
| ADO-N42 | Containment follow-ups: MCP `credentialRefs` and command-scoped GitHub credential variables follow the provider rule of #5664 | B | M | L | — | A capability-based MCP credential and a `GOOBERS_CRED_GITHUB_*` variable are materialised only for the provider they belong to. | Harness and MCP config unit tests per provider. | 0.5.0 | — |
| ADO-N31 | Topology (b): route backlog by role; credentials by family; no cross-provider `Fixes #` | E | H | M | N13, N17, N18 | A GitHub backlog with ADO code runs claim → PR → merge → close. The N13 guard is lifted. | N1 gate with a (b) gaggle. Live: one (b) scenario. | 0.5.x | — |
| ADO-N32 | Blockers use predecessor state; configurable `backlog.doneStates`; declare `backlog.blockers` | D | M | M | N33 | An item whose predecessor is in a done state (default Resolved, Completed or Removed; per-gaggle override) is eligible. An open predecessor blocks. | Fixture tests incl. a `byType` override. Live: link cases. | 0.5.x | #2061 |
| ADO-N33 | `workitemsbatch` hydration | D | M | L | — | At most one call per 200 WIQL hits. | Fixture test on call count. | 0.5.x | — |
| ADO-N34 | `validate --check-repos` ADO permission and policy reads | E | M | L | N5 | Reports identity and missing permissions. Warns on held bypass and blanket Prefix policies. Reads only. | Fixture tests. The no-phone-home gate. | 0.5.x | — |
| ADO-N35 | Accept `*.visualstudio.com` URLs (normalise) | E | L | L | — | Legacy remotes match in `push-branch` and in routing. | Unit tests on URL forms. | 0.5.x | — |
| ADO-N36 | Entra-org live legs: SP, workload identity, managed identity (§8.4) | F | H | L | N17, N18, org provisioned | The workload-identity leg and `azure-cli`-with-SP are weekly green (**gates v0.5.0**, PO decision 4). The managed-identity run is recorded before release but does not gate. The three §8.4 verifications are recorded. | The leg itself. | 0.5.0 | — |
| ADO-N37 | `RequestReview` by identity GUID | C | L | L | N5 | Reviewer strings resolve to a GUID. The call succeeds. | Fixture test. | 0.5.x | — |
| ADO-N38 | Remove the legacy claim-tag fallback | D | L | L | — | The `goobers:claim-run:*` tag is no longer read or cleared. | Unit tests. | 0.5.x | #1990 |
| ADO-N39 | `run --pr` accepts API-form ADO URLs | E | L | L | — | Both URL forms resolve. | Unit tests. | 0.5.x | #5651 |
| ADO-N40 | Rate-limit headers: absent means unknown; honour `X-RateLimit-Delay` | B | L | L | — | Missing headers never read as "0 remaining". The delay is surfaced in diagnostics. | Unit tests. | 0.5.x | — |
| ADO-N41 | `set-milestone` via `System.IterationPath` (only on demand) | D | L | M | — | Only if a customer workflow needs it (§6). | Fixture tests. | 0.5.x | — |

**Issue mapping.**

- #2061 (the ADO epic): this plan is its v0.5.0 slice.
- #2726 (v0.5.1): ADO-N2, N18, N23 and N24. Propose moving it to v0.5.0.
- #2750 (v0.5.1): ADO-N11.
- #2747 (forked `*ADO` functions): deferred to DSL 3.0 (PR #5661).

**Release cut.** One ship criterion: v0.5.0 is tagged when every row marked 0.5.0
has merged, the compile-matrix gate (N1) passes with an empty expected-failure list
for GitHub and ADO, the live ADO write leg (N16) is green, the workload-identity leg
of N36 is green, and the soak (§8.3) has passed. No 0.5.0 row may slip on its own.
If a row would delay the tag, the PO decides explicitly to reclassify it. A row can
be reclassified only if G1 and G2 still hold without it. That rules out N2, N12,
N14, N15, N17, N18, N20, N21 and N22, which make shipped workflows and non-PAT auth
work on ADO. Any reclassification is recorded on the issue and in the release notes'
known-issues section.

**Critical path.**

- N1 lands first.
- N17 → N18 → N2.
- N5 → N6, N10, N19 and N20.
- N19 → N22.
- The soak (§8.3) starts after N2, N12, N14, N15, N16 and N17–N22 merge.
- N36's workload-identity leg runs once N17 and N18 merge and the Entra org exists.

## 11. PO decisions (2026-09-25)

1. **Topology (b):** the v0.5.0 validation guard (ADO-N13), with full support
   (ADO-N31) as the first v0.5.x patch.
2. **Landing:** `github:pr:merge` is landing authority on ADO in DSL 2.0, and
   `ado:pr:complete` is accepted but optional (§3.3). DSL 3.0 names it
   `provider:pr:land`.
3. **Passthrough header:** send `X-VSS-ForceMsaPassThrough: true` on every Bearer
   request. Narrow it only if the Entra-org check (ADO-N36) shows any effect (§4.2).
4. **Entra test org:** the PO provisions the tenant and the ADO org and grants CLI
   access. The app registration, OIDC federation, org membership and permissions
   are scripted (§8.4). v0.5.0 is gated on the workload-identity leg; the
   managed-identity result is recorded but not blocking.
5. **Blocker done states:** Resolved, Completed and Removed categories by default,
   configurable per gaggle through `backlog.doneStates` (§6).
6. **`init --provider=ado` auth default:** `azure-cli`. Workload and managed
   identity are selectable, and PAT is an explicit opt-in (an org-scoped PAT in a
   secret store) (§7.1).

## Appendix A. Live-probe facts cited

The live probe ran on 2026-09-25 against Azure DevOps Services, with a scratch repo
in a stock-Agile project, authenticated by an Entra user token for an MSA account.
The raw logs contain private identifiers and are not reproducible from this
repository. The facts:

| Ref | Fact |
|---|---|
| F1 | On an MSA-backed org, a Bearer token gets a 302 or a 401 (TF400813) on REST and git unless `X-VSS-ForceMsaPassThrough: true` is sent. With it, every call worked. |
| F2 | A PR-level status that matches a status policy with `invalidateOnSourceUpdate: true` returns 403 "requires status to be posted on iteration". Iteration statuses satisfy it. Commit statuses never do. |
| F3 | `autoCompleteSetBy.id` must be the caller's own id (or empty). Anything else returns 400. |
| F4 | With auto-complete armed and a minimum-reviewer policy unmet, the PR stays `active`. A direct completion without bypass returns 403 `GitPullRequestUpdateRejectedByPolicyException`. |
| F5 | A self-vote of 10 is accepted but does not satisfy the policy under `creatorVoteCounts: false`. Voting as another identity returns 400 TF401186. A UPN reviewer path returns 400. |
| F6 | Completion with a stale `lastMergeSourceCommit` returns 409 TF401192. Auto-complete cannot be pinned (400). |
| F7 | Prefix scopes match by ref folder. The filtered configurations endpoint returns only Exact scopes. Evaluations are authoritative. |
| F8 | Any enabled, blocking policy makes a ref PR-only (TF402455). A blocking `refs/heads/` Prefix policy blocks pushes to every branch. |
| F9 | `transitionWorkItems` moved a linked Bug to Resolved, not Completed. `Fixes #n` in a squash message on the default branch closed an unlinked Task. |
| F10 | PR lists include labels without `includeLabels`. A single-PR GET never includes them. The labels API accepts `7.1`. |
| F11 | PR labels share the project tag namespace. Matching is case-insensitive, and the first writer's casing wins. |
| F12 | `POST _apis/wit/workitemsbatch` works (200 ids, `$expand: Relations`). `$batch` writes work but are not atomic. |
| §2 | An `active` thread fails a comment-resolution policy. `closed`, no status, or `fixed` pass. Descriptions are capped at 4,000 characters on create and update. Unmet approval reads `queued`. |
| §4 | Customised processes rename `referenceName` but keep `name`. States differ per type (the Issue type has no Proposed state; Bug has no Removed state). A comma in a tag write silently splits the tag. Adding a link does not bump the reverse end's `rev`. |
| §5 | `connectionData.authenticatedUser.id` equals `AssignedTo.id`, `createdBy.id` and `autoCompleteSetBy.id`. There is no `uniqueName`. The UPN is in `properties.Account.$value`. |
| §7 | A user token has no per-API scopes. Service principals and managed identities need an Entra-connected org and a Basic license. |
