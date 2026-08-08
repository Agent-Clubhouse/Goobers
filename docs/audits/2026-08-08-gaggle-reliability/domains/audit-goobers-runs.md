# Domain: goobers-runs

## Summary
The goobers gaggle's 97% completion rate (21,026/21,795 since Aug 1) hides four compounding instability layers in its 765-run failure/escalation tail. (1) GitHub API quota: both gaggles' PATs belong to one GitHub user (ID 1669494 in error text; identical X-RateLimit-Reset timestamps across gaggles), so the shared 5,000/hr bucket is exhausted hour after hour — 43% (327/765) of goobers' failed/escalated runs carry github_rate_limited, chronic since Jul 16 and amplified since Aug 5 by goobers-site's un-circuit-broken auth-fail loop (K2). (2) pr-remediation's local-ci was left on the engine's 10m default timeout when implementation.yaml was bumped to 1800s on Aug 3; `make ci` now takes >12m, so local-ci has gone 0-for-106 in August (304 successes in July), which alone produces 79 of the 144 pr-remediation escalations. (3) Escalation parking is not sticky — any base-branch advance un-parks a PR — so deterministically-doomed PRs churn: the 144 escalations map to just 48 PRs, with PR #2364 escalated 41 times in 5 days. (4) Repass waste is both unbounded (one run did 16 implement sessions over 4.75h despite DefaultMaxRepasses=3) and invisible (read.db repass_count is 0 for every run ever recorded); 71% of Aug model spend ($1,100 of $1,551) went to runs that did not complete. Secondary layers: rate-limited provider stages strand runs until the 45m stall reaper mislabels them "escalated", a one-hour Aug 7 DNS outage produced 72 failed merge-review runs, and an Aug 2-3 red-baseline episode produced 178 local-ci failures. Merge-review-critical and genuine dependency-blocked escalations look healthy.

## Findings

### [CRITICAL/confirmed] github-quota-shared-user
**Claim:** Both gaggles' tokens authenticate as the same GitHub user, so they share one 5,000/hr REST quota; goobers-site's failure storm and goobers' own every-minute polling exhaust it hour after hour, and this single mechanism accounts for 43% of the healthy gaggle's failed/escalated runs since Aug 1 (select stages, gathers, open-pr, park stages, and scheduler demand-counting all fail together).

**Evidence:** SQL: SELECT COUNT(DISTINCT re.run_id) FROM run_errors re JOIN runs r ON r.run_id=re.run_id WHERE r.gaggle='goobers' AND r.started_at>='2026-08-01' AND r.status IN ('failed','escalated') AND re.message LIKE '%rate limited%' => 327 of 765. Same reset timestamp in both gaggles' errors on Aug 8: 'resets at 2026-08-08T17:19:21Z'. Hourly exhaustion Aug 8: distinct resets 08:18:56Z through 17:19:21Z, one per hour. User named in error: 'API rate limit exceeded for user ID 1669494' (run_errors, gather-ci-history). scheduler_errors code=schedule_demand_count_failed: 851 rate-limited on Agent-Clubhouse/Goobers pulls, 643 on masra91/Goobers-Site, 1029 on 2026-08-05 alone. instance.yaml (main): goobers-site token is 'same masra91 identity'. Chronic pre-goobers-site: rate-limited run_errors by day — Jul 16: 412, Jul 22: 205, Aug 3: 801 (goobers-site gaggle only added Aug 4).

**Fix locus:** upstream-runtime  |  **Related:** K2, K4, #2685 (goobers-site 403 loop), operator's Aug 7 pr-remediation */5 cadence cut

### [CRITICAL/confirmed] pr-remediation-localci-timeout
**Claim:** pr-remediation's local-ci stage was left on the engine's 10-minute default timeout when the same fix was applied to implementation.yaml on Aug 3; `make ci` now takes >12 minutes, so pr-remediation local-ci has NEVER succeeded in August (0/106: 92 timeout, 14 nonzero_exit) and the entire substantive remediation lane is dead — only the mechanical update-behind-pr lane completes.

**Evidence:** SQL: stage_attempts for gaggle='goobers', workflow='pr-remediation', stage='local-ci', started_at>='2026-08-01' => failure/timeout 92 + failure/nonzero_exit 14, zero success (July: 304+15 successes). All timeouts exactly 10 min. Default: internal/boundedwait/boundedwait.go:15 DefaultTimeout = 10*time.Minute. implementation.yaml has timeoutSeconds:1800 ('Bumped from the 10m executor default ... 2026-08-01', instance repo main, commit 5dd72f2 2026-08-03); pr-remediation.yaml local-ci (line 591-598 on main) has no timeoutSeconds. Suite duration evidence: 'test (elapsed 13m18.572s)' in run cb428087b23b468148fb8464db4c7a0a's local-ci artifact. 86 runs hit the timeout since Aug 1; 79 escalated (55% of the 144 pr-remediation escalations).

**Fix locus:** instance-config  |  **Related:** instance commit 5dd72f2; memory: shared-host load false-red timeouts; #2368 (test-integration-strict)

### [HIGH/confirmed] escalation-park-not-sticky
**Claim:** A parked (escalated) PR is automatically un-parked as soon as the base branch tip moves (escalationStillBlocks returns false), and in a repo merging dozens of PRs daily this re-feeds deterministically-failing PRs into remediation indefinitely: the 144 pr-remediation escalations since Aug 1 map to only 48 distinct PRs, and PR #2364 alone was escalated 41 times in 5 days, burning agent sessions each cycle until a human applied goobers:needs-human.

**Evidence:** cmd/goobers/remediationcheckpoint.go:396-406 — 'if head != pr.HeadSHA { return false }' and 'if base != liveBaseTip { return false }' (un-block on any base advance). Journal scan of the 144 escalated runs' selectedNumber outputs: 48 distinct PRs / 142 mapped runs; PR #2364: 41, #2574: 6, #2471/#2349/#2256: 5 each. gh pr view 2364 --repo Agent-Clubhouse/Goobers => still OPEN, created 2026-08-03, now labeled goobers:needs-human. gatherprcontext.go:399-404 skips LabelNeedsHuman but the remediation-escalated label alone does not stick once base moves. No re-escalation generation counter exists in the checkpoint state.

**Fix locus:** upstream-runtime  |  **Related:** #1855 (restored the exit), memory: pr-remediation identical-diff re-escalation loop

### [HIGH/confirmed] finding-responses-contract-collision
**Claim:** validate-finding-responses requires the implementer's findingResponses count to equal the ORIGINAL merge-review verdict's findings count, but on review-gate repasses the implementer (reasonably) documents responses to the IN-RUN reviewer's findings; on failing-ci-cause remediations the verdict is nil so any response fails 'want exactly 0'. 96 stage failures since Aug 1 (90 of the all-time 152 are the want-exactly-0 shape), each burning a full implement cycle and feeding the escalation loop.

**Evidence:** cmd/goobers/respondtofindings.go:291 'contains %d response(s), want exactly %d' where findings come from brief.GatherPRContext.Verdict (nil => 0). Replayable case: run 060a2f067ed0be7411b30dd53a064629, PR #2364 — rebase-pr outputs remediationCauses='failing-ci,sibling-overlap' (no verdict findings), second implement pass emitted 1 response ('Installed the fake Copilot fixture...') answering the in-run reviewer, validator failed it. Message distribution: SELECT message, COUNT(*) FROM run_errors WHERE code='finding_responses_invalid' GROUP BY message => 'want exactly 0' variants 90, count-mismatch variants ~54, 'decode JSON array: unexpected end of JSON input' 8.

**Fix locus:** upstream-runtime  |  **Related:** #1342 (validate stage), #1236 (auditable remediation responses)

### [HIGH/confirmed] rate-limit-strands-runs
**Claim:** When a provider stage errors with github_rate_limited, some runs record the error event and then make no further journal progress — no failure, no retry — holding a concurrency slot until the 45-minute stalled-run reaper kills them and marks them 'escalated'; all 8 merge-review escalations since Aug 1 are this pattern, and before runControls landed (~Jul 29) such runs sat stalled 7-14 hours.

**Evidence:** 4/4 sampled run_stalled runs' journals end with a github_rate_limited provider error as last activity then a 45m gap: ed87e2cd0e107dd995d089908d949474 (gather-pr-context), f4637297de6c4deb2af0f0fdd9c30c65 (elect-lander), c48c3bbee620f2ca686ce5b98ec3a251 (update-behind-pr), ddb5202e1bd6ba8467b0bef8346f2eb2 (pr-select). Merge-review escalated runs' run_errors: 8/8 run_stalled. Pre-#1698 durations in run_errors: fb1fa45362034e8db08dbe69cb87663c 'no journal progress for 14h40m6s' (Jul 21), ce09364849e60306d94f7fafaca19b69 10h22m (Jul 22). Stall clusters (Aug 5 15:13-15:14, Aug 7 14:18/15:18) coincide with hourly quota exhaustion windows.

**Fix locus:** upstream-runtime  |  **Related:** #1698 (runControls stalled-run net), K2

### [HIGH/confirmed] repass-budget-not-composed
**Claim:** The bounded-repass budget (default 3) is enforced per-gate, so runs alternating between the review gate and the local-ci/ci gates far exceed it: one implementation run executed 16 agentic implement sessions over 4.75 hours (10 review needs-changes + 5 local-gate fail + 1 ci-gate fail) before escalating; 326 implementation runs since Aug 1 had >1 traversal and 9 had >=8 implement traversals. Combined waste: 71% of the gaggle's model spend since Aug 1 ($1,100.34 of $1,551.45) went to failed/escalated/aborted runs.

**Evidence:** Run 9824e10c4250fd9fcf533d45b0431101 (escalated 2026-08-03, duration 4.75h): journal shows 16 implement stage.finished success events, gate.evaluated: review needs-changes x10 / pass x6, local-gate fail x5, final event runner.repassAttempt=4 escalated=true. internal/runcontrol/runcontrol.go:13 DefaultMaxRepasses=3; internal/gate/evaluate.go:131 'MaxRepasses is the inherited run budget. Gate.MaxRepasses takes precedence.' Spend: SELECT r.status, SUM(u.cost_usd) FROM stage_model_usage u JOIN runs r ... started_at>='2026-08-01' GROUP BY r.status => completed $447.95, escalated $814.61, failed $230.78, aborted $54.95.

**Fix locus:** upstream-runtime  |  **Related:** #20 (bounded repass), #953/#941 (per-cause budgets)

### [MEDIUM/confirmed] readmodel-repass-count-dead
**Claim:** read.db run.repass_count is 0 for every run ever recorded (MAX=0 across all time) while telemetry shows stage traversals up to 17, because the projector only increments RepassCount on stage.rerun.requested — an operator-initiated rerun event — never on gate-driven repasses; the portal therefore cannot show the repass-waste layer at all.

**Evidence:** SQL: SELECT MAX(repass_count) FROM run => 0 (all gaggles, all time); telemetry: implementation max traversal 17, 326 runs >1 traversal since Aug 1. Code: internal/readmodel/project.go:348-352 increments RepassCount only under case journal.EventStageRerunRequested; internal/journal/event.go:31-33 documents that event as 'operator-requested rerun'. Gate repasses journal as gate.evaluated + new stage.started with higher traversal, which the projector does not count.

**Fix locus:** upstream-runtime  |  **Related:** read.db any_retry_waste column; dashboard V1.1 epic #1197

### [MEDIUM/confirmed] dns-outage-aug7
**Claim:** A single ~1-hour network/DNS outage on Aug 7 21:00-22:00Z produced ~300 'Could not resolve host: github.com' errors and failed 72 merge-review/merge-review-critical runs at their start stage (reconcile-post-merge worktree fetch) plus dozens of query-backlog worktree fetches; no stage-level retry or park absorbed a transient host-level outage, so each polling tick became a failed run.

**Evidence:** SQL: run_errors WHERE message LIKE '%resolve host%' => 242 at 2026-08-07T21, 54 at T22. reconcile-post-merge executor_error Aug 4-7: 72/73 are dns-could-not-resolve, all on 2026-08-07. merge-review.yaml main: 'start: reconcile-post-merge' (line 95) — these failures are entry-stage, not post-merge-inconsistency. Sample message: 'prepare stage "reconcile-post-merge": create worktree: worktree: fetch https://github.com/Agent-Clubhouse/Goobers.git ... fatal: unable to access ... Could not resolve host: github.com'.

**Fix locus:** upstream-runtime  |  **Related:** 

### [MEDIUM/confirmed] localci-red-baseline-cluster
**Claim:** Aug 2-3 saw 178 local-ci nonzero_exit attempts in two days (vs single digits most days) in implementation lanes — a mixed red-baseline episode: an environment-dependent test (TestRebasePRRegeneratesPortalDistConflict fails with 'exec: make: executable file not found in $PATH'), identical golden-file failures across unrelated runs (TestCLIDocsUpToDate, TestCLIHelpGolden, TestCompletionScriptsGolden), and genuine agent errors (errcheck on new files); the cluster coincides with the Aug 3 daemon update + config re-syncs.

**Evidence:** SQL: local-ci nonzero_exit by day, workflows implementation+critical: 2026-08-02=102, 2026-08-03=76, adjacent days 1-16. Run cb428087b23b468148fb8464db4c7a0a artifact 5e81e4d0...: '--- FAIL: TestRebasePRRegeneratesPortalDistConflict ... exec: "make": executable file not found in $PATH'. Runs ec89cd365f7524d73201a5774ee540d2 and c5299c6828a9548e42f2c7c74a0c3c09 both fail TestCLIDocsUpToDate+TestCLIHelpGolden+TestCompletionScriptsGolden (same goldens, different runs => baseline, not agent). Run 4ae6984f65edffdf0ec08b52959abd1f: agent-caused errcheck lint failure in test/ghcpecho/main.go:170.

**Fix locus:** process  |  **Related:** instance commits fe32e99/968e7a2 (Aug 3 syncs); memory: main RED time-bomb test, shared-host node drift

### [MEDIUM/confirmed] open-pr-rate-limit-waste
**Claim:** 32 implementation runs since Aug 1 completed the full agentic implement + local-ci pipeline and then failed at open-pr because the API quota was exhausted, discarding $88.39 of model spend at the final step; there is no deferred-publish or claim-preserving retry, so the issue is re-implemented from scratch by a later run.

**Evidence:** SQL: SELECT COUNT(DISTINCT re.run_id) FROM run_errors re JOIN runs r ... WHERE re.stage='open-pr' AND re.message LIKE '%rate limited%' AND r.started_at>='2026-08-01' => 32; joined stage_model_usage cost => $88.39. Sample: 'executor: provider stage "open-pr" reported github_rate_limited: open pull request: ...' x50 messages.

**Fix locus:** upstream-runtime  |  **Related:** K2

### [MEDIUM/confirmed] park-handoff-double-fault
**Claim:** The escalation-handoff stages themselves fail under rate limiting — 17 park-infrastructure-failure executor_error failures (the 'goobers remediation-checkpoint --escalate' call is itself rate-limited) plus 3 'selectedNumber is required' plumbing failures — so the escalation snapshot/label is never recorded, which feeds the re-selection churn of finding 3.

**Evidence:** SQL: stage_attempts pr-remediation escalated runs: park-infrastructure-failure failure/executor_error x17. run_errors messages for park stages: 'executor: provider stage "remediation-checkpoint" reported github_rate_limited: list pull requests...' x12 and per-PR variants; 'selectedNumber is required (inputsFrom gather-pr-context...)' x3. park-infrastructure-failure defined at pr-remediation.yaml:744-771 (main) with policyActions escalate-pr.

**Fix locus:** upstream-runtime  |  **Related:** 

### [LOW/confirmed] curation-issue-write-gap
**Claim:** All 8 backlog-curation escalations since Aug 1 are the curate agent discovering it has no write-capable GitHub issue tool — a known gap tracked by open issue #2201 (TBH-1 approved but unimplemented) — reported under six different invented error codes, so the same known cause keeps resurfacing as fresh-looking escalations.

**Evidence:** SQL: run_errors for backlog-curation blocked_by_agent: 'github_issue_write_forbidden: ... tracked by open issue #2201', plus WRITE_TOOL_UNAVAILABLE, TOOL_ACCESS_UNAVAILABLE, TOOLS_UNAVAILABLE (x2 variants), ISSUE_WRITE_UNAVAILABLE — one each. Also 1 curate failure ISSUE_WRITE_FORBIDDEN.

**Fix locus:** instance-config  |  **Related:** #2201, #2196 (TBH-1)

### [LOW/confirmed] adhoc-error-code-taxonomy
**Claim:** Agentic stages invent free-form error codes for the same condition — pr-remediation sibling-ordering blocks appear as BLOCKED_ON_SIBLING, SIBLING_MERGE_ORDER_BLOCKED, BLOCKED_BY_SIBLING_PRS, CROSS_PR_BLOCKED, SIBLING_PR_ORDERING, DEPENDENCY_NOT_MERGED, BLOCKED_BY_SIBLING_PR (7 codes for 7 runs) — defeating any downstream bucketing, dedup, or automated disposition of escalations.

**Evidence:** Journal scan of escalated pr-remediation runs without park stages: 9 implement 'blocked' events with the 7 distinct codes listed. Same pattern in backlog-curation (6 codes for one cause) and implementation blocked_by_agent (BLOCKED_BY_PREREQUISITE/BLOCKED_BY_PREREQUISITES/DEPENDENCY_NOT_AVAILABLE/DEPENDENCY_NOT_READY across 10 runs).

**Fix locus:** upstream-runtime  |  **Related:** memory: needs-human label conflation

### [LOW/confirmed] config-sync-burst-pattern
**Claim:** Each config/binary sync historically produced a same-day burst of contract-skew failures: Jul 18 GOOBERS_CRED_GITHUB_PR_REVIEW missing (x20 in 20 minutes), Jul 18-19 reviewDigest inputsFrom not found (x11), Jul 20 selectedHeadSha inputsFrom missing (x98 in ~90 minutes), Aug 3 the local-ci behavior shift — the whack-a-mole cadence the operator perceives tracks the sync events themselves.

**Evidence:** SQL: run_errors apply-verdict buckets with windows — pr-review-cred-missing 2026-07-18T21:11-21:31 x20; reviewDigest-missing 2026-07-18T23:33-2026-07-19T02:01 x11; selectedHeadSha-missing 2026-07-20T17:00-18:23 x98. Instance repo main log: syncs Jul 22/24/29, Aug 1/3 (commits 0aeb966, 5dd72f2, fe32e99, 968e7a2).

**Fix locus:** process  |  **Related:** memory: instance config is a drifted fork

### [LOW/confirmed] flake-detection-self-starved
**Claim:** test-instability-nomination — the workflow meant to diagnose flaky tests — fails mostly because its gather-ci-history stage is itself rate-limited (11 of 12 failures are missing_result_file wrapping 'HTTP 403: API rate limit exceeded for user ID 1669494' against Agent-Clubhouse/Goobers-Workflows actions runs), so the instability-diagnosis loop is a casualty of the instability it should explain.

**Evidence:** SQL: run_errors for workflow='test-instability-nomination': missing_result_file 'candidate-ci-history.json was not produced ... failed to get runs: HTTP 403: API rate limit exceeded for user ID 1669494 ... https://api.github.com/repos/Agent-Clubhouse/Goobers-Workflows/actions/runs' x10-11 of 12 failed runs since Aug 1.

**Fix locus:** upstream-runtime  |  **Related:** #1578 (flaky-test fast-pass epic), #664 (flake ledger)

### [INFO/confirmed] telemetry-readmodel-divergence
**Claim:** Minor bookkeeping gaps: 4 runs since Aug 1 have empty telemetry.runs.status while read.db shows escalated/running (ingest missed terminal events); 4 runs are frozen in phase='running' from the Aug 8 ~18:55Z daemon stop; scheduler stalled_run_sweep_failed fired 108 times (Jul 21-22) on a stray non-run directory (since removed); terminal_claim_inspection_failed x64 (Jul 27-Aug 7).

**Evidence:** SQL: ATTACH read.db; join on run_id where t.status<>r.phase => ''/escalated x3, ''/running x1. SELECT run_id, current_stage, last_activity_at FROM run WHERE phase='running' => 4 rows, last_activity 2026-08-08T18:48-18:55Z. scheduler_errors: stalled_run_sweep_failed 'inspect run directory 00033c291df81fafb861cf41b14dbb3d: journal: not a run dir' x108; directory no longer exists.

**Fix locus:** upstream-runtime  |  **Related:** 

### [INFO/confirmed] healthy-areas
**Claim:** Genuinely healthy signals: merge-review-critical escalations are near-zero (3-4), implementation's dependency-blocked escalations cite real open prerequisite issues (by-design, correct), the critical lanes' completion rates match the regular lanes, and July's dominant missing_result_file failure class (196 runs) is essentially gone in August (11) — that fix held.

**Evidence:** SQL: runs since Aug 1 by workflow/status (merge-review-critical: 5051 completed / 3 escalated). run_errors blocked_by_agent samples cite #2157/#2158/#2225/#2251/#1487 with coherent rationale. Pre-Aug missing_result_file 196 distinct runs vs 11 since Aug 1 (test-instability-nomination only).

**Fix locus:** unknown  |  **Related:** 
