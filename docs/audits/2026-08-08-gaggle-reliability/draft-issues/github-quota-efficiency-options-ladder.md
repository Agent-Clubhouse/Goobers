# Two gaggles with two separate PATs still share one GitHub quota bucket, and one exhausted token gates the other — efficiency options, not a rearchitecture

Suggested labels: `area:runner`, `area:providers`, `area:scheduler`, `type:bug`, `sprint:v1-breadth`

## Problem

An operator who provisions a dedicated token per gaggle reasonably expects each gaggle to have its own API budget. It does not. GitHub's primary REST quota is per authenticated *identity*, so two PATs minted by the same user share one 5,000/hr bucket — and the product never says so, at author time or anywhere else. The visible consequence is that one gaggle's failure storm halts a healthy, productive gaggle instance-wide.

The runtime then amplifies it. The scheduler's quota ledger is keyed by provider name alone, with no notion of which credential the observation came from. That has two effects even when identities *are* genuinely separate:

- An exhausted window recorded from one gaggle's token gates polling and dispatch for every gaggle on that provider.
- A later reset-window observation from a *healthy* token replaces the exhausted token's window, reopening dispatch for the token that is still exhausted — so runs are admitted that will deterministically fail.

Measured impact on a two-gaggle instance over one week: 43% of the productive gaggle's failed/escalated runs carry `github_rate_limited`; 6,305 scheduler tick-skips attributed to provider-quota budget hit the productive gaggle versus 1,320 the failing one; 32 implementation runs completed the full agentic implement + local-CI pipeline and then failed at `open-pr` on exhausted quota, discarding $88.39 of model spend at the final step with no deferred publish.

This is framed as **call-pattern efficiency with options**, not as a rate-limiting architecture. The baseline read-volume work already shipped and works; what remains is a small number of targeted levers plus honest surfacing of the identity constraint. The per-identity ledger is the eventual correctness fix, not the opening move.

## Evidence

- Audit: `docs/audits/2026-08-08-gaggle-reliability/domains/audit-goobers-runs.md` — finding `github-quota-shared-user` (CRITICAL/confirmed): both gaggles' tokens authenticate as the same GitHub user (identical `X-RateLimit-Reset` timestamps across gaggles; same user ID in error text); 327/765 (43%) of the healthy gaggle's failed/escalated runs since Aug 1 carry `github_rate_limited`. Same file: 32 runs lost at `open-pr` for $88.39; rate-limited provider stages strand runs until the 45m stall reaper mislabels them "escalated" (4/4 sampled `run_stalled` journals end on a `github_rate_limited` provider error).
- Audit: `docs/audits/2026-08-08-gaggle-reliability/domains/audit-daemon-stability.md` — finding `quota-blast-radius` (HIGH/confirmed): 6,305 vs 1,320 `tick.skipped`/provider-quota events by gaggle; 2,165 `poll.shed` events since Aug 2 with `provider=github … remaining=0`; real 403 `remaining=0` responses on the *other* gaggle's repo during demand counting (491/180/79 `schedule_demand_count_failed` on Aug 5/6/7).
- `internal/localscheduler/providerquota.go:109-113` — `windows map[apiv1.Provider]providerQuotaWindow`. No credential dimension.
- `internal/localscheduler/providerquota.go:123-149` — `Record` starts a new window whenever `resetAt.After(current.resetAt)`, so a healthy token's later window replaces an exhausted token's window regardless of which credential observed it.
- `internal/localscheduler/providerquota.go:163-172` (`ExhaustedFor`) and `internal/localscheduler/conditions.go:226` — the dispatch gate consults the provider key only.
- `cmd/goobers/daemon.go:380` — exactly one `NewProviderQuotaState` per daemon, shared by every gaggle. `cmd/goobers/runnerwiring.go:1255-1263`, `:1853`, `providers/github.go:3102-3118` — header observations and exhaustion records carry no token identity.
- Already shipped, and deliberately not re-proposed here: `cmd/goobers/apireadcache.go:22-56` — disk-backed conditional-GET (ETag / `If-None-Match`) cache on the provider HTTP seam, fail-open, shared across list consumers so later stages in one scheduler evaluation reuse the first stage's snapshot. Wired on both the run path (`newCachedGitHubProvider`, ~20 call sites) and the daemon poll path (`cmd/goobers/runnerwiring.go:1723`). GitHub 304s do not count against the primary budget.
- The cache's deliberate ceiling: `cmd/goobers/apireadcache.go:34-40` — "GitHub's weak ETags are persisted but never sent in conditional requests because weak validators on label-filtered issue collections can remain unchanged when membership changes." That exclusion is a correctness fix, not an oversight, and it means the highest-volume list in the product — the label-filtered backlog query — still pays full quota on every tick.

## Proposed direction

An ordered ladder. Each rung is independently shippable and independently valuable; do not treat the last rung as a prerequisite for the first.

**Rung 1 — tell the operator their gaggles share a bucket (S, ships alone).**
At `goobers validate --check-repos` time, the repository preflight already authenticates each token. Have it record the authenticated identity (`GET /user` login, or the App installation id) and emit `QUOTA001` when two or more distinct target repos resolve to tokens with the same identity: *"gaggles X and Y authenticate as the same GitHub identity and therefore share one 5,000/hr primary quota; consider a GitHub App installation per gaggle."* Warning, never exit-code-changing. Same treatment in `goobers doctor`. This is the single highest ratio of surprise-removed to code-written in the whole ladder, and it converts an invisible architectural constraint into a config-time statement.

**Rung 2 — make the recommended escape hatch real and documented (S/M).**
GitHub App installation tokens get their own per-installation quota; PATs from one user do not. The minting seam already exists (`repo.GitHubAppAuth()`, the App token source used by the repository preflight). What is missing is the guidance: document per-gaggle GitHub App installation as *the* supported way to get independent quota, with a worked two-gaggle example, and have `QUOTA001` point at it. Without this rung, rung 1 tells operators about a problem they cannot act on.

**Rung 3 — close the remaining uncached read floor for label-filtered lists (M).**
The conditional-GET cache cannot help the backlog query, by design. Instead of weakening that correctness rule, change the call pattern: anchor backlog listing on GitHub's `since=` parameter (server-side, strong, cheap, and correct under membership change) plus a stable local index of previously-seen items, so per-tick cost tracks *change* rather than backlog size on exactly the endpoint the cache had to skip. Must be verified against label-membership changes that do not bump `updated_at` — if that class exists, fall back to a full list on a bounded cadence rather than trusting the anchor.

**Rung 4 — a declared, discoverable poll budget (M).**
Poll cadence is currently implicit. Expose an optional per-gaggle call budget / poll interval that the scheduler honours when shedding, so an operator running a large backlog on a shared bucket can trade latency for headroom deliberately. Zero-config keeps today's cadence exactly; the knob is progressive disclosure for the operator who has already hit `QUOTA001`.

**Rung 5 — key the quota ledger by credential identity (M, the eventual correctness fix).**
Change `ProviderQuotaState.windows` to key on `(provider, credential identity)`, where identity is the resolved token's authenticated principal — not the token string, and not the config ref, so two refs pointing at one PAT correctly share one window and one PAT rotated in place keeps its window. Gate and shed per identity. When identities coincide (the shared-user case), behaviour is unchanged and correct, because the bucket genuinely is shared. This removes the false-gating of healthy gaggles and, more importantly, removes the *reopening* bug where a healthy token's window resurrects an exhausted one.

**Smart defaults / zero-config behavior:** nothing in rungs 1–3 or 5 requires an operator to configure anything. A single-gaggle instance sees no new warnings (rung 1 needs two distinct target repos to fire), no new fields, and identical polling behaviour. Rung 5 is invisible when there is one identity — which is every single-gaggle instance. Only rung 4 adds surface, and it is optional with today's cadence as the default.

**Explicitly out of scope:** a rate-limiting architecture, a global token-bucket scheduler, or a cross-gaggle quota allocator. Bounded-fair cross-gaggle dispatch already shipped; the residual problems are attribution and call pattern, not scheduling policy.

## Alternatives considered

- **Re-propose conditional requests and shared list fetching.** Already shipped and wired on both the run and poll paths. Re-filing it would duplicate closed work and produce a second cache.
- **Send weak ETags on label-filtered issue lists to widen cache coverage.** This is a known correctness regression: a weak validator can return 304 while membership changed, silently hiding newly-matching issues. It was already found and fixed once; rung 3 exists precisely to get the savings without reintroducing it.
- **Key the quota ledger by config token ref instead of authenticated identity.** Cheaper, but wrong in both directions: two refs pointing at the same PAT would each believe they have a full budget, and a rotated token would lose its window. Identity is the thing GitHub actually meters.
- **Serialize gaggles onto one poll cadence to stay under the cap.** Trades a correctness problem for a throughput problem, and still fails the moment one gaggle's backlog grows.
- **Treat every 403/`remaining=0` as gaggle-local and stop sharing quota state at all.** Loses the real protection the shared ledger provides when the bucket genuinely *is* shared — which is the current production configuration.
- **Defer everything until per-identity keying lands.** Rungs 1 and 2 remove the operator's core surprise in a fraction of the effort and are not blocked by it.

## Duplicate search

Searched 2026-08-08 against `Agent-Clubhouse/Goobers` (open and closed), read-only:

Terms: `per-token quota`, `quota per token`, `rate limit ledger`, `shared token rate limit`, `conditional request etag`, `conditional GET`, `weak etag`, `poll dedup`, `provider quota`, `api call efficiency`, `rate limit in:title`, `quota in:title`, `API volume in:title`, `fairness in:title`, `gaggle in:title quota`, `budget in:title poll`, `GraphQL in:title`, `poll interval in:title`, `installation token in:title`, `GitHub App in:title quota`.

Nearest existing issues and the delta:

- **#1053 (closed COMPLETED 2026-07-22) — "Reduce daemon's baseline GitHub API READ volume (uncached per-tick list GETs)."** **This kills rungs that a naive version of this proposal would have led with.** It delivered the conditional-GET (ETag) cache and the shared response store that collapses redundant independent listings — i.e. cache-first call patterns and poll dedup are *done*, on both the run and daemon paths. Delta retained: the weak-ETag exclusion leaves label-filtered backlog lists uncached (rung 3), and #1053 is entirely about read volume for one token — it never touches identity or ledger keying.
- **#1871 (closed) — "apiReadCache trusts a 304 from GitHub's weak ETag on label-filtered issue lists, silently hiding newly-matching issues."** The correctness fix that created rung 3's ceiling. Delta: rung 3 is the way to recover those savings without reverting #1871; this issue must cite it so no one "fixes" coverage by re-enabling weak validators.
- **#1314 (closed) — "Degrade polling with a reset-aware per-provider quota budget."** Built the ledger this issue proposes to re-key. Explicitly *per-provider* by design. Delta: rung 5 changes the key; the shedding and reset-awareness it delivered stay as-is.
- **#712 (closed) — provider-quota circuit breaker; #614 (closed) — rate-limit detection and backoff; #773 (closed) — secondary rate limits; #2587 (closed) — resume-burst re-exhaustion; #2385/#2403 (closed) — `open-pr` retry on rate limit; #2687/#2688 (closed) — `github_auth_failed` circuit-breaking.** The full symptom-handling lineage: what to do *once* exhausted. Delta: none of them reduces call volume on the remaining uncached endpoint, and none of them knows which credential exhausted the budget.
- **#775 (closed COMPLETED) — "bounded-fair provider scheduling across gaggles sharing capacity"**, child of **#161 (closed) — "H4 Provider rate-limit resilience + multi-gaggle fairness."** Closest match to the cross-gaggle blast radius, and it **shrinks this proposal**: bounded-fair dispatch across gaggles sharing a token budget already exists. Delta: fairness policy assumes the ledger correctly describes *whose* budget is exhausted. The defect here is attribution — an exhausted window from token A gating token B, and a healthy token B's later window reopening token A's gate. Fair scheduling over a wrong ledger is still wrong.
- **#686 (closed) — "Short-lived credential minting seam (e.g. GitHub App installation tokens)."** Delivered the minting seam rung 2 depends on. Delta: no guidance, no worked multi-gaggle example, and no connection drawn between App installation auth and quota independence.
- **#870 (closed) — apply-verdict 422 "Can not approve your own pull request" (single-token identity).** The other consequence of one identity across gaggles, fixed at the review layer. Delta: quota, not review self-approval; but good precedent that shared identity has non-obvious downstream effects worth surfacing at config time.
- **#2656 (open) — daemon redaction registry retains every historical credential.** Adjacent credential-lifecycle concern, unrelated to quota.
- No issue found, open or closed, proposing per-token/per-identity quota ledger keying or a config-time same-identity warning. Both appear genuinely unfiled.

Upstream files issues quickly; re-run this search at filing time.

## Size and risk

**Rung 1 (`QUOTA001` same-identity warning): S.** Blast radius: `cmd/goobers/validate.go` plus one extra authenticated call per distinct token during `--check-repos`. Risk: an identity lookup that fails must degrade to silence, never to a validation failure. No migration.

**Rung 2 (App-per-gaggle guidance): S.** Docs plus a worked example; no code beyond the diagnostic's cross-reference. No migration.

**Rung 3 (`since=`-anchored backlog listing): M.** Blast radius: the backlog-query read path — the daemon's core claiming mechanism, where a staleness bug means missing a claimable item for a tick. Needs explicit staleness tests including label-membership changes that do not bump `updated_at`, and a bounded full-list fallback. Should land as its own measured PR, never bundled.

**Rung 4 (declared poll budget): M.** Additive instance/gaggle config surface; default preserves current cadence exactly. Interacts with the DSL 2.0 epic only if the knob is placed on workflow surface rather than gaggle/instance config — prefer gaggle/instance config so it is not gated on the 1.4→2.0 migration.

**Rung 5 (per-identity ledger key): M, moderate blast radius.** Touches the dispatch gate every workflow passes through. Behaviour is provably unchanged when all credentials share one identity, which is the current production configuration and every single-gaggle instance — so the migration risk is confined to instances that already have independent identities, where today's behaviour is the bug. Requires an identity resolution that is cached and failure-tolerant: an unknown identity must fall back to the current per-provider key rather than to an unguarded dispatch.
