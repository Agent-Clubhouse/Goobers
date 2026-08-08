# Domain: issue-inventory

## Summary
Upstream tracking of the VISION wishlist is stronger than expected: of the 9 wishes, wish 7 (maxOpenPRs) is fully filed (#2692, approved+critical, filed today), wishes 2/3/5/6 are partially covered by open issues (#2418, #2607/#1795-#1800, #2605, plus merged #2614/#2659), and wishes 8/9 are correctly unfiled by design. Two wishes have NO tracking issue anywhere: wish 1 (structured artifact/handoff schemas) and wish 4 (one budget primitive covering all current+future remediation causes) — these are the duplicate-filing hazards to close. Every observed failure class is upstream-tracked, and remarkably all four hot fixes (#2680 docs, #2688 circuit-break, #2689 check-runs fallback, #2521 askpass) MERGED to origin/main on Aug 7-8 — the audit premise that #2685's fix "is not on origin/main" is stale as of 17:46Z today. The real problem is deployment lag: telemetry shows github_auth_failed at 18:02Z and pr-select executor_error at 18:33Z today, after every fix merged, so the running daemon predates all of them and a rebuild+restart would retire the K1/K2/K3 classes at once. The K3 askpass hypothesis is confirmed as an already-diagnosed upstream bug (#2518, fixed by #2521: GIT_ASKPASS path built from a relative workcopies root). K4's instance-wide credential override has no upstream issue for the validate-time ambiguity check wish 3 asks for — the only credential-scoping gap left unfiled.

## Findings

### [HIGH/confirmed] deployment-lag
**Claim:** Every fix for the goobers-site failure classes (K1/K2/K3) is merged on origin/main, but the daemon that produced the failures predates all of them: telemetry records github_auth_failed at 2026-08-08T18:02:23Z (after #2688 merged 11:20Z and #2689 merged 17:46Z) and pr-select executor_error at 18:33:25Z. The single highest-leverage remediation is not a code change or a filing — it is rebuilding the daemon from current main (c0167a1b, which contains #2521, #2680, #2688, #2689, #2614, #2659) and restarting. Until then goobers-site's ~95% failure rate cannot improve.

**Evidence:** sqlite3 'file:.../telemetry.db?mode=ro' "SELECT sa.stage, sa.error_code, COUNT(*), MAX(sa.started_at) FROM stage_attempts sa WHERE sa.started_at>='2026-08-0' AND sa.error_code!='' GROUP BY 1,2 ORDER BY 3 DESC" — query-backlog executor_error 3351 (last 16:55:10Z), pr-select executor_error 1126 (last 18:33:25Z), query-backlog github_auth_failed 441 (last 18:02:23Z); PR merge times via gh pr view 2521/2680/2688/2689 --json mergedAt.

**Fix locus:** deployment  |  **Related:** #2689, #2688, #2680, #2521, #2614, #2659

### [MEDIUM/confirmed] wish-1-structured-artifacts
**Claim:** Wish 1 (schema-validated artifact handoffs between stages, per-item validation extending expectedOutputs) has NO upstream tracking issue — this is the largest unfiled wish. Nearest neighbors are adjacent, not covering: #2522 (open — resultShapeHint patches the scalar-only outputs rule per-field, not generally), #1484 (open — evidence-artifact schema, scoped to investigation workflows only), #2407 (open — DSL-declared MCP tool schemas, a different surface).

**Evidence:** gh search issues --repo Agent-Clubhouse/Goobers with terms 'structured artifact', 'artifact schema', 'output schema', 'typed output', 'list-shaped', 'per-item validation', 'artifact contract', 'declare shape' — no hit proposes handoff-schema validation. Precedent lineage the wish builds on: #881/#565 (expectedOutputs was inert) -> #902/#905 (made a verified contract); incident class motivating it: #299/#300/#302/#692 (list-shaped output violations).

**Fix locus:** dsl-surface  |  **Related:** #2522, #1484, #2407, #881, #905, #299, #302

### [MEDIUM/confirmed] wish-3-capability-grants
**Claim:** Wish 3 (inspectable/honest capability grants) is PARTIALLY covered: open issues #2607 (validate doesn't flag inert contents:read/additionalRepos grants), #1795 MGV-14 (fail-closed explicit credential for additionalRepos), #1796 MGV-15, #1799 MGV-18, #1800 MGV-19; merged groundwork #1793 (credential/repo decoupling design), #1012/#1147 (per-gaggle credential scoping), #1285/#1464 (gaggle,repo,capability routing), #1119 (foreign-gaggle validate diagnostics). But the wish's core validate-time question — 'which token backs capability X for gaggle Y and can it reach the target repo' — is filed nowhere.

**Evidence:** gh search issues --repo Agent-Clubhouse/Goobers 'credential scoping' / 'capability grant' / 'RunnerGrants' / 'validate credential reach' (last returns empty). #2607/#1795/#1796/#1799/#1800 all state=open as of 2026-08-08.

**Fix locus:** upstream-runtime  |  **Related:** #2607, #1795, #1796, #1799, #1800, #1793, #1147, #1464, #1119

### [MEDIUM/confirmed] gap-k4-credential-override-validation
**Claim:** K4's exact surface — instance.yaml `credentials:` entries applied INSTANCE-WIDE by internal/credentials/scoping.go RunnerGrants, silently routing a capability override to a token with no access to a second gaggle's repo — has NO upstream issue asking validate to refuse (or even warn on) an ambiguous instance-level override across gaggles with different target repos. Only the downstream symptom is filed (#2691, open: IsSelfReviewError misses the fine-grained-PAT 404 signature that this misrouting produced). This is an unfiled wish-3 sub-item and a prime duplicate-filing hazard.

**Evidence:** /Users/masonallen/source/goobers-instances/instance.yaml lines 44-58 (REMOVED 2026-08-08 comment naming RunnerGrants' instance-wide application); gh search issues 'instance-wide credential' / 'capabilityOverrides' / 'first-registered' / 'credential override' return no covering issue; gh issue view 2691 (open, created 2026-08-08T18:30Z) covers only the 404-vs-422 classification.

**Fix locus:** upstream-runtime  |  **Related:** #2691, #1793, #1147

### [MEDIUM/confirmed] wish-4-budget-primitive
**Claim:** Wish 4 (one budget field applying to every remediation cause including future ones) has NO upstream tracking issue. The hand-listed workaround it replaces is real and current: the RUNNING pr-remediation.yaml declares four per-cause inputs (conflictBudget/substantiveBudget/failingCIBudget/siblingOverlapBudget, each "2"), and #2395 (merged) added a new 'human comment' remediation cause — precisely the N+1th-cause scenario the wish warns will bypass any hand-listed budget.

**Evidence:** git -C /Users/masonallen/source/goobers-instances show main:config/gaggles/goobers/workflows/pr-remediation.yaml | grep -n Budget (lines 381-384); gh search issues 'per-cause budget' / 'budget primitive' / 'budget every cause' — only #953/#1791 (built the per-cause workaround), #390 (per-PR repass budget), #1698 (MaxRepasses per-workflow/gaggle override), #1022 (token/cost budget hierarchy — different axis), #1973 (open, trackRepass reset bug). None proposes a single all-causes declaration.

**Fix locus:** dsl-surface  |  **Related:** #953, #1791, #2395, #1698, #1022, #1973, #390

### [MEDIUM/confirmed] wish-6-run-identity
**Claim:** Wish 6 is PARTIALLY covered: the agentic half shipped today — #2617 (agentic stages have no run identity, goober fabricated a run id in a filed issue) closed via PR #2659 MERGED 2026-08-08T18:50:40Z (get_run_info tool on the goobers-io MCP). The deterministic half remains both broken and UNFILED: GOOBERS_RUN_ID/GOOBERS_GAGGLE/GOOBERS_WORKFLOW are injected only when injectRunContext is true, which is false for any deterministic stage whose command[0] != "goobers" (a deliberate #322/#371 scoping to keep run env out of test stages) — so run.script/run.command stages still cannot state their own provenance, and no issue proposes an opt-in for them.

**Evidence:** internal/executor/env.go:135 ('injectRunContext is false for a stage whose command is NOT the goobers CLI') and env.go:177-178 in worktree /Users/masonallen/source/Goobers/.clubhouse/agents/Goobers-Special-Agent; gh issue view 2617 --json body (root-cause section names both paths); gh search issues 'GOOBERS_RUN_ID' — only #321/#322/#371 (the deliberate narrowing) and #2617/#2659.

**Fix locus:** upstream-runtime  |  **Related:** #2617, #2659, #322, #371, #2606

### [MEDIUM/probable] askpass-k3-upstream-mapping
**Claim:** K3's hypothesis is upstream-confirmed and already fixed: #2518 (closed) / PR #2521 (MERGED 2026-08-07T05:08:39Z) is titled 'GIT_ASKPASS path built from relative workcopies root — token-auth git subprocesses fail with "No such file or directory" when daemon launched without an explicit absolute instance root' — exactly the relative-path/CWD mechanism hypothesized, matching the observed 'cannot exec gaggles/goobers-site/workcopies/auth/goobers-askpass.sh' error shape (relative path, file exists today). The instance mapping is probable (not replayed here); the inventory fact is confirmed. No filing needed; the fix ships with the same rebuild as the deployment-lag finding.

**Evidence:** gh pr view 2521 --repo Agent-Clubhouse/Goobers --json title,mergedAt; gh search issues 'askpass' also surfaces lineage #667, #1383/#1391 (Windows .cmd helper), #14/#48.

**Fix locus:** deployment  |  **Related:** #2518, #2521, #667, #1383

### [MEDIUM/confirmed] multi-gaggle-general
**Claim:** Multi-gaggle support is upstream-tracked as a largely-delivered arc with a live tail, and the tail maps exactly onto this audit's failure classes: merged MGV core (#1012/#1147 credential scoping, #1285/#1286/#1464 additionalRepos routing, #1119 validate isolation, #2111 per-gaggle PR base, #2491 un-hardcoding workflow names, #2523 docs) vs open tail #1287 (MGV-12 live two-owner validation — the milestone this instance is de facto running), #1795/#1796/#1799/#1800 (credential-model completion), #2418, #2605/#2607, #2692, plus epics #1099 (V1 breadth, open) and #1899 (multi-instance same-repo, open) and tier-3 futures #656/#685. Every K1-K4 incident lives on the second-gaggle path, consistent with 'first-built gaggle wins' defects the MGV tail is meant to close.

**Evidence:** gh search issues 'cross-gaggle' / 'gaggle' --sort created (full listings in transcript); state checks: #1287, #1795, #1796, #1799, #1800, #1099, #1899 all open as of 2026-08-08.

**Fix locus:** upstream-runtime  |  **Related:** #1099, #1287, #1899, #34, #1012, #1147, #1285, #1464, #2491, #2523, #656, #685

### [INFO/confirmed] wish-2-shared-roles
**Claim:** Wish 2's cross-gaggle case is PARTIALLY covered by #2418 (open, 'Instance-level goobers: shared personas across all gaggles'): it delivers define-once-use-everywhere via an instance-level goobers dir mirroring skills #2223, but its proposed design targets an IDENTICAL shared persona — the wish's additional ask (per-gaggle differences like label vocabulary/trust-prefix declared as parameters) is not in #2418's scope as written. The within-gaggle case needs no upstream work (VISION marks it 'today', already applied in the wishlist branch).

**Evidence:** gh issue view 2418 --repo Agent-Clubhouse/Goobers --json body (proposed design sections 1-3: directory layout, loading, reference validation — no parameterization mechanism); VISION.md lines 105-114 names backlog-clerk vs site-sync-clerk with parameter differences.

**Fix locus:** dsl-surface  |  **Related:** #2418, #2223, #2405

### [INFO/confirmed] wish-5-reference-repo-decoupling
**Claim:** Wish 5 is PARTIALLY covered and actively moving: #2606 (deterministic run.script/run.command stages never got GOOBERS_ADDITIONAL_REPO_*) closed via PR #2614 MERGED 2026-08-08T18:50:44Z — it is the current HEAD of origin/main (c0167a1b). The core decoupling case remains OPEN: #2605 (workspace:scratch stages never get additionalRepos checkouts; carries goobers:approved AND goobers:needs-remediation — the self-hosting pipeline already failed once on it) and #2607 (validate accepts the inert combination silently).

**Evidence:** gh pr view 2614 --json mergedAt; gh api repos/Agent-Clubhouse/Goobers/branches/main (sha c0167a1b, message 'executor: expose additional repos to deterministic stages (#2614)'); gh issue view 2605 --json state,labels (open, needs-remediation); groundwork #1285/#1286 (MGV-10/11), #1464 merged.

**Fix locus:** upstream-runtime  |  **Related:** #2605, #2606, #2614, #2607, #1285, #1286, #1464

### [INFO/confirmed] wish-7-maxopenprs
**Claim:** Wish 7 is FULLY covered and VISION's 'already filed upstream' claim verified: #2692 (open, created 2026-08-08T18:45:56Z, labels goobers:approved + goobers:critical) states maxOpenPRs is silently unenforced for every gaggle except repos[0]'s owner — root cause buildOpenPRRefresher built once per daemon with cfg.Repos[0] hardcoded (cmd/goobers/runnerwiring.go:1568) — with empirical confirmation from this instance (goobers-site cap 3, 8 open PRs #224-#231). It is on the critical self-hosting lane, so upstream will pick it up autonomously.

**Evidence:** gh issue view 2692 --repo Agent-Clubhouse/Goobers --json state,createdAt,labels,body. Cap-bug lineage: #891/#981 (counts every goobers/* PR), #1115/#1149 (per-namespace filtering — the part that works), #986/#988 (exclude human-parked), #721 (tuning), #501 (open — separate maxEscalated design).

**Fix locus:** upstream-runtime  |  **Related:** #2692, #891, #1115, #1149, #986, #501

### [INFO/confirmed] wish-8-comment-discipline
**Claim:** Wish 8 (comment discipline) is explicitly 'on us, not Goobers' per VISION and correctly has no upstream issue; no filing needed or appropriate. Tangential upstream precedent exists only for structural (not comment) parity: #911 (closed).

**Evidence:** VISION.md lines 162-166; gh search issues 'comment discipline' via broad sweeps returned nothing on-point.

**Fix locus:** instance-config  |  **Related:** #911

### [INFO/confirmed] wish-9-lane-variants
**Claim:** Wish 9 (workflow lane variants) is deliberately unfiled per the operator's own deprioritization, and nothing upstream collides with that stance: #2089 (open) is a different 'lane' concept (parallel lanes against different branches), #1834 (workflow flighting/shadow) and #2682 (variant comparison primitive) are eval-oriented variant mechanics, not variant-dedup DSL. Correct state; do not file.

**Evidence:** VISION.md lines 169-179 ('explicitly not prioritized... revisit only if the copy actually drifts painfully'); gh search issues 'workflow variant' / 'lane' / 'implementation-critical' — no dedup-variant proposal.

**Fix locus:** dsl-surface  |  **Related:** #2089, #1834, #2682

### [INFO/confirmed] failure-class-checkruns-pat
**Claim:** The fine-grained-PAT/check-runs failure class (K1) is fully tracked and FIXED upstream — the audit premise is stale: #2685 CLOSED via PR #2689 MERGED 2026-08-08T17:46:55Z (merge commit 5f07c22f, an ancestor of current main c0167a1b), #2677 CLOSED via PR #2680 MERGED 07:36:56Z (docs 'Checks' permission), and the last open residual is #2691 (IsSelfReviewError misses the fine-grained-PAT 404 signature). No filing needed anywhere in this class.

**Evidence:** gh pr view 2689 --json state,mergedAt,mergeCommit; gh api repos/Agent-Clubhouse/Goobers/compare/5f07c22f...main (ahead_by 5, behind_by 0); gh issue view 2691 --json state (open, created 2026-08-08T18:30Z).

**Fix locus:** upstream-runtime  |  **Related:** #2685, #2689, #2677, #2680, #2691

### [INFO/confirmed] failure-class-rate-limit-circuit
**Claim:** The rate-limit/circuit-breaking class (K2) is fully tracked with a complete lineage and its newest gap FIXED upstream today: #2687 CLOSED via PR #2688 MERGED 2026-08-08T11:20:58Z (github_auth_failed now circuit-broken). Prior art: #614/#618 (detect+backoff), #711/#746 (typed provider failures), #712/#726 (provider-quota circuit breaker), #773/#1006 (secondary limits), #2385/#2403 (open-pr retry on rate-limit), #2587/#2593 (resume-burst re-exhaustion, merged 2026-08-07). No filing needed.

**Evidence:** gh pr view 2688 --json mergedAt; gh search issues 'circuit breaker' / 'rate limit' / 'github_auth_failed' / 'quota exhaust' — all hits enumerated, none open except unrelated #501.

**Fix locus:** upstream-runtime  |  **Related:** #2687, #2688, #712, #726, #614, #711, #746, #2587, #2593, #2385

### [INFO/confirmed] upstream-loop-velocity
**Claim:** Notable healthy signal with a coordination caveat: the upstream self-hosting loop consumed this operator's incident reports same-day (K1 filed 07:37Z merged 17:46Z; K2 filed 08:52Z merged 11:20Z; #2691/#2692 filed 18:30-18:45Z already approved/critical-labeled), and a DSL 2.0 deprecation epic (#2695 with #2696-#2700) was opened at 19:46Z today — while the instance's wishlist branch is named 'mason/dsl-2.1-wishlist'. Any filings for the two unfiled wishes (1 and 4) should be positioned against the 2.0 epic rather than a hypothetical 2.1, and re-searched at filing time given this issue-creation velocity.

**Evidence:** gh search issues --repo Agent-Clubhouse/Goobers 'gaggle' --sort created --order desc (creation timestamps in transcript); gh search issues 'dslVersion' — #2695, #2696, #2697, #2698, #2699, #2700 all created 2026-08-08T19:46-47Z; instance repo working branch name via task context.

**Fix locus:** process  |  **Related:** #2695, #2696, #2697, #2698, #2699, #2700, #2681
