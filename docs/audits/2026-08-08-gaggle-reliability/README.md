# Gaggle Reliability Audit — 2026-08-08

Forensic audit of the two-gaggle self-hosting instance (`goobers` on
Agent-Clubhouse/Goobers, `goobers-site` on masra91/Goobers-Site), commissioned
by the operator ahead of a reliability-hardening pass. Eight parallel domain
sweeps over the run databases, per-run journals, instance-config history,
upstream code at `origin/main` (c0167a1b), and the upstream issue tracker;
every critical/high finding carries replayable evidence. Raw domain reports
with full evidence are in `domains/`. 114 findings total: 3 critical, 22 high.

Method note: all timestamps below are UTC unless suffixed "local" (local =
UTC-7). "Since Aug 1" means `started_at >= '2026-08-01'`.

## The corrected story of goobers-site

The gaggle lived 87 hours (Aug 5 03:52Z – Aug 8 18:47Z), ran 4,246 runs, and
failed 93.3% of them — but the failure history is not noise. It is a strict
sequence of four regimes, each masking the next, each ended by a restart or a
live config/credential change, never mid-flight:

| # | Regime | Window | Failures | Root cause | Fix, and where it was |
|---|--------|--------|----------|-----------|----------------------|
| 1 | clone 403 | Aug 5 03:52Z → Aug 6 02:45Z | 933 | Launched with a token the config commit itself admitted had no repo access (137d7b4), on a per-minute cron | Dedicated PAT; live at Aug 5 19:48 local, committed 25h later (14d06d1) |
| 2 | relative GIT_ASKPASS | Aug 6 02:49Z → Aug 8 00:59Z | 2,501 (63% of all failures ever) | `GIT_ASKPASS` built from a relative workcopies root, resolved against the git child cwd (upstream #2518) | Fixed upstream a3b2e636, merged Aug 6 22:08 local (PR #2521) — the daemon ran a pre-fix binary for ~20 more hours |
| 3 | check-runs 403 | Aug 8 05:25Z → 18:02Z | 490 stage + 496 scheduler | Fine-grained PAT cannot call the Checks API; began the minute the gaggle opened its first PR (#224) | #2685 Actions-API fallback, merged as PR #2689 (squash 5f07c22f) 17:46Z, deployed 18:09Z |
| 4 | final-hour config bugs | Aug 8 18:12Z → 18:47Z | 6 + 4 | (a) instance-wide `github:pr:review` override routed the wrong token (K4) → apply-verdict 404; (b) `siblingOverlapBudget` omitted in the site's hand-copied pr-remediation → hard-fail | (a) removed live 18:36Z; (b) fixed in instance commit 2e489f8 — **which has never run** |

**In its final 55 minutes** — fixed binary (93f4098e, containing the check-runs
fallback and the auth circuit-breaker) plus the live credential fix — the
gaggle completed 8 implementation runs end-to-end, opened PRs #224–#233,
claimed 43 issues, and **merged its first PR (#225) at 18:42:59Z, five minutes
before shutdown**. The system was turned off in its first working hour.

Every observed goobers-site failure class now has a merged/committed fix. The
single remaining known-unfixed defect in its path is #2692 (maxOpenPRs
unenforced for non-`repos[0]` gaggles — VISION wish 7, filed, approved,
critical, no PR yet).

### Corrections to the audit's own starting assumptions

- **K1 ("#2685 fix never ran") — wrong twice over.** The fix merged to main as
  squash commit 5f07c22f at 17:46Z (checking ancestry of the *branch tip*
  86ad1f70 gives a false negative — the squash-merge trap), and it was
  deployed and validated in the final session (github_auth_failed → zero,
  first end-to-end pipeline ever).
- **K2 ("~2,600 executor_errors are rate-limit cascades") — wrong by three
  orders of magnitude.** Exactly 2 stage failures in the site's history are
  `github_rate_limited`. The 3,487 opaque `executor_error`s decompose as
  2,501 askpass + 933 clone-403 + 48 DNS + 5 other. The *real* quota damage
  was cross-gaggle: the site's 404/retry storms on the shared identity starved
  the healthy gaggle's scheduler (1,029 scheduler errors on Aug 5 alone).
- **K3 (askpass) — confirmed and already closed upstream** (#2518/PR #2521);
  a residual copy of the same bug remains in `goobers workspace reset`
  (`cmd/goobers/workspace.go` passes an un-Abs'd root) and the remote
  config-repo git source.
- **K4 (instance-wide credential overrides) — confirmed mechanically.**
  `CredentialGrant` has no gaggle/repo qualifier (internal/instance/config.go:929),
  so the DSL *cannot express* a scoped override, and `goobers validate`
  structurally cannot detect the misroute (validate.go checks `repos[]` tokens
  only, never materializes effective per-gaggle grants).

## The healthy gaggle's hidden layers

The goobers gaggle's 97% completion rate (21,026/21,795 since Aug 1) conceals
four compounding mechanisms in its 765-run failure/escalation tail:

1. **One GitHub user, one quota — everything shares it.** Both gaggles' PATs,
   the operator's `gh`, and the Copilot CLI all authenticate as masra91
   (user ID 1669494), sharing a single 5,000/hr REST bucket that exhausts
   hour after hour — 43% (327/765) of the healthy gaggle's failed/escalated
   runs carry `github_rate_limited`, chronic since Jul 16 (pre-dating the
   site gaggle). Upstream amplifier: the provider-quota ledger is keyed **per
   provider, not per token** (providerquota.go:112), so gaggles overwrite
   each other's windows in both directions.
2. **pr-remediation's substantive lane is dead: 0-for-106 in August.** The
   Aug 1 local-ci timeout bump (600s→1800s) was applied to implementation.yaml
   only; `make ci` now takes >12 minutes; every pr-remediation local-ci since
   has timed out (92) or failed (14), producing 79 of the 144 escalations.
   One missing YAML line.
3. **Escalation parking is not sticky.** Any base-branch advance un-parks a
   PR (remediationcheckpoint.go:396-406), so deterministically-doomed PRs
   churn: 144 escalations map to 48 PRs; PR #2364 was escalated **41 times in
   5 days**, burning an agent session each time.
4. **Repass waste is unbounded and invisible.** The repass budget is enforced
   per-gate, not per-run — one run executed 16 implement sessions over 4.75h.
   read.db's `repass_count` is 0 for every run ever (the projector only counts
   operator-requested reruns), so the portal cannot see it. **71% of August
   model spend ($1,100 of $1,551) went to runs that did not complete.**

Also confirmed: `validate-finding-responses` demands responses matching the
*original* verdict's finding count (nil on failing-ci causes → "want exactly
0"), failing 96 legitimate remediations; rate-limited provider stages strand
runs until the 45-minute reaper mislabels them "escalated"; 32 completed
implementations were discarded at open-pr by quota exhaustion ($88 of spend,
re-implemented from scratch later).

## The nomination flows

- **test-instability-nomination has never once done its job (CRITICAL).** All
  55 runs analyzed the wrong repository: its gather-ci-history stage runs bare
  `gh` in a scratch workspace that lives *inside the goobers-instances git
  repo*, so gh resolves origin to the config repo (4 PRs, no CI) instead of
  Agent-Clubhouse/Goobers. Every run: green, "completed", no-work. A false-
  green flagship, undetectable from run status.
- **`no-work` is trusted unconditionally.** quality-sprint's first run lost
  all 8 lens outputs (agents wrote to `~/.copilot/session-state`) and still
  recorded a healthy completed run. The runner holds the evidence to
  cross-check (upstream journaled artifacts) and doesn't.
- **quality-sprint is nearest flagship-ready**: the 8-lens fan-out mechanics
  are genuinely healthy, the artifact-handoff contract is now declared
  everywhere, and the two most recent runs delivered 39+ real consumed
  backlog items (#2704 already claimed). 6 of its 7 bad runs trace to the
  artifact-contract retrofit era.
- **upstream-sync's lone "success" was operator-rescued**: nominate filed 55
  stub-bodied issues in 5 minutes; the high-quality bodies visible today came
  from a 2.5-hour out-of-band operator enrichment sweep. The committed
  two-pass fix has never run. (Also: the triage agent stamped a hallucinated
  date — 2026-09-15 — into all 55 issue footers; nothing injects real run
  facts or validates agent-authored metadata.)

## Cross-cutting themes (the actual disease)

1. **Deployment lag is the #1 reliability killer.** Every incident's fix
   existed upstream before or within hours of the pain, and binaries lagged
   fixes by 0.3–21 hours (~1,300 runs burned on the already-fixed askpass bug
   alone). Fix-merged→deployed closure does not exist as a loop. Related:
   run.yaml records workflow/goober digests but not the binary version, and
   telemetry ingests no daemon lifecycle events, so binary provenance requires
   journal archaeology.
2. **`repos[0]` is baked into every instance-scope daemon service**: the
   maxOpenPRs poller (#2692), the backlog demand counter's token, terminal-
   branch cleanup (deletes against the wrong repo for site runs — silent
   "branch-not-found", real branch leaks), `goobers status`. Per-RUN scoping
   (RunnerGrants) is healthy; the daemon-level singletons are the defect class.
3. **Config surface that cannot tell the truth**: unqualified credential
   overrides (K4), caps that bind only for the first repo (#2692), budget
   inputs that hard-fail when a hand-copied file omits them, `validate` blind
   to all of it. VISION wishes 3, 4, and 7 each have a live incident from this
   audit as their motivating evidence.
4. **Running state diverges from declared state**: uncommitted config ran in
   production three times in four days (the failed upstream-sync run's
   workflow digest exists in no commit); `workflowVersion` is stuck at 1;
   five `-dirty` binaries ran in production in the window.
5. **Observability forces archaeology**: 87% of site failures share one
   opaque bucket (`executor_error`/`unknown`, empty runner_json); claims-lock
   timeouts surface as generic executor errors; scheduler_events has no gaggle
   column; agent-performed `gh` mutations bypass provider_mutations entirely;
   agents invent free-form error codes (7 codes for one sibling-blocked cause).
6. **Hand-fork drift, both directions, exactly as VISION predicts** — the site
   copies carried the deepest lessons correctly (#947/#929/#415/#2340) but
   dropped the reviewer verdict-class contract (needs-changes PRs ping-pong
   with zero rework — observed live), the park-label exclusions (#2028), the
   release-claim stage, the infra-vs-rejection park split, and the quota
   throttle went to the *wrong gaggle* (goobers got */5; the quota-burning
   site kept per-minute crons).

## Daemon/storage health (secondary but real)

29 daemon sessions in 15 days, 45% dirty stops; telemetry.db is 854MB of which
281MB is a single Jul 21-22 unbounded-error-blob incident and ~335MB spans;
scheduler-span ingest re-parses and re-upserts all 75K spans (55.6MB) every
cycle (no cursor); no retention configured; read.db's day-bucket queue is
producer-only dead code (bucket_day empty forever, dirty_day never consumed);
claims.json lock contention: 4,505 slow acquisitions, 244 timeouts (each a
failed run stage); the terminal-claim reaper cannot inspect backlog-reconcile
claims (synthesized slash-bearing run IDs fail FindRunDir) — 64 hourly errors.

## Action plan

**Phase A — restart-and-verify (zero new code).** Build from current main,
run against instance-config main (2e489f8 — the committed-but-never-run fixes),
and verify: site query-backlog green, apply-verdict reaches the 422 degrade
path, pr-remediation past remediation-checkpoint, no github_auth_failed.
Expected result: the site gaggle *works*, because it already did for 55 min.

**Phase B — instance-config fix set** (branch off instance main): goobers
pr-remediation `timeoutSeconds: 1800`; site cron throttles (*/5 minimum);
site drift closures (reviewer verdict-class contract, park-label exclusions,
release-claim, park-infrastructure-failure split, implementer lessons bundle,
curator calibration); test-instability gather-ci-history pinned with
`-R Agent-Clubhouse/Goobers`; quality-sprint maxRunsPerHour revert.

**Phase C — upstream fixes on this branch**, in leverage order: #2692
(maxOpenPRs per gaggle×repo — the `repos[0]` class, fixing the siblings:
demand-counter token, terminal-branch cleanup); per-token quota ledger;
park stickiness (re-escalation generation counter); finding-responses
contract; repass budget composition per-run; error-code propagation into
stage_attempts (kill the executor_error/unknown bucket); binary version in
run.yaml + lifecycle events into telemetry; span-ingest cursor; error-blob
truncation; askpass residual in workspace.go; no-work cross-check against
upstream journaled artifacts.

**Phase D — conformance testbeds** (goobers-testbed-*): degenerate minimal
workflow first — it would have caught regimes 1–3 in minutes on a fresh repo.

**Phase E — draft issues for operator review** (unfiled gaps found by the
inventory): VISION wish 1 (structured artifact handoffs — largest unfiled
wish), wish 4 (single budget primitive covering current+future causes),
K4 validate-time ambiguity detection (wish 3's core), per-token quota ledger,
deterministic-stage run identity (wish 6's unfiled half), park stickiness,
no-work verification, agentic-mutation telemetry. Position DSL filings
against the new upstream DSL 2.0 epic (#2695), not a hypothetical 2.1, and
re-search at filing time — upstream merged four of this instance's incident
fixes same-day.
