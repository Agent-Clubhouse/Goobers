All evidence gathered. Assembling the ground-truth report.

# ADO Empirical Ground Truth — freemasoninc/Goobers/goobers-testbed-ado (wire evidence, 2026-08-09)

All REST reads against api-version=7.1 with the special-agent PAT. Everything on the wire — seeds, daemon writes, pipeline — shows one identity ("Mason Allen", masra91@live.com), so actor attribution below is by timing correlation, not identity.

## 1. PR 359 as it exists

**Headline: PR 359 is still ACTIVE. "Closed out" ≠ completed on ADO — close-out parked the work item and left the PR open for merge-review.**

```json
{"pullRequestId":359,"status":"active","closedDate":null,
 "title":"ledger summary reports the wrong AVERAGE for every category",
 "sourceRefName":"refs/heads/goobers/tb-ado-implementation/fda1e742443517dbb2dabb83fae0e586",
 "targetRefName":"refs/heads/main","mergeStatus":"succeeded",
 "lastMergeSourceCommit":"71b45eb4...","lastMergeTargetCommit":"044b636c...","lastMergeCommit":"946e9e5b...",
 "reviewers":[],"labels":null,"completionOptions":null,"autoCompleteSetBy":null,"completionQueueTime":null}
```
- `reviewers: []` — we added no reviewer, not even self. `labels: null` and `GET /labels` → `{"count":0}` — PR-level labels unused on ADO (all our label semantics went to work-item tags instead).
- `completionOptions: null` / no auto-complete — nothing queued this PR for completion; merge-pr on ADO would be a from-scratch `PATCH status=completed` with explicit completionOptions.
- `GET /pullRequests/359/workitems` → `{"count":0,"value":[]}` — **zero linked work items**. The close-out wrote the PR URL as comment *text* on WI 1456 (see §2) but never created an ArtifactLink, so ADO's native PR↔work-item machinery (auto-transition on merge, "complete linked work items" completionOption, traceability views) has nothing to act on.

**Our published status (report-pr-status), verbatim:**
```json
{"id":1,"state":"succeeded","description":"Goobers reviewer verdict and local pytest suite passed",
 "context":{"name":"validation","genre":"goobers"},
 "creationDate":"2026-08-09T06:00:00.7083239Z","createdBy":"Mason Allen","iterationId":null,"targetUrl":null}
```
- Exact identity a Status policy would need: **genre `goobers`, name `validation`** (policy statusName `goobers/validation`).
- `iterationId: null` — published **PR-scoped, not iteration-scoped**. Contrast Azure Pipelines' own statuses (ids 2–4) which carry `"iterationId":1`. Consequence: a new push to the source branch would NOT invalidate our status; a Status policy configured with "reset on source update" off would keep passing against stale code. (Also note status id 2's raw JSON has **no `state` key at all** — ADO's `notSet` serializes as absent; any consumer must treat missing state as pending/unknown, not error.)
- `targetUrl: null` — no link back to run/portal evidence.

## 2. Work items 1456–1460

The claimed item was **1456** (not 1460) — title matches PR 359. Current state of all five:

| id | State | Tags (verbatim) | AssignedTo |
|---|---|---|---|
| 1456 | **New** | `bug; goobers/status:in-review; goobers:approved; goobers:claimed; goobers:ready; reporting` | null |
| 1457 | New | `cli; enhancement; goobers:approved; goobers:ready; reporting` | null |
| 1458 | New | `cli; enhancement; goobers:approved; goobers:ready` | null |
| 1459 | New | `enhancement; formatting; goobers:approved; goobers:ready` | null |
| 1460 | New | `goobers:approved; goobers:ready; tests` | null |

- Tag strings are semicolon-space separated, ADO alphabetizes them; casing exactly as written. Note 1456 carries **both** `goobers:ready` and `goobers/status:in-review` simultaneously — the park works only because the implementation selector's `excludeLabels` matches `goobers/status:in-review` (coldstart TWEAK 4); ready was never removed.
- `System.State` = "New" on all five, **including the completed one**. `System.AssignedTo` never touched. Our entire lifecycle is expressed in tags + comments; ADO state machine untouched.

**WI 1456 full mutation history (GET /workItems/1456/updates, 6 revs):**
- rev 1, 02:52:40 — created, tags `bug; reporting` (no goobers tags at creation).
- rev 2, **05:55:23.503** — tags → `bug; goobers:approved; goobers:ready; reporting` (trust + ready appear).
- rev 3, **05:55:24.223** — comment only: `"goobers-claim: run=fda1e742443517dbb2dabb83fae0e586\n\nClaimed by Goobers run `fda1e742...` for exactly-once processing."` — **the claim is a System.History comment, no tag, no state, no assignee.**
- rev 4, **06:00:49.343** — tags += `goobers:claimed` — the claimed *tag* landed at **close-out**, ~5 min after the claim comment, not at claim time.
- rev 5, **06:00:49.513** — tags += `goobers/status:in-review` — this is what "closed out in-review" concretely wrote.
- rev 6, 06:00:49.x — comment: `"Implementation complete: https://dev.azure.com/freemasoninc/35bcd05f-.../_apis/git/repositories/ea8da249-.../pullRequests/359 is open for merge-review."` — the PR reference is an **_apis GUID URL** (API form, not the human web URL), and text-only: `relations: []`, no ArtifactLink ever created.

**Attribution caveat on rev 2:** it landed 5.4 s *into* the run's query-backlog stage (05:55:18.19–05:55:24.51) and 0.72 s before the claim comment. Same PAT identity as everything else. Two readings: (a) operator's standup seed sweep ran concurrently at ~05:55:23 (1457's tag-add rev is the latest rev so its timestamp is the `9999-01-01` sentinel — can't confirm the other four were tagged at 02:52 vs 05:55); (b) the run itself wrote its own `goobers:approved` trust tag — which would be a trust-label bypass. **The wire cannot distinguish; the code half must check whether the ADO claim path can ever write the trust tag.**

## 3. Branch / repo / policy state

- Default branch `refs/heads/main`. Exactly two heads: `main` @ 044b636c and **the run branch still exists** — `goobers/tb-ado-implementation/fda1e742...` @ 71b45eb4 (= PR source tip).
- Policies (GET /policy/configurations) — **exactly one**:
```json
{"id":9,"isEnabled":true,"isBlocking":true,"type":"Build",
 "settings":{"buildDefinitionId":28,"queueOnSourceUpdateOnly":true,"manualQueueOnly":false,
   "displayName":"CI","validDuration":720.0,
   "scope":[{"refName":"refs/heads/main","matchKind":"Exact","repositoryId":"ea8da249-..."}]}}
```
- No Minimum-reviewers policy, no Status policy, no work-item-linking policy, no comment-resolution policy. **Consequently our `goobers/validation` PR status is currently decorative — no policy consumes it.** A gating merge-review on ADO requires the customer to create a Status policy for genre/name `goobers`/`validation`.
- What a merge of PR 359 requires today: build policy id 9 satisfied (it is — see §5; evaluation `approved`, `buildIsNotCurrent:false`, validDuration 12 h from build) + nothing else. `mergeStatus: "succeeded"` — no conflicts. Zero approvals required.

## 4. Tag vocabulary reality

GET /wit/tags → 10 tags total: `bug, cli, enhancement, formatting, reporting, tests` + exactly four goobers tags:
```
goobers/status:in-review   (f8b4fb05)
goobers:approved           (31d2cd8c)
goobers:claimed            (27c3a744)
goobers:ready              (54e98ea7)
```
- Seeded vocabulary (standup): `goobers:approved`, `goobers:ready`. **Created implicitly by our run** (ADO auto-creates a project-wide tag on first use, no pre-registration): `goobers:claimed` and `goobers/status:in-review`, both first applied 06:00:49. The mixed delimiter convention (`goobers:` vs `goobers/status:`) is now permanently in the customer's project-wide tag namespace.
- No `goobers:needs-human` / `goobers:needs-remediation` exist — those paths have never fired on ADO.

## 5. Pipeline evidence (definition 28, PR-validation)

```json
{"id":579,"buildNumber":"20260809.1","status":"completed","result":"succeeded","reason":"pullRequest",
 "sourceBranch":"refs/pull/359/merge","sourceVersion":"946e9e5b...",
 "queueTime":"2026-08-09T05:59:59.810Z","finishTime":"2026-08-09T06:00:36.677Z",
 "definition":"goobers-testbed-ado-ci","triggerInfo":{"pr.number":"359","pr.isFork":"False","pr.triggerRepository.Type":"TfsGit"}}
```
- Built the **speculative merge ref** `refs/pull/359/merge` at `sourceVersion` = PR's `lastMergeCommit` 946e9e5b — ADO's analog of GitHub's merge ref, auto-queued by the branch policy 0.5 s after PR creation.
- Policy evaluation for PR 359 (`vstfs:///CodeReview/CodeReviewId/35bcd05f-.../359`): `{"status":"approved","configurationId":9,"context":{"buildId":579,"buildDefinitionId":28,"buildIsNotCurrent":false}}` — this **policy evaluation record**, not the build resource, is what "CI green" means for merge purposes on ADO; ci-poll's 47 s stage (06:00:00.83–06:00:47.76) brackets the build finishing at 06:00:36, consistent with polling one of build-status/PR-status/policy-evaluation (which surface it reads is the code half's question — the coldstart doc flagged it undocumented).
- The pipeline itself also published PR statuses (ids 2–4, `createdBy: "Azure Pipelines Test Service"`, genre `goobers-testbed-ado-ci`, name `codecoverage`, lifecycle absent-state→`pending`→`notApplicable`, iteration-scoped) — so any PR-status reader on ADO must tolerate third-party statuses interleaved with ours and pick latest-per-(genre,name).

## 6. Claim trail: telemetry vs wire

Runs (telemetry.db, gaggle `testbed-ado`): 5× `tb-ado-backlog-curation` failed 05:47–05:51 (`reconcile backlog metadata: backlog curation/reconcile is not supported on Azure DevOps yet (BL-033)` — matches known gap, fail-closed, verbatim in run_errors); 1× `tb-ado-implementation` 24a4c4fb failed 05:53 (`read ready-label transitions: ADO work-item label transitions reach parity in V1` — the other known gap); fda1e742 completed 05:55:18–06:00:49. Workflow digest **differs** between the 05:53 failure and the 05:55 success (sha256:9319132c… → sha256:35c9b6b9…) — the workflow was edited in between, presumably dropping the transition read.

**provider_mutations for the successful run — 2 rows total:**
```
seq 2  ado branch goobers/tb-ado-implementation/fda1e742... (create)   runner_json: (empty)
seq 72 ado branch ...fda1e742... operation=delete
       runner_json: {"operation":"delete","outcome":"unnecessary","reason":"pull-request-opened"}
```
Correlation verdicts:
- **Recorded → landed:** branch create landed (ref exists). The seq-72 "delete" row is honest *only in runner_json* — outcome `unnecessary` (skipped because a PR was opened); the bare `operation=delete` column misleads any consumer that doesn't parse runner_json. Wire agrees: branch still exists. (For the 6 failed runs, scratch-branch create+delete pairs all landed — repo shows only 2 heads.)
- **Landed → not recorded (the gap):** five wire mutations by run fda1e742 have **no provider_mutations row**: WI 1456 claim comment (rev 3), `goobers:claimed` tag (rev 4), `goobers/status:in-review` tag (rev 5), close-out comment (rev 6), **PR 359 creation**, and **PR status id 1 creation**. Six, counting both comments. `ready_claims` has no row for item 1456 (only GitHub-numeric items from other gaggles); `ready_label_transitions` has nothing ADO; stage_attempts runner_json is empty for open-pr/report-status/close-out; span_events are disk/model metrics only. **On ADO, the mutation ledger captured branch ops only — work-item and PR mutations are invisible to telemetry.** The seq gap (2→72) shows sequence numbers were consumed by unlogged events.
- Unattributable wire write: WI 1456 rev 2 (trust+ready tags, 05:55:23.503) matches no telemetry record *and* no seeding record — see §2 caveat.

## Requirements for a working ADO merge-review workflow (wire-derived)

**Stage viability on ADO, as evidenced:**
- *Works today:* claim via WIQL + comment (rev 3), branch push, PR creation, report-pr-status (native PR status landed with our genre/context), ci-poll against a build-policy pipeline (build 579 + evaluation `approved`), tag-based close-out/park.
- *Fails closed (known, confirmed verbatim):* backlog curation/reconcile (BL-033), ready-label transition reads (V1 parity item) — any merge-review stage depending on label-transition history cannot run on ADO yet.
- *Silent no-ops to fix:* work-item ArtifactLink never created (kills native PR↔WI machinery); claimed-tag written at close-out instead of claim (the claim's only durable marker for 5 minutes is a comment — a competing claimer scanning tags would not see the claim); telemetry blind to WI/PR mutations.

**apply-verdict vs report-pr-status on ADO:** report-pr-status (proven) publishes `genre:"goobers", name:"validation"`, state succeeded/failed/pending — but it is only gate-effective if the customer configures a **Status policy** on main for `goobers/validation` (none exists on this repo; today the status is decorative and the only gate is the Build policy). apply-verdict must additionally: publish **iteration-scoped** statuses (ours is `iterationId:null`, so a post-verdict push does not invalidate the verdict — a stale-approval hole GitHub's dismiss-on-push covers natively), set `targetUrl` to evidence, and for GitHub-style "request changes" semantics either cast a reviewer **vote** (-5/-10, requires the PAT identity as reviewer, `vote` API) or rely on the status policy alone. There is no label path: PR labels are empty/unused; verdict park-markers live on the work item as tags.

**merge-pr on ADO must be:** `PATCH pullRequests/{id}` with `status:"completed"`, `lastMergeSourceCommit` (concurrency token, here 71b45eb4) and explicit `completionOptions`: `mergeStrategy` (noFastForward | squash | rebase | rebaseMerge — repo policy may restrict; none does here), `deleteSourceBranch` (the branch survives today; ADO refuses pre-completion deletion of an active PR's source branch, which is exactly why the runner skipped it — `reason:"pull-request-opened"`), `transitionWorkItems` (**would do nothing on this PR**: 0 linked work items — the ArtifactLink gap means ADO cannot close WI 1456 on merge; our own close-out must keep doing it, or open-pr must start linking), and optionally `bypassPolicy` (needs elevated permission; otherwise merge requires policy evaluation 9 `approved` and within its 720-min validDuration). Auto-complete is the ADO-native "merge when green": `PATCH` with `autoCompleteSetBy` + completionOptions, after which ADO completes the PR the moment blocking policies pass — the natural mapping for our merge-queue-less flow, and currently unset. Merge requirements on this repo, concretely: Build policy def 28 green (satisfied), 0 reviewers, no status policy — so a working gate for Goobers verdicts requires the customer to add a Status policy (`goobers/validation`) and us to publish it iteration-scoped.

**Cross-cutting:** all writes appear as the PAT owner's personal identity ("Mason Allen") — PR author, status creator, WI commenter are indistinguishable from the human; `System.State` stays "New" through the whole lifecycle (dashboards keyed on state see nothing); tag vocabulary is project-global and grows implicitly (`goobers:claimed`, `goobers/status:in-review` now exist forever); consumers of PR statuses must handle absent `state` keys (`notSet`) and third-party genre interleaving.