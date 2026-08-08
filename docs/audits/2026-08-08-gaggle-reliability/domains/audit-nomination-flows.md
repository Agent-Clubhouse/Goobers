# Domain: nomination-flows

## Summary
The two flagship nomination flows plus test-instability-nomination are each one defect away from (or past) silent uselessness, and their apparent health is misleading. (a) upstream-sync ran twice: the failure was an uncommitted YAML revision whose deterministic stage referenced $GOOBERS_ADDITIONAL_REPO_GOOBERS that run.script stages never receive; the "success" filed 55 stub-bodied issues in 5 minutes whose real value was created by a 2.5-hour out-of-band operator enrichment session — the committed two-pass fix has never run. (b) quality-sprint's 8-lens fan-out genuinely executes in parallel, but 7 of 10 runs yielded zero filings, almost all from one root cause: the artifact hand-off contract (artifactFile/InputArtifactFile) was retrofitted piecemeal while agents wrote findings into ~/.copilot/session-state; the worst case (first run) surfaced as a trusted agent "no-work" that recorded a completed run with total silent data loss; the two later clean runs did deliver real, consumed backlog items (#2700-2704, one already claimed). (c) test-instability-nomination is the flagship-killer finding: its gather-ci-history stage runs bare `gh` in a scratch workspace nested inside the goobers-instances git repo, so for all 55 runs it analyzed the CI history of the config repo Agent-Clubhouse/Goobers-Workflows (4 PRs, no CI) instead of the target repo — zero nominations ever, every run green "completed/no-work". Cross-cutting: both gaggles plus operator tooling share one GitHub user's rate-limit bucket (masra91), config runs before it is committed (provenance unreproducible, workflowVersion stuck at 1), and agent-performed issue mutations never reach provider_mutations. Verdict: quality-sprint is nearest flagship-ready (2 consecutive clean runs), upstream-sync needs one clean post-fix run to prove itself, and test-instability-nomination must be considered non-functional until its repo targeting is fixed.

## Findings

### [CRITICAL/confirmed] test-instability-wrong-repo
**Claim:** test-instability-nomination has never nominated anything in 55 runs because gather-ci-history collects CI history from the wrong repository: bare `gh pr list`/`gh run list` run in a scratch workspace that lives inside the goobers-instances git working tree, so gh resolves origin Agent-Clubhouse/Goobers-Workflows (the config repo: 4-5 config PRs, zero CI) instead of the target Agent-Clubhouse/Goobers. Every 'completed' run is a false green: candidates:[] -> no-work, in a target repo with a documented flake history.

**Evidence:** Run 7348e0411646fd3746024823fdb97154 result artifact = {mergedPRs:[#4,#3,#2,#1] all statusCheckRollup:[], mainRuns:[]}; mergedAt values exactly match `gh pr list -R Agent-Clubhouse/Goobers-Workflows --state merged` (e.g. PR#1 2026-08-01T19:31:56Z = instances commit 0aeb966). Target-repo PR numbers are ~2600+. SQL: SELECT sa.stage,sa.status,COUNT(*) FROM runs r JOIN stage_attempts sa ON sa.run_id=r.run_id WHERE r.workflow='test-instability-nomination' AND r.status='completed' GROUP BY 1,2 -> 39 runs, ALL end no-work (23 shape-instability, 16 nominate-instability). Mechanism: cmd/goobers/runnerwiring.go:2259 ScratchDir=filepath.Join(l.WorkcopiesDir(),"scratch") (inside the instances repo); internal/runner/run.go:3921 MkdirTemp there; `grep -rn GH_REPO internal/ cmd/` = zero hits, so gh has only CWD to resolve from. Fix: pin -R/GH_REPO in the YAML command, or move ScratchDir outside any git repo (would fail loudly).

**Fix locus:** dsl-surface  |  **Related:** upstream design docs/design/test-suite-quality-workflow.md #1489/#1490; flake ledger #664, #1239, epic #1578 prove target-repo flakes exist to find

### [HIGH/confirmed] no-work-trusted-silent-loss
**Claim:** An agent-declared 'no-work' completion is trusted unconditionally and terminates the workflow as 'completed', which lets upstream data-plumbing failures masquerade as healthy runs. quality-sprint's first run lost all 8 lens outputs and still recorded completed; the same semantics green-light every wrong-repo test-instability run. The runner has the evidence to cross-check (upstream stages journaled artifacts / non-empty findings) but does not.

**Evidence:** Run 313af282743c533c51f75ec9cab0b038 (2026-08-03, status=completed): all 8 review-* stages succeeded but each findingsRef points to /Users/masonallen/.copilot/session-state/<uuid>/files/*.json with artifact_sizes:[] (nothing journaled; paths also unreadable under sandbox deny-rule PRIVATE_0=/Users/masonallen seen in the efc5e075 error text); triage then returned status no-work and the run completed with zero filings. Completion contract text (embedded in harness prompt, visible in efc5e075 error): "Use no-work only when the task completed without error but found nothing to act on; the runner then completes the workflow without running downstream stages."

**Fix locus:** upstream-runtime  |  **Related:** 

### [HIGH/confirmed] artifact-handoff-contract-retrofit
**Claim:** The artifactFile/InputArtifactFile hand-off contract was absent or partial for the pipeline's first week and is the single root cause of 6 of quality-sprint's 7 bad runs: agents wrote findings into ~/.copilot/session-state instead of the workspace, so fan-in stages received inert filename strings or unreadable absolute paths. The contract is now declared on every stage (with explanatory comments), and the two most recent runs are clean, but the residual footgun remains: artifact placement depends entirely on agent compliance, and only declared artifacts are checked.

**Evidence:** Taxonomy from journals: 313af282 triage no-work (silent loss); cc07bd73 triage blocked ARTIFACT_ACCESS_DENIED; 997a4d57 triage blocked FINDINGS_ARTIFACTS_UNAVAILABLE; 0aabcad7 6/8 lenses failure missing_declared_artifact (outputs still show session-state paths, e.g. /Users/masonallen/.copilot/session-state/c492e32d-.../files/ux-findings.md) then operator-canceled; e3d13583 nominate blocked MISSING_BACKLOG_PLAN ("only a summary reference (backlogPlanRef) was provided"). Post-fix: 5755fee4 and 2feb24bf triage+nominate success with artifacts journaled. YAML: git -C /Users/masonallen/source/goobers-instances show main:config/gaggles/goobers/workflows/quality-sprint.yaml (repeated "Without this, findingsRef is just an inert filename string" comments).

**Fix locus:** dsl-surface  |  **Related:** harness InputArtifactFile contract (internal/harness)

### [HIGH/confirmed] shared-user-rate-limit-blast-radius
**Claim:** Both gaggles' PATs, the operator's gh, and copilot CLI all authenticate as one GitHub user (masra91, id 1669494), sharing a single primary rate-limit bucket. 11 of test-instability-nomination's 12 failed runs are user-level 403 rate-limit exhaustion hitting the healthy gaggle's cron; the Aug 3 failure cluster predates the goobers-site gaggle's existence entirely, proving quota exhaustion has multiple sources beyond K2's auth-retry storm and will recur even after #2685/#2686-class fixes land.

**Evidence:** Journal stage.finished errors in 9f727333/2b2c7545/f5699dcd/a37e66f1/c53ba4c9 (Aug 3) and 835cabba/f9a4c936/56b1baab (Aug 6), 0abcb86c/8b6f4906 (Aug 7): "HTTP 403: API rate limit exceeded for user ID 1669494"; gh api user/1669494 -> login masra91. goobers-site gaggle added 2026-08-04 (instances commit 137d7b4), after the Aug 3 cluster. f3ca9a26 is the 12th failure (network: "error connecting to api.github.com").

**Fix locus:** deployment  |  **Related:** K2 (#2686-adjacent: github_auth_failed not circuit-broken); K1 #2685

### [MEDIUM/confirmed] upstream-sync-failed-run-rootcause
**Claim:** The failed upstream-sync run died because an (uncommitted) YAML revision used a deterministic run.script stage 'upstream-churn-data' that read $GOOBERS_ADDITIONAL_REPO_GOOBERS under set -u, but the runner never injects additional-repo env into run.script stages: buildStageEnv gates it on injectRunContext, which is set only when command[0]=="goobers", and scratch workspaces never provision additionalRepos checkouts at all. Exit 1 -> missing_result_file in 0.26s. Config fix (drop the stage; agentic churn digest gathers git-log itself) is committed and worked, but upstream has no validation to catch a script referencing an env var it can never receive.

**Evidence:** Journal gaggles/goobers-site/runs/2d1001f21ac2af1b38c7c77d970ddb13/events.jsonl seq 7: error 'declared result file "upstream-churn-report.json" was not produced (exit code 1); stderr: sh: line 1: GOOBERS_ADDITIONAL_REPO_GOOBERS: unbound variable'; inputs/workflow-graph shows start=upstream-churn-data kind=deterministic. Code: internal/executor/env.go:96-100 (var naming), internal/executor/env_test.go:102 ("only for a stage whose command is the goobers CLI"), internal/harness/environment.go:56-72 (agentic stages get AdditionalWorkspaces env unconditionally). Fixed YAML: instances commit 2e489f8, header points 3-4.

**Fix locus:** upstream-runtime  |  **Related:** MGV-10/MGV-11 #1285/#1286

### [MEDIUM/confirmed] config-provenance-unreproducible
**Claim:** The daemon executes uncommitted working-tree config, so what actually ran cannot be reconstructed from the instances repo: the failed upstream-sync run's workflow graph (digest eb179ace...) exists in no commit; the successful run started 16 hours before upstream-sync.yaml was first committed; and both runs recorded workflowVersion: 1 despite materially different graphs, so the version field carries no provenance signal — only the digest does, and its source is unrecoverable.

**Evidence:** runs/2d1001f21.../run.yaml startedAt 2026-08-07T19:38:51-07:00 workflowVersion:1 digest sha256:eb179ace...; runs/b703e2bd.../run.yaml workflowVersion:1 digest sha256:6132e29c...; git -C /Users/masonallen/source/goobers-instances log main -- config/gaggles/goobers-site/workflows/upstream-sync.yaml -> single commit 2e489f8 dated 2026-08-08 12:01:44 -0700.

**Fix locus:** upstream-runtime  |  **Related:** memory: instance-config-is-drifted-fork

### [MEDIUM/confirmed] upstream-sync-value-was-rescued-not-produced
**Claim:** upstream-sync's lone success is not yet evidence of flagship readiness: nominate filed 55 issues in 5.3 minutes from a 22KB plan (~400 bytes/item stub bodies, matching the YAML header's '~50 items with stub bodies'), and the high-quality 8-10KB source-verified bodies now visible were written by an out-of-band operator enrichment sweep hours later. Only after that enrichment did curation ready them and implementation runs open CI-green site PRs. The committed anti-stub fix (two-pass worklist discipline, 7200s timeouts, no caps) has never been exercised by any run.

**Evidence:** events.jsonl b703e2bd seq 70-76: nominate 20:25:46->20:31:04 -07:00, outputs total_filed:55; backlog-plan.json artifact = 22148 bytes for 55 items; GraphQL userContentEdits on masra91/Goobers-Site#181: edited 2026-08-08T04:22:46Z by masra91 (created 03:29:03Z), #223 updatedAt 06:40:25Z — an in-order 04:22->06:40Z sweep matching no daemon run (runs table shows only 4-10s implementation polls in that window). Downstream proof: run 5186284a claimed enriched issue #186 and opened PR masra91/Goobers-Site#229, ci-poll passing. Fix text: 2e489f8 upstream-sync.yaml header point 4 + site-relevance-triage/site-sync-clerk instructions.md.

**Fix locus:** process  |  **Related:** K4 (instance-wide credential override was removed in the same period)

### [MEDIUM/probable] harness-transient-no-retry-inconsistency
**Claim:** A transient copilot-CLI startup failure window (exit status 1 within seconds) killed three runs across two workflows on Aug 4 02:56-03:37Z with no stage retry (attempt_class empty), while a later executor_error of a different flavor (completion-schema violation) WAS retried as class 'infra' and saved its run — failure-class routing is inconsistent about which executor_errors are retryable, so single harness blips kill whole multi-stage sweeps.

**Evidence:** SQL: SELECT run_id,stage,attempt,attempt_class,status,error_code FROM stage_attempts WHERE (run_id='5755fee493b8531f4244b4241b8ec7a7' AND stage='nominate') OR (run_id='efc5e075246ef7a66036f3f53b1c7184' AND stage='churn-report') -> churn-report attempt1 executor_error no retry; nominate attempt1 executor_error then attempt2 attempt_class='infra' success. Failed trio: efc5e075 (02:58Z), 601655d4 (03:37Z), 401532089cdb11a870713d38b857839b (02:56Z, test-instability) all 'harness: copilot-cli: ... exit status 1'. 5755fee4 attempt-1 error: outputs/trust object rejected by result.schema.json.

**Fix locus:** upstream-runtime  |  **Related:** Agent-Clubhouse/Goobers#2704 (quality-sprint's own nomination: 'failure-class routes only Retryable=true failures to infra'); memory completion-contract-fix-producer-not-schema

### [MEDIUM/confirmed] agentic-mutations-invisible-to-provider-telemetry
**Claim:** Issue mutations performed by agents via gh bypass provider_mutations entirely: the run that filed 55 issues recorded only 2 branch-kind rows, so the most consequential external writes the nomination flows make have no telemetry audit trail (deterministic provider ops like issue|claim/issue|label are recorded; agentic gh calls are not).

**Evidence:** SQL: SELECT provider,kind,operation,COUNT(*) FROM provider_mutations WHERE run_id='b703e2bd5db1af70948c60ee074ca16a' GROUP BY 1,2,3 -> github|branch||1, github|branch|delete|1 (nothing else) vs journal seq 76 total_filed:55; global GROUP BY kind shows issue|claim 16656, issue|label 678 etc. from deterministic paths.

**Fix locus:** upstream-runtime  |  **Related:** 

### [MEDIUM/confirmed] quality-sprint-trust-rubric-variance
**Claim:** backlog-clerk's per-item trust rubric produced 40/40 needs-human (0 approved) on the Aug 5 quality-sprint run — recreating the exact human-bottleneck the needs-human label conflation memos document — while the Aug 8 run differentiated properly (mix of approved+claimed and needs-human). Rubric behavior is run-to-run unstable.

**Evidence:** events.jsonl 5755fee4 nominate outputs: {approvedCount:0, needsHumanCount:40, filed:'9 parents, 31 children...'}; 2feb24bf outputs {filed:39}; gh issue view Agent-Clubhouse/Goobers#2704 -> goobers:approved+goobers:claimed vs #2700 -> goobers:needs-human.

**Fix locus:** instance-config  |  **Related:** memory: needs-human-label-conflation, needs-human sweeps 2026-07-31/2026-08-01

### [LOW/confirmed] hallucinated-date-in-published-artifacts
**Claim:** The triage agent stamped a hallucinated future date (2026-09-15) into backlog-plan.json ('generated':'2026-09-15T14:30:00Z') and it propagated into all 55 filed issue footers ('Filed automatically by site-sync-clerk ... (run 2026-09-15)'), because nothing injects the real run date into agent context and nothing validates agent-authored metadata against run facts.

**Evidence:** artifacts/sha256/cb/a3e66b1440...(backlog-plan.json) first line; gh issue view 181/169 --repo masra91/Goobers-Site body footers; actual run started 2026-08-07T19:58-07:00 (run.yaml).

**Fix locus:** upstream-runtime  |  **Related:** 

### [LOW/confirmed] quality-sprint-temp-throttle-drift
**Claim:** quality-sprint.yaml still carries the TEMP testing raise 'maxRunsPerHour: 10' with its own comment saying 'Revert to 3 once testing settles' (2026-08-04) — unreverted config drift on a manual-trigger flagship.

**Evidence:** git -C /Users/masonallen/source/goobers-instances show main:config/gaggles/goobers/workflows/quality-sprint.yaml (readiness block: '# TEMP (2026-08-04): raised 3 -> 10 ... Revert to 3 once testing settles.').

**Fix locus:** instance-config  |  **Related:** 

### [INFO/confirmed] fanout-fanin-mechanics-healthy
**Claim:** The static fan-out/fan-in machinery itself is healthy: quality-sprint's 8 lens branches and upstream-sync's 4 all launch concurrently in independent read-only worktrees, branch/parallel completeness records are accurate, continue_on_error works, and post-fix the join stage receives every branch's artifact via context pointers. The blocked-escalation path is also working as designed — agents that could not do the work reported honest machine-readable blocked codes rather than fabricating success (6 escalations, all with accurate reasons).

**Evidence:** b703e2bd events.jsonl seq 10-46: 4 branches started within 150ms, parallel.finished completeness all succeeded/1-artifact; triage context artifact (sha256:2c5f3f8c...) enumerates all 5 upstream artifacts with branch names; stage_attempts for 210426c5/2feb24bf/5755fee4/997a4d57/cc07bd73/e3d13583 show all-8-lens success. Blocked codes: ARTIFACT_ACCESS_DENIED, FINDINGS_ARTIFACTS_UNAVAILABLE, MISSING_BACKLOG_PLAN, GITHUB_ISSUES_WRITE_UNAVAILABLE, plus 4 test-instability pre-restructure equivalents.

**Fix locus:** unknown  |  **Related:** docs/design/static-fan-out-fan-in.md, FO-10

### [INFO/confirmed] end-to-end-value-proven-once-each
**Claim:** Both manual flagship flows have each achieved genuine end-to-end value exactly once: quality-sprint 2feb24bf filed 39 real, well-formed target-repo issues on Aug 8 (#2700-2704 sampled; #2704 approved and already claimed by the implementation lane), and upstream-sync's output — after operator enrichment — was consumed by curation and implementation into CI-green site PRs (#229-233). The pipelines' downstream contracts (curation labeling, implementation claiming) demonstrably connect.

**Evidence:** gh issue list -R Agent-Clubhouse/Goobers --search 'created:2026-08-08' -> 50 results incl. #2700-2704 matching nominate outputs; gh issue view 2704 -> goobers:approved+goobers:claimed; site run 5186284a journal: query-backlog claimed #186 (readyAt 2026-08-08T05:22:28Z), open-pr #229, ci-poll passing, close-out success.

**Fix locus:** unknown  |  **Related:** 
