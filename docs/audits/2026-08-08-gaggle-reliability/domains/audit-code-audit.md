# Domain: code-audit

## Summary
Upstream per-RUN credential scoping is healthy — RunnerGrants correctly matches each gaggle's repo token by owner/name (internal/credentials/scoping.go:36-43), which is why goobers-site stages authenticate correctly once its dedicated PAT existed. The systemic defect is one level up: nearly every DAEMON-level (instance-scope, built-once) service path bakes in cfg.Repos[0] — the maxOpenPRs poller (VISION wish 7, confirmed live: goobers-site's cap is never enforced), the backlog demand counter's token ref, terminal-branch cleanup, and status — and the provider quota ledger is keyed per-provider, not per-token, so one gaggle's exhausted PAT and the other's healthy quota overwrite each other. K4 is confirmed mechanically: instance.yaml credentials: entries have no gaggle/repo qualifier in the schema and RunnerGrants applies them as unqualified last-wins overrides for every gaggle; `goobers validate` structurally cannot detect the resulting misroute. K3 is root-caused: GIT_ASKPASS was built from a relative workcopies root and resolved against git's cmd.Dir (the mirror); fixed on main by a3b2e636 for the daemon path only, with `goobers workspace reset` still exposed. Contrary to the K1 brief, the Actions-API fallback IS on origin/main (squash 5f07c22f, merged Aug 8 10:46 local), and the auth circuit-breaker (788d6768) is too — but telemetry shows github_auth_failed continuing to 18:02 local today, proving the running daemon binary lags main by ~a day and all three shipped fixes have been inert in production. VISION wishes 5 and 6 are partially delivered on main: #2614 only ungates env vars for deterministic stages (scratch workspaces still never provision reference checkouts — filed #2605), and get_run_info is wired for the Copilot harness only.

## Findings

### [HIGH/confirmed] credentials-scoping
**Claim:** K4 confirmed at the mechanism level: instance.yaml credentials: entries become unqualified overrides that REPLACE the per-gaggle repo-matched grant for EVERY gaggle — the CredentialGrant schema has no gaggle/repo qualifier, so the DSL cannot express a scoped override. Blast radius is all 10 credentialedCapabilities (repo:push, pr:write, pr:review, pr:merge, branch:delete, issues:*, milestones:write) times every gaggle.

**Evidence:** internal/credentials/scoping.go:55-60 (override replaces grant; comment line 26: 'These stay unqualified (shared)'); cmd/goobers/runnerwiring.go:546-555 (cfg.Credentials -> overrides), 334-336 (capability list); internal/instance/config.go:929-939 (CredentialGrant: Capability/MCP/Token only). Operator hit: git -C /Users/masonallen/source/goobers-instances show main:instance.yaml (REMOVED 2026-08-08 comment: github:pr:review override routed dev5-github-token to goobers-site apply-verdict -> 404 on POST .../pulls/N/reviews)

**Fix locus:** dsl-surface  |  **Related:** VISION wish 3; instance.yaml removal comment 2026-08-08; #2691 (the 404-vs-422 symptom it produced)

### [HIGH/confirmed] quota-ledger-single-token
**Claim:** The provider quota ledger conflates all tokens and gaggles: ProviderQuotaState is keyed by provider name only, one instance per daemon, fed by rate-limit outcomes and response headers from every gaggle's differently-tokened calls. One gaggle's exhausted PAT gates the healthy gaggle's polls, and conversely a healthy token's later-reset window observation replaces the exhausted window (Record keeps the later resetAt), reopening dispatch for the exhausted token — the amplification behind K2's github_rate_limited cascade.

**Evidence:** internal/localscheduler/providerquota.go:109-113 (map[apiv1.Provider]providerQuotaWindow), 123-149 (Record window replacement at 136-141), 163-172 (ExhaustedFor per provider); cmd/goobers/daemon.go:379 (single NewProviderQuotaState shared by all gaggles); cmd/goobers/runnerwiring.go:1255-1263 (RecordExhausted from any run), 1853 + providers/github.go:3102-3118 (header observations, no token identity); internal/localscheduler/conditions.go:226 (gate consults provider key only)

**Fix locus:** upstream-runtime  |  **Related:** K2; #712 (original single-token-era design); appears unfiled as a per-token issue

### [HIGH/confirmed] maxopenprs-first-repo
**Claim:** VISION wish 7 verified as a live runtime bug (already filed as #2692): exactly one OpenPRRefresher is built for the whole instance and it polls only cfg.Repos[0] with the first-binding default token. goobers-site's implementation workflow sets maxOpenPRs:1, but its count filters Agent-Clubhouse/Goobers PRs for heads prefixed 'goobers-site/implementation/' — always zero — so the cap never binds and masra91/Goobers-Site PR accretion is unbounded, while the field claims otherwise.

**Evidence:** cmd/goobers/runnerwiring.go:1586-1595 (repo := cfg.Repos[0]; single lister), 1582 (buildCredentials cfg,'','' -> bindings[0] token via scoping.go:32-35); internal/localscheduler/openprcount.go:78-104 (per-gaggle namespace filter over the ONE repo's PRs); internal/localscheduler/scheduler.go:995 (single counter instance-wide); config: git -C /Users/masonallen/source/goobers-instances show main:config/gaggles/goobers-site/workflows/implementation.yaml (maxOpenPRs set) + main:config/gaggles/goobers-site/gaggle.yaml (branchNamespace: goobers-site/)

**Fix locus:** upstream-runtime  |  **Related:** #2692 (OPEN); VISION wish 7

### [HIGH/confirmed] deployment-lag
**Claim:** Shipped fixes are chronically inert because the running daemon binary lags origin/main: the askpass fix (a3b2e636, on main Aug 6 22:08) did not stop askpass failures until Aug 7 17:59 (2501 goobers-site runs hit it, many post-fix); the auth circuit-breaker (788d6768, on main Aug 8 04:20) and the Actions-API fallback (squash 5f07c22f, merged Aug 8 10:46 local) were both on main today, yet goobers-site kept failing github_auth_failed through 18:02 local with no circuit suppression. Correction to the K1 brief: the #2685 fix IS on origin/main — as squash commit 5f07c22f (PR #2689), an ancestor of audit tip c0167a1b; only 'has never run' remains true.

**Evidence:** git log -1 a3b2e636 / 788d6768 (dates); gh pr view 2689 (mergeCommit 5f07c22f, mergedAt 2026-08-08T17:46:55Z); git merge-base --is-ancestor 5f07c22f c0167a1b = yes; providers/github.go:2004-2033 (fallback present in worktree); journals: grep "cannot exec 'gaggles" gaggles/goobers-site/runs/*/events.jsonl -> first 2026-08-05T19:49, last 2026-08-07T17:59, 2501 files (0 in goobers — public repo never invokes askpass); SQL: SELECT r.gaggle,sa.stage,count(*),min(sa.started_at),max(sa.started_at) FROM stage_attempts sa JOIN runs r ON r.run_id=sa.run_id WHERE sa.error_code='github_auth_failed' AND sa.started_at>='2026-08-07' GROUP BY 1,2 -> goobers-site query-backlog 441 (05:26->18:02 Aug 8), pr-select 47

**Fix locus:** deployment  |  **Related:** K1, K2, K3; #2685/#2689/#2687; memory: embedded-schema-fix-needs-rebuild-restart

### [MEDIUM/confirmed] credentials-validate-blindness
**Claim:** `goobers validate` structurally cannot detect a K4-class misroute: --check-repos preflights only each repos[].token against its own repo and never materializes the effective per-gaggle capability grants, so an override token with zero access to a gaggle's repo validates clean. No static warning exists for unqualified credentials: overrides in a multi-gaggle instance.

**Evidence:** cmd/goobers/validate.go:825-864 (checkTargetRepositoriesAtFile iterates repos[] only), 871-893 (resolveRepoToken uses repo.Token/GitHubApp only, never credentials: refs); grep 'credential' cmd/goobers/configwarnings.go = no hits; validate.go:503-504 shows the only repos[0] warning is for empty spec.project binding

**Fix locus:** upstream-runtime  |  **Related:** VISION wish 3; #2607 (same blindness for additionalRepos grants)

### [MEDIUM/confirmed] askpass-relative-residual
**Claim:** K3 root cause confirmed and fixed for the daemon (GIT_ASKPASS built from a relative workcopies root resolved against git's cmd.Dir = the managed mirror, not the daemon cwd), but a3b2e636 patched only runnerwiring.go — `goobers workspace reset` still passes a possibly-relative WorkcopiesBaseDir into buildWorktreeGitEnv, so its askpass path regresses identically when run with the default '.' instance path against a private repo. Same latent class in the remote config-repo source (no cmd.Dir, relative-instanceRoot askpass).

**Evidence:** a3b2e636 diff (filepath.Abs added at cmd/goobers/runnerwiring.go:1977 only); cmd/goobers/workspace.go:81+106 (workcopiesRoot := layout.WorkcopiesBaseDir(), root default '.', passed un-Abs'd); cmd/goobers/runnerwiring.go:2438 (WriteAskpassScript(filepath.Join(workcopiesDir,'auth'))); internal/worktree/manager.go:284-296 (doc: two processes resolve relative paths against different cwds), 784/801/1001/1014 (cmd.Dir = mirror); internal/instance/gitsource.go:83,114,130,584-589 (managedRoot from instanceRoot; git runs with process cwd)

**Fix locus:** upstream-runtime  |  **Related:** K3; a3b2e636 / #2518 / PR #2521

### [MEDIUM/confirmed] backlog-counter-token-misroute
**Claim:** The daemon-side backlog demand counter authenticates with the FIRST repo's token while querying the workflow's own repo: ref is hardcoded cfg.Repos[0].Owner+'/'+Name but repo is the workflow's repoRef. The sibling buildScheduleDemandCounter does it correctly (ref from repoRef), proving 1750 is a bug not a design. Latent in this instance today (no workflow in either gaggle uses a backlog-item trigger on main), but the moment goobers-site adopts one, its demand polls will 404 under the org token and — with the new circuit — IsAuthenticationError will park the workflow until operator reload.

**Evidence:** cmd/goobers/runnerwiring.go:1749-1751 (ref: cfg.Repos[0]... vs repo: repoRef...) contrast 1791 (ref: repoRef.Owner+'/'+repoRef.Name); cmd/goobers/daemon.go:626 (credResolver over all repos, refs named owner/name), 706; internal/localscheduler/scheduler.go:1296-1305 (auth error in pollDemand -> openAuthCircuit); trigger audit: git -C /Users/masonallen/source/goobers-instances show main:config/gaggles/*/workflows/*.yaml -> all schedule/manual, none backlog-item

**Fix locus:** upstream-runtime  |  **Related:** unfiled; same family as #2692

### [MEDIUM/confirmed] terminal-branch-first-repo
**Claim:** Terminal-branch cleanup is pinned to repos[0] with the first-binding token: every terminal/aborted run's leftover pushed branch is deleted against cfg.Repos[0] regardless of the run's gaggle. For goobers-site runs the delete targets the wrong repo, silently reports 'branch-not-found', and the real branch leaks in masra91/Goobers-Site forever; if two gaggles ever shared a branch namespace, this could delete a same-named branch in the WRONG repo.

**Evidence:** cmd/goobers/terminalbranch.go:81-97 (repo := Repos[0]; buildCredentials cfg,'','' -> first-repo grants), 192 (DeleteBranchRequest{Repository: repo} — the fixed closure value), 196-197 (silent skipped 'branch-not-found'); namespaces distinct today (masons-goobers/ vs goobers-site/) so leak-only

**Fix locus:** upstream-runtime  |  **Related:** unfiled; adjacent symptom #2509 (stranded branches)

### [MEDIUM/confirmed] additionalrepos-wish5
**Claim:** c0167a1b (#2614) does NOT deliver VISION wish 5: it only moves GOOBERS_ADDITIONAL_REPO_* env exposure out of the injectRunContext gate for deterministic stages that declare contents:read. Reference-repo checkouts are still provisioned exclusively for repo-backed workspace modes — the scratch path creates a bare temp dir and never calls provisionAdditionalCheckouts, and pinned+additionalRepos is a hard error — so 'read a reference repo without your own project checkout' remains impossible for both deterministic and agentic stages (agentic paths come from Envelope.AdditionalWorkspaces, populated only from provisioned repo-backed workspaces).

**Evidence:** git show c0167a1b -- internal/executor/env.go (diff: only env-gating change); internal/runner/run.go:3909-3925 (WorkspaceScratch: no provisioning), 4067-4069 (pinned + additionalRepos error), 3886 (AdditionalWorkspaces from provisioned workspace only), 3939/3980/3993/4030 (provision calls all in repo-backed arms); internal/harness/environment.go:56-74 (agentic env from AdditionalWorkspaces)

**Fix locus:** upstream-runtime  |  **Related:** VISION wish 5; #2605 (OPEN, exactly this), #2607

### [LOW/confirmed] run-identity-wish6
**Claim:** 6754aab9 (#2659) delivers get_run_info only for agentic stages under the Copilot harness: the goobers-io MCP server is wired exclusively in the Copilot adapter; ClaudeAdapter.Run registers no MCP server, so claude-code agentic stages remain identity-blind. Deterministic stages get run identity env only when the command is the goobers CLI (deliberate #322 boundary). VISION wish 6 ('every stage type') is partially delivered; no live impact today since this instance runs the Copilot harness.

**Evidence:** internal/mcpio/protocol.go:146,239 + tools.go:37-53 (get_run_info); internal/harness/copilot_mcp_io.go:12-14,64,79 (Copilot-only wiring); internal/harness/claude.go:124-203 (Run: no MCP/goobers-io registration); internal/executor/env.go:135-143 (injectRunContext rationale), shell.go:306

**Fix locus:** upstream-runtime  |  **Related:** VISION wish 6; #2659/6754aab9

### [INFO/confirmed] auth-circuit-characterization
**Claim:** The github_auth_failed circuit-breaker (788d6768, on origin/main) is per WorkflowIdentity, opens from demand-poll auth errors and run FailureCode, suppresses both polls and dispatch, and resets ONLY on config reload — no half-open probe, so one transient 401 parks a workflow until an operator reloads. Subtler gap: IsAuthenticationError excludes 403s carrying rate-limit guidance, so a misconfigured token whose quota is already exhausted presents as rate-limited and the circuit cannot latch until the window resets — K2's two failure modes mask each other's breaker.

**Evidence:** internal/localscheduler/scheduler.go:182-185 (doc), 1296-1305 and 1708-1710 (open), 751/1091/1610 (suppression), 1001 (reload-only clear); providers/transient.go:18-41 (403+guidance excluded, statusCodePattern fallback for subprocess-crossed errors); providers/github.go:2942-3030 (send retries: transport/5xx bounded, rate-limit budgeted, 401/403 never retried in-request)

**Fix locus:** upstream-runtime  |  **Related:** 788d6768 / #2687 (CLOSED); K2

### [INFO/confirmed] healthy-per-gaggle-scoping
**Claim:** Healthy-area confirmation: per-gaggle run-path credential scoping and askpass scaffolding are sound. RunnerGrants matches each gaggle's own repo binding by owner/name (first-binding fallback documented for single-repo instances); AdditionalReadGrants produces only repo-qualified read grants with no write path; the askpass helper is written per-gaggle at daemon startup/reload into gaggles/<g>/workcopies/auth/, is secret-free, and tokens exist only in the git child environment with credential helpers disabled. The len(Repos)==1-guarded provider fallbacks are correctly scoped. Cosmetic residue: `goobers status` PR-label counts read only repos[0] (self-documented).

**Evidence:** internal/credentials/scoping.go:31-67, 95-120; cmd/goobers/runnerwiring.go:2438 + internal/credentials/git.go:25-32, 80-86, 101-120 (GIT_CONFIG credential.helper= disable); guarded fallbacks: cmd/goobers/adoprovider.go:45, giteaprovider.go:34, runnerwiring.go:2353/2379/2465; cmd/goobers/status.go:148-161 (documented primary-repo-only)

**Fix locus:** upstream-runtime  |  **Related:** MGV-5 #1012, MGV-10 #1285, MGV-11 #1286
