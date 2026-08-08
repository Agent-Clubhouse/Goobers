# Domain: deployment

## Summary
Deployment provenance is fully reconstructable and healthier than the audit's shared premise: the daemon builds roughly daily from the main checkout at /Users/masonallen/source/Goobers (make build -> bin/goobers), and the journal's daemon.started events give a complete binary timeline (12 versions since Aug 1, all but one built from origin/main). The last deployed binary 93f4098e (built Aug 8 10:47, ran 11:06-12:05 local) is only 4 commits behind origin/main and ALREADY CONTAINS both the #2685 Checks-API fallback (merged as PR #2689 / 5f07c22f at 10:46 today — K1 is stale) and the K2 circuit-breaker (788d6768 / PR #2688, merged 04:20 today); in its ~55 minutes of life github_auth_failed went to zero and goobers-site completed 8 implementation runs end-to-end plus its first PR merge. K3 (askpass) was root-caused, fixed (a3b2e636, daemon-authored PR #2521), deployed Aug 7 18:40, and empirically eliminated. The two residual goobers-site failure classes observed after the fix (apply-verdict 404 from the K4 global credential override; a siblingOverlapBudget:"" validation error mislabeled provider_error) are both already fixed in instance-config main commit 2e489f8 (12:01 today) that the daemon never ran. Net: every currently-known goobers-site failure class has a committed fix; the highest-leverage zero-new-code action is restart (optionally after pulling the 4 undeployed commits, two of which serve VISION wishes 5 and 6). The one relevant defect with no fix anywhere is caps: issue #2692 (maxOpenPRs unenforced for non-repos[0] gaggles), filed today, open, no PR.

## Findings

### [HIGH/confirmed] residual-failures-already-fixed-in-config-never-run
**Claim:** Both goobers-site failure classes that SURVIVED the fixed binary are already fixed in instance-config main commit 2e489f8 (Aug 8 12:01) which the daemon never meaningfully ran (clean shutdown ~12:05): (1) merge-review/apply-verdict failed 6x with 404 on POST /repos/masra91/Goobers-Site/pulls/224/reviews — the K4 global github:pr:review override routing dev5-github-token; the override is now removed (instance.yaml comment documents internal/credentials/scoping.go RunnerGrants instance-wide semantics); (2) pr-remediation/remediation-checkpoint failed 4x with 'siblingOverlapBudget must be a positive integer, got ""' — now set to "3" in the running-config branch. Combined with finding k1-correction, EVERY known goobers-site failure class has a committed fix; a daemon restart is the entire remaining deployment step.

**Evidence:** journal: gaggles/goobers-site/runs/dae016bdfbedf3fdcaa3751a8a8321d8/events.jsonl errorMessage 'submit native review for PR #224: POST .../pulls/224/reviews failed: status 404'; run 25432fafac06c9cc79b3c39d348a4a20 errorMessage 'siblingOverlapBudget must be a positive integer, got "'; git -C /Users/masonallen/source/goobers-instances show main:instance.yaml (REMOVED 2026-08-08 comment, lines 44-58); git -C ... show main:config/gaggles/goobers-site/workflows/pr-remediation.yaml lines 126-135; git -C ... log -1 main -- instance.yaml -> 2e489f8 08-08 12:01; reflog shows main ff to 2e489f8 at 12:02, daemon last events ~12:05

**Fix locus:** instance-config  |  **Related:** K4; #870 (self-review 422 degrade path)

### [MEDIUM/confirmed] deploy-lag-prolonged-askpass-storm
**Claim:** Deployment lag, not missing code, extended the askpass outage by ~20 hours: the fix merged Aug 6 22:08, but the binary deployed at Aug 6 21:18 (20b38eba, built 20:08) predated it by two hours and was left running until Aug 7 18:40 — accruing roughly 1,300 more executor_error failures on Aug 7 alone after the fix already existed on main. Same pattern risk applies generally: the instance runs main-built binaries with a 0.3-21h fix-to-deploy latency (askpass 20.5h; resume-burst 0f4552b5 7.5h; circuit-breaker 6.8h; checks-fallback 20min).

**Evidence:** commit dates: a3b2e636 2026-08-06 22:08, 0f4552b5 2026-08-07 11:11, 788d6768 2026-08-08 04:20, 5f07c22f 2026-08-08 10:46 vs daemon.started: 20b38eba 08-06 21:18, cfae81b4 08-07 18:40, 93f4098e 08-08 11:06; SQL daily failures gaggle='goobers-site': 2026-08-07|executor_error|1322

**Fix locus:** process  |  **Related:** #2518, #2593

### [MEDIUM/confirmed] undeployed-commit-inventory
**Claim:** Exactly 4 commits are merged-but-undeployed (93f4098e..origin/main). Two are directly relevant: c0167a1b '#2614 executor: expose additional repos to deterministic stages' (additional-repos class; VISION wish 5) and 6754aab9 '#2659 goobers-io MCP get_run_info tool' (run-identity class; VISION wish 6). The other two are inert for reliability: 05fe996a (#2690 portal run-detail digest) and 810cf9e5 (#2581 CLI reflection parity test). No rate-limit, caps, credentials, or site-403 code is waiting undeployed — those all shipped in 93f4098e.

**Evidence:** git log --oneline 93f4098e..origin/main -> c0167a1b, 6754aab9, 05fe996a, 810cf9e5; VISION.md wishes 5 ('Reference-repo access decoupled from workspace type — needs upstream') and 6 ('A real run-identity primitive — needs upstream') at /Users/masonallen/source/goobers-instances/VISION.md lines 136-152

**Fix locus:** deployment  |  **Related:** #2614, #2659, #2690, #2581; VISION wishes 5, 6

### [MEDIUM/confirmed] caps-bug-open-no-fix-exists
**Claim:** The caps failure class has NO fix anywhere — deployed, merged, branched, or PR'd: issue #2692 'maxOpenPRs is silently unenforced for every gaggle except repos[0]'s owner' was filed today (2026-08-08T18:45:56Z, by masra91) and is open with zero associated PRs or branches. This is VISION wish 7's 'runtime bug — filed upstream'. goobers-site (owner masra91, not repos[0] owner Agent-Clubhouse) is precisely the unprotected case, so on restart the site gaggle will run with maxOpenPRs silently ignored.

**Evidence:** gh issue view 2692 --json state,createdAt -> OPEN, 2026-08-08T18:45:56Z; gh pr list --state all --search 2692 -> []; VISION.md wish 7 line 153; unmerged-branch sweep (git branch -r + merge-base loop) shows no branch referencing 2692

**Fix locus:** upstream-runtime  |  **Related:** #2692; VISION wish 7

### [LOW/confirmed] dirty-nonmain-build-ran-in-prod
**Claim:** One binary in the audit window was built from uncommitted local changes on a side branch: f85e7c35-dirty ran Aug 1 18:32-20:59 (tip of origin/fix/preflight-exit-code-check, atop the later-reverted #2203 external github-mcp-server work; superseded by revert 4df9a9bf at 20:59). Four more -dirty builds ran Jul 26-27. These binaries are not exactly reconstructable from any git ref — a provenance-hygiene gap, though bounded: since Aug 2 every deployed build has been a clean origin/main ancestor.

**Evidence:** journal daemon.started 2026-08-01T18:32:37 version 'f85e7c35-dirty'; git branch -r --contains f85e7c35 -> only origin/fix/preflight-exit-code-check; git merge-base --is-ancestor f85e7c35 origin/main -> no; parent 30bbcd95 = #2203; 4df9a9bf 'Revert #2199/#2203/#2204' deployed 20:59; July dirty versions: ebf8ace9-dirty, fe6a2d63-dirty, 453e283f-dirty, 1d42e809-dirty

**Fix locus:** process  |  **Related:** #2203, revert 4df9a9bf

### [LOW/confirmed] per-run-binary-provenance-gap
**Claim:** run.yaml records workflowDigest/gooberDigest/workflowVersion but NOT the daemon binary version, and telemetry.db's scheduler_events ingests no daemon lifecycle events (no daemon.started/version rows; only trigger/run/claim/tick/error types) — so attributing a given run to a binary requires a time-join against journal daemon.started events. Adding the build version to run.yaml (or ingesting lifecycle events into telemetry) would make provenance queries first-class. The journal's daemon.started payload also normalizes instanceRoot to an absolute path, which masked the very launch-CWD difference that triggered K3 (argv/CWD are not recorded).

**Evidence:** gaggles/goobers-site/runs/ffed6c6c020d254282793e9c481ff6d5/run.yaml (fields: gooberDigest, workflowDigest, workflowVersion; no binary version); SQL: SELECT type,count(*) FROM scheduler_events GROUP BY type -> no lifecycle types; journal daemon.started for the Aug 5 19:47 dirty_restart still shows instanceRoot '/Users/masonallen/source/goobers-instances' despite the relative-root launch behavior fixed by a3b2e636

**Fix locus:** upstream-runtime  |  **Related:** a3b2e636

### [INFO/confirmed] deployed-binary-identity
**Claim:** The daemon binary for the final session was built from 93f4098e (origin/main minus 4 commits), and a complete per-session binary timeline exists in the journal: 2d904612 -> 4292a8e0 -> f85e7c35-dirty -> 4df9a9bf (Aug 1-3) -> 7dae0bc8 -> 34359cf4 -> 7f446c1f (Aug 3) -> ff1d890b (Aug 4) -> 00304bea (Aug 4-6) -> 20b38eba (Aug 6-7) -> cfae81b4 (Aug 7-8) -> 93f4098e (Aug 8 11:06-~12:05). The build source is the clean main checkout /Users/masonallen/source/Goobers (currently at 93f4098e, behind origin/main by 4), so 'git pull && make build' is the exact upgrade path.

**Evidence:** cat /Users/masonallen/source/goobers-instances/scheduler/up.lock -> version "93f4098e ... built 2026-08-08T10:47:47-07:00", startedAt 2026-08-08T18:36:12Z; grep '"daemon.started"' /Users/masonallen/source/goobers-instances/scheduler/events.jsonl (each event carries runner.version + instanceRoot); /Users/masonallen/source/Goobers/bin/goobers version -> 'goobers 93f4098e'; git -C /Users/masonallen/source/Goobers status -sb -> '## main...origin/main [behind 4]' clean; git rev-list --count 93f4098e..origin/main -> 4

**Fix locus:** deployment  |  **Related:** 

### [INFO/confirmed] k1-correction-fallback-merged-deployed-validated
**Claim:** K1 is stale: the #2685 Actions-API fallback WAS merged to origin/main today (PR #2689, head branch goobers/implementation/2685, squash commit 5f07c22f, merged 2026-08-08T17:46:55Z) and WAS deployed and validated — the fixed binary 93f4098e started 18:06:47Z; the last goobers-site github_auth_failed is 18:02:23Z (zero after), and 8 goobers-site implementation runs then completed end-to-end (query-backlog through close-out, 8 open-pr successes) plus one merge-review that merged a PR — the first working goobers-site pipeline ever.

**Evidence:** gh pr view 2689 --json state,mergedAt,mergeCommit,headRefName -> MERGED 2026-08-08T17:46:55Z, mergeCommit 5f07c22f, head goobers/implementation/2685; git merge-base --is-ancestor 5f07c22f 93f4098e -> yes (and NOT in cfae81b4); SQL: SELECT sa.run_id,sa.started_at FROM stage_attempts sa JOIN runs r USING(run_id) WHERE r.gaggle='goobers-site' AND sa.error_code='github_auth_failed' AND sa.started_at>='2026-08-08T18:00' -> last 18:02:23Z; SQL: SELECT r.workflow,r.status,sa.stage,sa.status,count(*) FROM runs r LEFT JOIN stage_attempts sa USING(run_id) WHERE r.gaggle='goobers-site' AND r.started_at>='2026-08-08T18:06:47' GROUP BY 1,2,3,4 -> implementation|completed 8x all stages success, merge-review merge-pr success 1

**Fix locus:** deployment  |  **Related:** #2685, PR #2689, branch goobers/implementation/2685 (tip 86ad1f70, now merged as squash — deletable)

### [INFO/confirmed] k2-circuit-breaker-merged-deployed
**Claim:** The K2 rate-limit-storm defect also has its fix merged and deployed in the same final binary: commit 788d6768 (PR #2688, merged 2026-08-08T11:20:58Z) adds an auth circuit-breaker (internal/localscheduler/scheduler.go +55, providers/transient.go +29, authcircuit2687_test.go). It is in deployed 93f4098e but was NOT in any binary that ran during the Aug 5-8 storm (first present after the 11:06 local restart).

**Evidence:** git show --stat 788d6768; gh api repos/Agent-Clubhouse/Goobers/commits/<sha>/pulls -> PR #2688 merged_at 2026-08-08T11:20:58Z; git merge-base --is-ancestor 788d6768 93f4098e -> yes; NOT ancestor of cfae81b4

**Fix locus:** upstream-runtime  |  **Related:** #2687, PR #2688

### [INFO/confirmed] k3-askpass-root-cause-verified-and-fixed
**Claim:** K3 verified and closed: the askpass exec failures were caused by GIT_ASKPASS being built from a RELATIVE workcopies root (resolved against the git subprocess CWD), fixed by a3b2e636 (cmd/goobers/runnerwiring.go: buildWorktreeGitEnv now takes absoluteWorkcopiesRoot; daemon-authored PR #2521, closes #2518, merged Aug 6 22:08). The 2500-event storm is exactly bounded by launch context, not code change: it began 2026-08-05T19:49:15, two minutes after a dirty_restart of binary 00304bea at 19:47:10 — the SAME binary had run cleanly since Aug 4 20:55 — and ended at the Aug 7 17:59:20 shutdown; zero occurrences after cfae81b4 (first binary containing the fix) started 18:40:36.

**Evidence:** grep -c 'goobers-askpass.sh.*No such file' scheduler/events.jsonl -> 2500; first hit 2026-08-05T19:49:15, last 2026-08-07T17:59:20; daemon.started events: 00304bea started 08-04T20:55 and again (dirty_restart) 08-05T19:47:10; git show a3b2e636 -- cmd/goobers/runnerwiring.go (workcopiesRoot -> absoluteWorkcopiesRoot at ~line 1996); git merge-base --is-ancestor a3b2e636 cfae81b4 -> yes, not in 20b38eba

**Fix locus:** upstream-runtime  |  **Related:** #2518, PR #2521

### [INFO/confirmed] no-other-relevant-unmerged-work
**Claim:** No other unmerged branch or open PR carries fixes relevant to the observed failure classes: the 6 open PRs are external/feature work (2679 docs, 2412 WebGL, 2405 gaggle memory, 2364 Windows CI, 2242 monitoring, 2222 Gitea), and the ~40 unmerged goobers/* agent branches are stale (newest besides the merged 2685 branch is Aug 1). Branch goobers/implementation/2685 (tip 86ad1f70) is fully superseded by squash-merge 5f07c22f and is safe to delete. The instances-repo working tree being on mason/dsl-2.1-wishlist is post-hoc: reflog shows the checkout moved off main only at 12:15-12:21 today, after the daemon's final config read window, so 'main = what ran' holds.

**Evidence:** gh pr list --state open --json number,title; unmerged-branch sweep saved at tool-results/bdk8v2aop.txt; goobers/* branch dates via git log -1 per branch (newest non-merged content 08-01); git -C /Users/masonallen/source/goobers-instances reflog --date=iso -> checkout main->wishlist at 2026-08-08 12:21:18, daemon clean_shutdown ~12:05

**Fix locus:** unknown  |  **Related:** PR #2689
