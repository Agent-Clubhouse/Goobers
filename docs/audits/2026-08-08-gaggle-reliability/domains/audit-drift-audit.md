# Domain: drift-audit

## Summary
Cross-gaggle drift audit of the four shared workflows (backlog-curation, implementation, merge-review, pr-remediation) and their three goober role pairs (curator/site-curator, implementer/site-implementer, reviewer/site-reviewer), plus the wishlist delta. The site gaggle is a competent hand-fork: the deepest goobers lessons (#947/#929/#415/#2340, the salvage-omission subtlety in remediation) were carried correctly, and its large structural drops are declared in file headers and mostly maturity-justified — but the copy-by-hand process leaked exactly the drift class VISION.md predicts. Highest-impact accidental drift: the site-reviewer instructions lost the verdict finding-class contract, and the one real site needs-changes verdict confirms the consequence (unclassed finding -> invisible to remediation's substantive-cause detection -> label ping-pong with zero rework); the Aug-7 GH-quota throttle was applied only to the healthy gaggle while the quota-burning site still ticks every minute; and within the goobers gaggle itself the Aug-1 local-ci timeout bump never reached pr-remediation, which has since taken 92 false-red timeouts. Latent site drift: parked needs-remediation issues will be re-claimed by curation (missing #2028 exclusions), curation leaks goobers:claimed labels (missing release-claim), and infra failures get durably mislabelled as reviewer rejections. The wishlist branch (main + 2 commits) is verified semantically honest — its comment-discipline files are byte-equivalent after comment stripping — and its only structural change (quality-sprint's 8 researchers collapsed into one parameterized quality-lens-researcher) is achievable today with DSL surface already proven on main; wishes 3, 4, and 7 each have a live incident from this audit as their motivating evidence, confirming the wishlist is grounded in real failures rather than aspiration.

## Findings

### [HIGH/confirmed] goobers-pr-remediation-local-ci-timeout
**Claim:** Within the goobers gaggle, the 2026-08-01 local-ci timeout bump (timeoutSeconds: 1800) was applied only to implementation.yaml; pr-remediation.yaml's local-ci runs the same `make ci` merge tier with the 600s executor default and is mass-failing on timeout — 92 timeout failures since Aug 1 vs 14 genuine nonzero_exit, each burning remediation budget/repasses. Same hand-sync drift class the VISION decries, one file apart.

**Evidence:** git -C /Users/masonallen/source/goobers-instances show main:config/gaggles/goobers/workflows/pr-remediation.yaml (local-ci stage: no timeoutSeconds) vs main:config/gaggles/goobers/workflows/implementation.yaml (timeoutSeconds: 1800, comment 'Bumped from the 10m executor default while investigating apparent local-ci timeouts (2026-08-01)'). SQL: sqlite3 'file:.../telemetry.db?mode=ro' "SELECT r.workflow, sa.error_code, count(*) FROM stage_attempts sa JOIN runs r ON r.run_id=sa.run_id WHERE r.gaggle='goobers' AND sa.stage='local-ci' AND sa.status='failure' AND r.started_at>='2026-08-01' GROUP BY 1,2" -> pr-remediation|timeout|92, implementation|timeout|10.

**Fix locus:** instance-config  |  **Related:** #2368 (test-integration-strict tier), VISION wish 9 (hand-forked lane files drifting)

### [HIGH/confirmed] site-reviewer-verdict-contract-drift
**Claim:** Accidental drift (b): goobers reviewer instructions pin the verdict contract (holistic-mode findings MUST carry class in {rebase-needed,conflict,substantive,cross-pr-blocked}; exact severity enum; no evidence field); site-reviewer instructions omit all of it. Consequence is real: an unclassed finding fails FindingClass.RequiresCodeChange(), so gather-pr-context computes hasSubstantiveFindings=false and pr-remediation clean-rebases + clears the needs-remediation label WITHOUT any agentic rework — a needs-changes PR ping-pongs between merge-review and pr-remediation making zero progress.

**Evidence:** Instructions diff: git show main:config/gaggles/goobers/goobers/reviewer/instructions.md ('Every finding in holistic mode MUST carry a class') vs main:config/gaggles/goobers-site/goobers/site-reviewer/instructions.md (no mention of class). Occurred: the site's only merge-review needs-changes verdict, run 99a120c2365fca5c8cd6914c311055d3, artifact sha256/40/fbc026... = one severity:error finding with NO class key. Code path: cmd/goobers/gatherprcontext.go:612 (if !finding.Class.RequiresCodeChange() -> not substantive) and api/v1alpha1/envelope.go:348-354 (empty class returns false).

**Fix locus:** instance-config  |  **Related:** #358 verdict schema; VISION wish 1 (schema-validated handoffs would make this a validation error, not silent degradation)

### [HIGH/confirmed] quota-throttle-applied-one-sided
**Claim:** Accidental drift (b): the GH-API-quota lesson (commit b3ad664, 2026-08-08: 'every-minute was burning GH API quota' -> goobers pr-remediation dropped to */5) was applied only to the goobers gaggle. goobers-site implementation and pr-remediation still tick every minute — on the gaggle whose fine-grained PAT actually exhausts its hourly quota via the K1/K2 403-retry storm — producing ~1,000 failing implementation runs/day, each re-burning quota with no backoff.

**Evidence:** git -C /Users/masonallen/source/goobers-instances show b3ad664 (only goobers/pr-remediation.yaml schedule changed); main:config/gaggles/goobers-site/workflows/implementation.yaml and pr-remediation.yaml both schedule '* * * * *'. SQL: runs per day for goobers-site: 2026-08-06 implementation 1068 runs / 1067 failed; stage_attempts failure codes: query-backlog executor_error 2886 + github_auth_failed 441.

**Fix locus:** instance-config  |  **Related:** K1/K2, #2685, #2686-adjacent commit 788d6768 (no circuit breaker upstream)

### [MEDIUM/confirmed] site-sibling-overlap-budget-crash
**Claim:** The site's pr-remediation dropped 'sibling-overlap classification' but the sibling-overlap CAUSE still arrives via merge-review post-merge fan-out-remediation, and remediation-checkpoint hard-fails on any unset legacy budget input — so omitting siblingOverlapBudget crashed 4 of the site's 5 pr-remediation runs the first day the cause fired. Fixed on main same day (2e489f8) but never re-run; the file header still claims 'a flat repass bound instead' of per-cause budgets while the body now declares all four. This is the operator's own live proof of VISION wish 4 (one real budget primitive, needs upstream).

**Evidence:** SQL: 4 of 5 goobers-site pr-remediation runs failed 2026-08-08T18:44-18:47Z at remediation-checkpoint; journal gaggles/goobers-site/runs/25432fafac06c9cc79b3c39d348a4a20/events.jsonl errorMessage 'siblingOverlapBudget must be a positive integer, got "' . Fix: git show 2e489f8 (goobers-site/pr-remediation.yaml adds siblingOverlapBudget: "3").

**Fix locus:** upstream-runtime  |  **Related:** VISION wish 4; #953/#941 (PRR-6); filed-upstream list in 2e489f8 (#2605-#2692)

### [MEDIUM/probable] site-curation-missing-park-label-exclusions
**Claim:** Accidental drift (b), latent: site backlog-curation's query-backlog excludeLabels is 'goobers:ready,goobers:needs-human' — missing goobers:needs-remediation and goobers:blocked-on-sibling, which the goobers copy added (#2028) so parked items aren't re-offered for triage. The site's own implementation park-escalated parks with status needs-remediation, so once any site issue parks, curation will re-claim it, re-mark it goobers:ready, and implementation will re-fail it in a loop. Latent only because no site run has reached a park stage yet (they die at query-backlog).

**Evidence:** git show main:config/gaggles/goobers-site/workflows/backlog-curation.yaml (excludeLabels) vs main:config/gaggles/goobers/workflows/backlog-curation.yaml (#2028 comment + full exclusion list). site implementation.yaml park-escalated inputs.status: needs-remediation. SQL: 0 rows for site stage_attempts stage IN ('park-escalated','park-needs-human'); 8 close-out successes.

**Fix locus:** instance-config  |  **Related:** #2028

### [MEDIUM/probable] site-curation-missing-release-claim
**Claim:** Accidental drift (b): site backlog-curation ends at the curate stage — no release-claim terminal. The file header lists deliberate omissions (reconcile/health-sample/dedupe-surface) and release-claim is not among them. The claim-ledger lease is released by runner FinalizeTerminal, but the provider-visible goobers:claimed label mirror is only cleared by the release stage (#1003), so site issues keep stale goobers:claimed labels from curation until an implementation close-out eventually removes them.

**Evidence:** Stage inventory: SQL DISTINCT sa.stage for site backlog-curation = {query-backlog, curate} only. Journal gaggles/goobers-site/runs/90475e47b4f71c95fe7bffe8afd40392/events.jsonl: 10 claim-reconcile ref.touched ops, zero release ops before run.finished. Rationale for the stage: goobers backlog-curation.yaml release-claim comment (#234/#235/#1003/#1244). Runner lease release: internal/runner/run.go:1867,2024 ('FinalizeTerminal releases the run's claims').

**Fix locus:** instance-config  |  **Related:** #234, #1003, #1244; memory: claim-label leak (claims.json is truth)

### [MEDIUM/confirmed] site-infra-failure-misattributed-escalation
**Claim:** Accidental drift (b): site pr-remediation has no park-infrastructure-failure stage; its local-gate routes both infra and escalate to park-escalated, whose command hardcodes the escalation reason 'the in-run reviewer returned a terminal fail verdict... the approach itself was rejected'. A retryable infrastructure failure (npm registry hiccup, runner glitch) will be durably recorded on the PR as a reviewer rejection — a false forensic record that misdirects the operator. The goobers copy parks infra separately with an accurate 'no implementation defect was established' reason.

**Evidence:** git show main:config/gaggles/goobers-site/workflows/pr-remediation.yaml (local-gate branches infra: park-escalated; park-escalated command args) vs main:config/gaggles/goobers/workflows/pr-remediation.yaml (park-infrastructure-failure with --escalation-outcome infrastructure-failure).

**Fix locus:** instance-config  |  **Related:** 2026-08-03 sync block in goobers pr-remediation header (local-gate failure-class + infra branch)

### [MEDIUM/confirmed] site-maxopenprs-cap-dead
**Claim:** goobers-site sets readiness.maxOpenPRs: 3 but the cap is structurally unenforceable: the daemon builds exactly one OpenPRRefresher over cfg.Repos[0] (Agent-Clubhouse/Goobers), so the site gaggle's count under its goobers-site/ branch namespace is always 0 and the cap never throttles. Config surface lying — VISION wish 7's exact case, confirmed at code level; fix is upstream runtime (already filed per VISION).

**Evidence:** cmd/goobers/runnerwiring.go:1585 'repo := cfg.Repos[0]' feeding NewOpenPRRefresher (single repo per instance); internal/localscheduler/conditions.go:245-246 reads that one counter for every gaggle/workflow. instance.yaml on main lists Agent-Clubhouse/Goobers first, masra91/Goobers-Site second. VISION.md wish 7 ('filed upstream').

**Fix locus:** upstream-runtime  |  **Related:** #353, #1115, VISION wish 7

### [LOW/confirmed] site-implementer-missing-lessons
**Claim:** Accidental drift (b), bundle: site-implementer lacks (1) goober-level timeoutSeconds 3600 (#1070/#1034 lesson — falls back to the 30m built-in), (2) the ISSUE_OVER_SCOPE/NEEDS_DECOMPOSITION non-retryable escape codes (an un-scopeable issue burns the full repass budget), (3) the 'never self-report make ci/CI results' false-green rule, (4) the SEC-047 untrusted-issue-text framing — notable because upstream-sync files site backlog items derived from public upstream-repo content — and (5) the implement-goal PROVIDER_ACTION_REQUIRED guidance present in both goobers implementation lanes.

**Evidence:** Diff main:config/gaggles/goobers/goobers/implementer/goober.yaml (timeoutSeconds: 3600) + instructions.md (ISSUE_OVER_SCOPE, 'Do not report... make ci', SEC-047 line) vs main:config/gaggles/goobers-site/goobers/site-implementer/{goober.yaml,instructions.md} (none present); implement goal text in goobers-site/workflows/implementation.yaml ends before the PROVIDER_ACTION_REQUIRED sentence present in goobers/workflows/implementation.yaml.

**Fix locus:** instance-config  |  **Related:** #1070, #1034, #724, SEC-047

### [LOW/confirmed] site-curator-calibration-inversion
**Claim:** Site-curator inverts the goobers curator's earned calibration: goobers says 'Mark the outcome — bias toward ready' (built after repeated needs-human sweeps found ~95% mislabelled); site-curator says 'When genuinely unsure... choose needs-human'. The site will accumulate needs-human items needing manual sweeps. Additionally, site curate-goal and site-curator instructions reference a 'deterministic staleness signal' and staleAutoClose, but the site query-backlog sets neither staleAfterDays nor staleAutoClose, so the referenced signal never exists (benign no-op, evidence of unadapted hand-copy).

**Evidence:** main:config/gaggles/goobers/goobers/curator/instructions.md section '5. Mark the outcome — bias toward ready' vs main:config/gaggles/goobers-site/goobers/site-curator/instructions.md 'choose needs-human'. main:config/gaggles/goobers-site/workflows/backlog-curation.yaml query-backlog inputs lack staleAfterDays/staleAutoClose while the curate goal says 'using each item's deterministic staleness signal'.

**Fix locus:** instance-config  |  **Related:** #1273 (CURE-5); memory: needs-human mislabel sweeps 2026-07-31 / 2026-08-01

### [LOW/confirmed] goobers-goober-workflows-lists-stale
**Claim:** Reverse-direction drift (b, fix on the SITE side): goobers implementer.goober.yaml declares workflows: [implementation] and reviewer.goober.yaml [implementation, merge-review], but both are also referenced by pr-remediation (and reviewer by merge-review-critical). Site copies list all referencing workflows. Impact is cosmetic today — readservice inventory unions the declared list with actual task/gate references — but the declared registration set is wrong for the k8s-operator tier and misleads readers.

**Evidence:** main:config/gaggles/goobers/goobers/{implementer,reviewer}/goober.yaml workflows: lists vs pr-remediation.yaml's 'goober: implementer'/'goober: reviewer' references; union logic at internal/readservice/inventory.go:474-500; operator registration set at internal/operator/gaggle_controller.go:323.

**Fix locus:** instance-config  |  **Related:** 

### [LOW/hypothesis] site-merge-review-escalate-into-apply-verdict
**Claim:** The site merge-review review gate declares an escalate: apply-verdict branch that the goobers copy deliberately does not have. An escalated evaluation (reviewer harness failed twice, no verdict artifact written) would route into apply-verdict, which reads the gate's verdict from the journal and threads reviewDigest — plausibly crashing or publishing a nonsense review instead of failing the run cleanly. Unexercised so far (site review gate: 7 pass / 1 needs-changes, 0 escalate).

**Evidence:** main:config/gaggles/goobers-site/workflows/merge-review.yaml review gate branches (escalate: apply-verdict) vs main:config/gaggles/goobers/workflows/merge-review.yaml review gate (pass/needs-changes/fail only). SQL gate_verdicts for goobers-site merge-review review: 7 pass, 1 needs-changes.

**Fix locus:** instance-config  |  **Related:** #765 (gate-evaluator retry), #825

### [INFO/confirmed] site-declared-structural-drops
**Claim:** Structural divergence (c), declared and mostly maturity-justified: site merge-review drops queue-watch/queue gates/reconcile-post-merge (no merge queue), elect-lander/elect-gate, scope-gate, record-merge-refusal; site pr-remediation drops guard-before-*/release-claim (#1860), enrichment stages (review-threads/issue-context/ci-failures), and respond-to-findings; each drop is named in the file header with a pointer to the fuller goobers file. Residual consequences worth knowing: merge refusals terminate silently (merge-gate fail: "") with no demotion counter, so a stuck lander retries forever un-labeled; budget-exhausted/policy-excluded checkpoint escalations end as ordinary completed runs (no @escalate phase, invisible to escalation surfaces); and a crash between merge-pr and post-merge has no reconcile-post-merge re-entry to close out the issue.

**Evidence:** Headers of main:config/gaggles/goobers-site/workflows/merge-review.yaml and pr-remediation.yaml (explicit drop lists); site merge-gate branches fail: "" and checkpoint-gate fail: "" vs goobers record-merge-refusal (#950) and checkpoint-gate -> release-escalated-claim -> @escalate split (2026-08-03 sync block).

**Fix locus:** instance-config  |  **Related:** #950, #1860, #1162

### [INFO/confirmed] site-lessons-correctly-carried
**Claim:** Healthy-area confirmation: the deepest, subtlest goobers lessons WERE carried into the site copies correctly — open-pr-gate mid-flight staleness abort (#947), ci-gate timeout routed through park-escalated not bare @escalate (#929), park-escalated @escalate vs park-needs-human @abort phase split (#415/#2028), onTimeout: salvage present in implementation but correctly ABSENT in pr-remediation's implement (where salvage would false-green a timed-out session against the PR's pre-existing diff), check-issue-staleness before the expensive review (#2340), review-gate evaluator retry (#765), and branchNamespace 'goobers-site/' in lockstep with headPrefixes. Genuine parameters (a) are coherent: cadences, maxConcurrentRuns 2 vs 5, maxRunsPerHour 20-200 vs 900, maxItems 10 vs 20, npm-vs-make local-ci commands, no critical lane / no goobers:critical vocabulary on site.

**Evidence:** Side-by-side of the four workflow pairs extracted via git show main:config/gaggles/{goobers,goobers-site}/workflows/{backlog-curation,implementation,merge-review,pr-remediation}.yaml (diffs in scratchpad); gaggle.yaml branchNamespace values.

**Fix locus:** instance-config  |  **Related:** #947, #929, #415, #2340, #765

### [INFO/confirmed] wishlist-delta-map
**Claim:** The wishlist branch = main + 2 commits (906d45d, d9a2cce); 41 files. Per-file meaning: (1) VISION.md (new, 225 lines) — the charter: 9 wishes. (2) goobers/quality-sprint.yaml + DELETE 8 researcher goobers + ADD quality-lens-researcher — wish 2 within-gaggle: one lens goober invoked 8x with per-branch areaName/areaFocus inputs; achievable TODAY (main's own upstream-sync change-area-researcher already proves the DSL surface); must be adopted atomically with the deletions and quality-sprint is manual-trigger-only, so adoption risk is low. (3) goobers-site/upstream-sync.yaml + its 4 goobers — wish 8 rewrite, VERIFIED semantically neutral (comment-stripped diff = whitespace only); 3 inline WISH markers map to wish 1 (backlogPlanRef should be a schema-validated item list — needs upstream), wish 2 (site-sync-clerk/backlog-clerk should be one referenceable cross-gaggle role — needs upstream), wish 5 (upstream-churn-digest is agentic ONLY because deterministic stages can't read additionalRepos — needs upstream). (4) backlog-clerk + implementation/implementation-critical/backlog-curation/test-instability-nomination + light-touch goober.yamls — wish 8 comment discipline; VERIFIED 0 semantic changes (only issue-number citations removed, including from agent-visible goal prose; rules like the malformation backstop preserved with incident stories dropped). (5) Deliberately untouched: merge-review/merge-review-critical/pr-remediation (declared second pass) and wish 9 lane variants (explicitly deprioritized). Needs-upstream wishes each have a live incident on record in this audit: wish 3 = the K4 github:pr:review 404 (instance.yaml removal comment), wish 4 = the siblingOverlapBudget crash, wish 7 = the dead site maxOpenPRs (runnerwiring.go:1585).

**Evidence:** git -C /Users/masonallen/source/goobers-instances diff --stat main...mason/dsl-2.1-wishlist; comment-stripped per-file semantic-diff counts (0 for all comment-discipline files, 150 for quality-sprint.yaml); git grep -n WISH mason/dsl-2.1-wishlist -- config (4 markers); VISION.md sections 'The concrete wishes' and 'What's in this pass'.

**Fix locus:** dsl-surface  |  **Related:** VISION wishes 1-9; commits 906d45d, d9a2cce, 2e489f8
