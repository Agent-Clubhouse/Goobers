# Domain: site-runs

## Summary
goobers-site's entire life spans 2026-08-05T03:52Z to 2026-08-08T18:47Z (4,246 runs; 3,961 failed = 93.3%). Its history is a strict sequence of four failure regimes, each unmasking the next: (1) clone 403 "Write access to repository not granted" (933 failures, Aug 5 03:52Z–Aug 6 02:45Z) from launching with a token the config commit itself admitted had no repo access; (2) relative GIT_ASKPASS exec failure on fetch (2,501 failures = 63% of all failures ever, Aug 6 02:49Z–Aug 8 00:59Z) — upstream bug #2518, fixed by PR #2521, but the daemon ran a pre-Aug-2 binary until a restart at Aug 8 01:42Z; (3) fine-grained-PAT check-runs 403 => github_auth_failed (490 stage + 496 scheduler failures, Aug 8 05:25Z–18:02Z), which began the moment the gaggle opened its first PR and ended when a restart at ~18:09Z deployed a build containing the supposedly never-run #2685 Actions-API fallback (proven by /actions/runs?head_sha entries in the daemon's api-read-cache); (4) two instance-config bugs in the final hour — the K4 global github:pr:review override (6 apply-verdict 404s, fixed live at 18:36Z) and a missing siblingOverlapBudget param that killed 4 of 5 pr-remediation runs and was STILL FAILING at 18:47:31Z, its committed fix (2e489f8) never having run. K2's rate-limit attribution for the ~2600 executor_errors is wrong — exactly 2 rate-limited stage failures exist in the gaggle's history; the real quota damage was Aug 5's shared-token 404-polling starving the MAIN gaggle's scheduler. The operator's "never worked" belief is precisely true for the first ~74 hours (zero work products) and false for the last 13: 10 PRs opened (#224–#233), 43 issues claimed, and 1 PR (#225) merged at 18:42:59Z — five minutes before final shutdown.

## Findings

### [HIGH/confirmed] askpass-k3-resolved
**Claim:** K3 resolved: the askpass script existed before the first run ever (birth Aug 4 20:51:27, created at daemon start) and never went missing. The failure is the pre-LR-1 daemon binary passing GIT_ASKPASS as a path relative to the instance root ('gaggles/goobers-site/workcopies/auth/goobers-askpass.sh'); git resolves it against the child cwd, so clone (cwd=instance root) succeeded once at Aug 5 19:48:11 local while every subsequent fetch (cwd=repo.git) failed — 2,501 failures, 63% of all failures ever. The bug was latent instance-wide since launch but only the private site repo triggers it (public Agent-Clubhouse/Goobers never prompts for credentials). Upstream issue #2518 / PR #2521 (merged 2026-08-07T05:08Z) fixed it; the class ended at the 2026-08-08T01:42Z restart.

**Evidence:** stat -f '%SB' .../workcopies/auth/goobers-askpass.sh => birth Aug 4 20:51:27; repo.git birth Aug 5 19:48:11 == first query-backlog success 2026-08-06T02:48:11Z. Error text in run a9ab984ea2e168a401f22565d0d1c66c events.jsonl. Fix: cmd/goobers/runnerwiring.go:1977 (filepath.Abs, landed in 14499957 LR-1 2026-08-02) + gh issue #2518/PR #2521. Counts: 1927 implementation + 381 merge-review + 193 backlog-curation with 'cannot exec .../goobers-askpass.sh' in type=error events.

**Fix locus:** upstream-runtime  |  **Related:** #2518, #2521, #2302 (LR-1), K3

### [HIGH/confirmed] k1-fallback-did-run
**Claim:** K1 correction: the #2685 Actions-API fallback HAS run in production. The ~18:09Z Aug 8 restart deployed a build containing it (branch commit 86ad1f70 authored 10:30:47 local, 39 min before the restart): github_auth_failed vanished after 18:10Z with the token file unchanged since Aug 5, and the daemon's API cache holds GET /repos/masra91/Goobers-Site/actions/runs?head_sha=... entries (the exact endpoint the fix adds) stored at 18:47Z. All 10 PRs and the single merge happened under this unmerged-branch binary — if the daemon is next rebuilt from origin/main (which lacks #2685), the check-runs-403 class returns immediately against the 9 still-open PRs.

**Evidence:** python: json.load('scheduler/api-read-cache.json') entries matching 'actions/runs' with storedAtUnix 1786214844 (=2026-08-08T18:47:24Z); goobers-site-repo-token mtime Aug 5 09:21:10; last github_auth_failed 18:02:23Z, first post-restart query-backlog success 18:10:55Z; git log goobers/implementation/2685 => 86ad1f70 2026-08-08 10:30:47 -0700; restart gap 18:03:14→18:09:31Z.

**Fix locus:** deployment  |  **Related:** #2685, 86ad1f70, K1

### [HIGH/confirmed] k2-rate-limit-misattribution
**Claim:** K2 correction: the ~2,600 executor_error failures in query-backlog/pr-select (2,614 + 597 in stage_attempts) are git clone/fetch failures (clone-403 + askpass + DNS), not rate-limit cascades. Exactly 2 stage failures in the gaggle's entire history carry github_rate_limited (both 2026-08-08T16:55Z, quota reset 17:19:21Z). The auth-failed retry burn did exhaust the site token's quota once on Aug 8 (16:55→17:19Z, 17 scheduler demand-count rate-limit errors), but the cascade magnitude in K2 is off by three orders.

**Evidence:** grep -rl github_rate_limited gaggles/goobers-site/runs --include=events.jsonl | wc -l => 2 (runs 0eabfd14dcb7e16b5e80c85f70975f7e, c367e025dd0e82a6b5437ec157ef8428). Journal type=error messages of all 3,487 executor_error attempts classified: 2,501 askpass + 933 clone-403 + 48 DNS + 5 other. scheduler_errors: 17 rows LIKE '%rate limited%' on 2026-08-08.

**Fix locus:** process  |  **Related:** 788d6768, #2587, K2

### [HIGH/confirmed] sibling-overlap-budget-live-at-shutdown
**Claim:** The only failure class still live in the gaggle's final minute: pr-remediation's remediation-checkpoint hard-fails with provider_error 'siblingOverlapBudget must be a positive integer, got ""' because the goobers-site pr-remediation.yaml (hand-copied from the goobers gaggle's, which sets "2") omitted the param. 4 of 5 pr-remediation runs ever failed on it; last failure 18:47:31Z. The fix (siblingOverlapBudget: "3", commit 2e489f8 at 19:01Z) landed AFTER the daemon stopped and has never executed — first thing to verify on next start. VISION.md's validation wish names this exact bug shape ('a budget input a stage will unconditionally require but the workflow never sets').

**Evidence:** Runs 25432fafac06c9cc79b3c39d348a4a20, 4ff82a2bc4d93e4abdb9ab7aa80c9ded, 88f7cdf968526111c61cc0f1425e7367, 0b9a10fe6632e2d99e3863ef54bf9004 (failures 18:12–18:47:31Z). git show main:config/gaggles/goobers-site/workflows/pr-remediation.yaml:126-135 ('NEW 2026-08-08: siblingOverlapBudget was missing here'); goobers counterpart line 384 sets "2". git log main -S siblingOverlapBudget -- config/gaggles/goobers-site/ => only 2e489f8 (2026-08-08 12:01:44 -0700, after last run 18:47Z).

**Fix locus:** dsl-surface  |  **Related:** 2e489f8, VISION.md 'humming' bullet 2

### [MEDIUM/confirmed] stale-binary-deployment
**Claim:** The daemon that launched goobers-site ran a binary predating 2026-08-02: it lacked both LR-1's filepath.Abs (Aug 2) and #2521 (merged Aug 7 05:08Z). The askpass fix was live upstream ~20 hours before the operator's Aug 8 01:42Z restart deployed it — the instance burned ~1,000 additional failed runs against an already-fixed bug. A restart on Aug 6 20:34 local (with old binary, new config) predictably did not help.

**Evidence:** Askpass-class failures persist across the 2026-08-07T03:34→04:19Z restart gap (977 implementation failures on Aug 7 UTC) but stop exactly at the 2026-08-08T00:59:44→01:42:26Z gap. PR #2521 mergedAt 2026-08-07T05:08:41Z (gh search prs). git log -L1977,1977:cmd/goobers/runnerwiring.go => 14499957 2026-08-02.

**Fix locus:** deployment  |  **Related:** #2521

### [MEDIUM/confirmed] cross-gaggle-quota-starvation
**Claim:** The site gaggle's real quota damage was to the HEALTHY gaggle: on Aug 5 (site day 1, per-minute crons, shared dev5 token), the scheduler's demand polling of masra91/Goobers-Site 404'd 538 times and the same token's org-repo demand counting got rate-limited 491 times (vs 7 total scheduler errors on Aug 4) — the misconfigured site gaggle degraded Agent-Clubhouse/Goobers scheduling for three days (491/180/79 rate-limited org queries Aug 5/6/7, ~0 after the site got its own token).

**Evidence:** SQL: SELECT substr(occurred_at,1,10), SUM(message LIKE '%Goobers-Site%'), SUM(message LIKE '%Agent-Clubhouse/Goobers%'), COUNT(*) FROM scheduler_errors WHERE occurred_at>='2026-08-04' GROUP BY 1 => Aug4: 0/7/7, Aug5: 538/491/1029, Aug6: 247/180/427, Aug7: 162/79/245, Aug8: 496/5/501. First site 404 at 2026-08-05T03:52:17.96Z, before the first run row.

**Fix locus:** instance-config  |  **Related:** K2, K4 (shared-credential blast radius)

### [MEDIUM/confirmed] launch-with-known-bad-credential
**Claim:** Phase 1 was a known-at-commit-time gap shipped anyway: config commit 137d7b4 (Aug 4 11:58) added the gaggle with dev5-github-token while its own comment states the token '404s on this repo today... must be widened before this gaggle's workflows can actually run', combined with per-minute implementation cron. Result: 933 clone-403 failures ('remote: Write access to repository not granted') over ~23h. The dedicated token file was created Aug 5 09:21 local but the live instance.yaml swap only took effect at 19:48 local — a 10.5-hour lag, and the commit (14d06d1) came another 25h later.

**Evidence:** git -C goobers-instances show 137d7b4:instance.yaml (comment block); diff 137d7b4..14d06d1 -- instance.yaml ('UPDATED 2026-08-05: ... every run 403'd on git clone'); token birth: stat ~/.goobers-secrets/goobers-site-repo-token => Aug 5 09:21:08; first clone success == repo.git birth Aug 5 19:48:11; full 403 message in run 68fee04f2dc26d8922fd91c6d6899319 events.jsonl.

**Fix locus:** process  |  **Related:** 14d06d1, 137d7b4

### [MEDIUM/confirmed] k4-apply-verdict-404
**Claim:** K4 quantified and time-boxed: the instance-wide github:pr:review override produced exactly 6 apply-verdict provider_error 404s (POST /pulls/{224,225,...}/reviews with dev5-github-token), 18:12:01–18:31:31Z — the class was masked for the gaggle's whole prior life because earlier regimes prevented merge-review from ever reaching apply-verdict. The operator's live removal of the override took effect at the 18:36Z restart (askpass-script mtime 11:36:25 local marks it): apply-verdict succeeded at 18:42:42Z and 18:47:20Z, enabling the gaggle's only merge; commit 2e489f8's removal comment documents internal/credentials/scoping.go RunnerGrants as the instance-wide mechanism.

**Evidence:** SQL: SELECT run_id,status,started_at FROM stage_attempts WHERE stage='apply-verdict' (runs dae016bdfbedf3fd..., 2db59f06f50b...; 6 failures then 2 successes). 404 body in run 2db59f06f50b9e0d6c3de3ebb2f5b6db events.jsonl. git diff 60320be 2e489f8 -- instance.yaml (REMOVED 2026-08-08 comment). Restart marker: stat goobers-askpass.sh mtime Aug 8 11:36:25.

**Fix locus:** instance-config  |  **Related:** K4, 2e489f8, #870

### [MEDIUM/confirmed] telemetry-observability-gap
**Claim:** 87% of the gaggle's failures (3,487/3,987) are indistinguishable in SQL: error_code='executor_error', error_class='unknown', runner_json empty, and no stage.finished failure event in the journal — the message exists only in a type=error journal event. Clone-403, askpass, and DNS failures (three different root causes needing three different owners) share one opaque bucket; per-run journal parsing was required to build any taxonomy. error_class is 'unknown' for 100% of goobers-site failures including provider-classified ones.

**Evidence:** SQL: SELECT error_code,error_class,COUNT(*) FROM stage_attempts sa JOIN runs r USING(run_id) WHERE r.gaggle='goobers-site' AND sa.status='failure' GROUP BY 1,2 => executor_error/unknown 3487, github_auth_failed/unknown 488, etc.; runner_json empty for all 3487. Journal contrast: run a9ab984ea2e168a401f22565d0d1c66c has type=error but no stage.finished for the failed stage.

**Fix locus:** upstream-runtime  |  **Related:** 

### [MEDIUM/confirmed] config-runs-ahead-of-git
**Claim:** Three times in four days the RUNNING config diverged from git main for hours-to-days: the credential swap was live Aug 5 19:48 but committed Aug 6 20:29 (14d06d1); the upstream-sync workflow ran Aug 7 19:38 local (runs 2d1001f2, and the completed sibling) but was committed Aug 8 12:01 (2e489f8, whose message says 'add upstream-sync workflow'); the github:pr:review override removal was live 18:36Z, committed 19:01Z. Config-history correlation for this instance MUST use restart markers and journals, not commit timestamps — git main understates what actually ran.

**Evidence:** upstream-sync runs exist at 2026-08-08T02:38Z/03:xxZ (telemetry runs table) vs git log main: workflow file first appears in 2e489f8 2026-08-08 12:01:44 -0700. Clone success 2026-08-06T02:48:11Z vs 14d06d1 2026-08-06 20:29:08 -0700. apply-verdict success 18:42:42Z vs 2e489f8 19:01Z.

**Fix locus:** process  |  **Related:** memory: instance-config-is-drifted-fork

### [LOW/confirmed] one-off-failures
**Claim:** Residual singletons, all explained: (a) upstream-sync's upstream-churn-data failed missing_result_file because a run.script stage referenced $GOOBERS_ADDITIONAL_REPO_GOOBERS, which the runner never injects for sh-invoked stages — the committed redesign removes the deterministic stage entirely (never yet run, manual-trigger only); (b) one local-ci nonzero_exit at 18:44Z is a genuine agent-code CI failure (quality gate engaging correctly); (c) worktree_remove_failed (18:53Z) and local-ci signal:interrupt (18:55:50Z) are daemon-shutdown artifacts; (d) two implementation runs remain phase='running' in read.db (702f16c7d61ab0f88588c400f94be7b7 at local-ci, fc398ba38c4eb189c139cb8926590778 at implement) — stale state to reconcile on restart.

**Evidence:** Run 2d1001f21ac2af1b38c7c77d970ddb13 events.jsonl: 'declared result file "upstream-churn-report.json" was not produced (exit code 1); stderr: sh: line 1: GOOBERS_ADDITIONAL_REPO_GOOBERS: unbound variable'; main:config/gaggles/goobers-site/workflows/upstream-sync.yaml header lines 19-33 documents the gating (internal/executor env.go injectRunContext). read.db: SELECT run_id,current_stage FROM run WHERE gaggle='goobers-site' AND phase='running'.

**Fix locus:** instance-config  |  **Related:** 2e489f8

### [INFO/confirmed] failure-taxonomy-timeline
**Claim:** The gaggle's complete failure history is four sequential regimes, each masking the next: clone-403 (933 failures, Aug 5 03:52Z–Aug 6 02:45Z), relative-askpass fetch failure (2,501, Aug 6 02:49Z–Aug 8 00:59Z, incl. a 48-failure DNS-outage sub-window Aug 7 01:02–03:34Z), check-runs 403/github_auth_failed (490, Aug 8 05:25Z–18:02Z), then final-hour config bugs (6 apply-verdict 404s + 4 siblingOverlapBudget failures). Every regime boundary coincides with a daemon restart or live config/credential change, never mid-flight.

**Evidence:** SQL: SELECT substr(r.started_at,1,10),r.workflow,sa.stage,sa.status,sa.error_code,COUNT(*) FROM stage_attempts sa JOIN runs r ON r.run_id=sa.run_id WHERE r.gaggle='goobers-site' GROUP BY 1,2,3,4,5. Boundary runs: d3be1ab17fa7 (02:45:08Z CLONE-403) vs dd32365f038e (02:49:07Z ASKPASS). Restart gaps in scheduler_events: 2026-08-07T03:34→04:19Z, 2026-08-08T00:59:44→01:42:26Z, 18:03:14→18:09:31Z. First auth_failed 05:26:01Z = 1 min after first PR open (provider_mutations pr open #224 05:25:29Z).

**Fix locus:** process  |  **Related:** #2518, #2521, #2685, K1-K4

### [INFO/confirmed] never-worked-claim-tested
**Claim:** The operator's 'has never worked' belief is precisely true for the first ~74 hours and false for the last ~13.5: zero work products before 2026-08-08T05:20Z (no PR/issue/comment mutations, only branch-reservation bookkeeping), then 43 issues claimed by curation, 10 PRs opened (#224–#233), 8 close-outs, and PR #225 MERGED at 18:42:59Z — five minutes before final shutdown. Of 284 completed runs, 267 (94%) are disposition=no-work; only ~17 completed runs plus 2 'failed' runs did substantive work. Notably both first PR-opening runs (c9be28ca => #224, c88670fa => #225) are recorded status=failed (ci-poll /status 403), so the eventual merged deliverable came from a 'failed' run — run status alone undercounts delivered work.

**Evidence:** read.db: SELECT workflow,phase,disposition,COUNT(*) FROM run WHERE gaggle='goobers-site' GROUP BY 1,2,3. telemetry provider_mutations: kind='pr' opens #224 (05:25:29Z, run c9be28ca50a1358964129d39c4f6d155, status failed) through #233 (18:43:48Z); operation='merge' external_id=225; kind='issue' claim x43 from 05:20:54Z. Aug 5-7 mutations: kind='branch' only (1436/2762/2596 rows).

**Fix locus:** process  |  **Related:** 

### [INFO/hypothesis] restart-success-burst-anomaly
**Claim:** During the askpass era there were exactly 34 successful stage attempts, in two clusters: 1 at the initial clone (02:48Z Aug 6) and 33 in the hour immediately after the Aug 7 04:19Z daemon restart, after which fetch failures resumed. Mechanism unproven; consistent with a startup-time mirror refresh running under a cwd where the relative askpass resolves (or a fetch-freshness window), after which per-worktree fetches (cwd=repo.git) fail again. Worth a look in worktree.Manager's startup vs per-create fetch paths if #2521-style bugs recur.

**Evidence:** SQL: SELECT substr(sa.started_at,1,13),COUNT(*) FROM stage_attempts sa JOIN runs r USING(run_id) WHERE r.gaggle='goobers-site' AND sa.status IN ('no-work','success') AND sa.started_at<'2026-08-08T01:00' GROUP BY 1 => 2026-08-06T02: 1, 2026-08-07T04: 33 (nothing else). Restart gap ends 2026-08-07T04:19:47Z.

**Fix locus:** upstream-runtime  |  **Related:** #2518
