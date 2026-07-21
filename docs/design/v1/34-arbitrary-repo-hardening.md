# Design: Arbitrary-repo hardening + multi-gaggle instances — V1 epic #34

> Status: **Draft for review** · Area prefix: `GAG` · Milestone: **V1**
> Requirements: [`docs/requirements/gaggle.md`](../../requirements/gaggle.md),
> [`docs/requirements/instance.md`](../../requirements/instance.md) ·
> Architecture: [`docs/ARCHITECTURE.md`](../../ARCHITECTURE.md) §6
>
> Detailed-design artifact for epic **#34**. The dispatchable work items (H1–H5)
> each link back to the correspondingly-named section here.

## 1. Verdict

**This is hardening, not greenfield — most primitives already exist.** V0 shipped a
multi-gaggle-*capable* config model and provider rate-limit handling; what's missing is
the runtime scoping and the UX/docs to make it *trustworthy over arbitrary tier-1/2
repos*. Two facts from the V0 code set the shape:

- **Multi-gaggle config resolution already works.** The loader reads `[]apiv1.Gaggle`
  pruned to a manifest ([`internal/instance/configdir.go`](../../../internal/instance/configdir.go)),
  and [`cmd/goobers/runnerwiring.go`](../../../cmd/goobers/runnerwiring.go) resolves each
  workflow → its gaggle → per-gaggle goober instruction paths
  (`config/gaggles/<gaggle>/goobers/...`).
- **Provider rate-limit handling already exists.** `providers/seams.go` defines
  `RateLimitObserver` / `RateLimitEvent` (Retry-After, GitHub secondary/abuse limit) and
  `providers/github_issues.go` honors `X-RateLimit-*` / `Retry-After`. *(Caveat from the
  2026-07-13 review: the provider has **no pagination and no 5xx retry** — #139 — and
  mutation idempotency gaps — #140. H4 builds on those fixes rather than re-doing them.)*

**The load-bearing gap is runtime scoping (GAG-011).** The instance layout is **flat**:
`RunsDir` = `<root>/runs`, `WorkcopiesDir` = `<root>/workcopies`, `TelemetryDB` =
`<root>/telemetry.db` ([`internal/instance/instance.go`](../../../internal/instance/instance.go)) —
none scoped per gaggle. Telemetry is the exception: the `runs` rollup table already
carries a `gaggle` column, so it can stay one shared DB with gaggle-filtered queries.

## 2. Scope boundary

**In scope (V1, tiers 1–2):** point one daemon at several arbitrary repos/backlogs with
per-gaggle runtime isolation, actionable config-validation for foreign layouts, rate-limit
resilience under bigger backlogs, and an end-to-end onboarding guide.

**Out of scope — V2:** tier-3 provisioning, cluster/multi-node daemons, cross-instance
federation. Per-goober *credential* scoping is **#35** (referenced, not built here). Auth
is **#38**.

## 3. Architecture

### 3.1 Reuse map

| Need | V0 primitive | Status |
|------|--------------|--------|
| Multi-gaggle config model | `configdir.go` manifest-pruned `[]Gaggle`; `runnerwiring.go` gaggle resolution | reuse |
| Rate-limit backoff | `providers/seams.go` observer + `github_issues.go` | reuse; extend for fairness (H4) |
| Telemetry per gaggle | `runs.gaggle` column in the rollup schema | reuse; add gaggle-filtered queries |
| Config validation | `goobers validate` (counts) | extend to actionable diagnostics (H1) |
| Runtime storage | flat `runs/`, `workcopies/`, `telemetry.db` | **scope per gaggle (H2)** |
| Claim ledger | `internal/localscheduler` dedupe/claim keys | **namespace by gaggle (H2/H3)** |

### 3.2 Target instance layout (GAG-011)

```
<instance-root>/
  instance.yaml
  config/gaggles/<gaggle>/...           # already per-gaggle
  gaggles/<gaggle>/runs/                # NEW: per-gaggle run journals
  gaggles/<gaggle>/workcopies/          # NEW: per-gaggle managed working copies
  scheduler/                            # instance-wide claim ledger, keyed (gaggle,provider,externalID)
  telemetry.db                          # shared, gaggle-tagged (query-filtered)
```

## 4. Missions (dispatchable, single-PR-sized)

### H1 — Config validation UX for foreign repos
- Enrich `goobers validate`: actionable diagnostics for a foreign layout — unbound
  workflows (gaggle named by a workflow but not defined), manifest/gaggle mismatches,
  capability-string typos (validate against the #74 registry), missing goober
  instructions, target-repo reachability check. Human-readable "here's what's wrong and
  where" output. *(Complementary to #124, which makes `validate` actually compile
  definitions and closes the admission holes — H1 is the UX layer on top; coordinate.)*
- **Seams:** `internal/instance/configdir.go`, `cmd/goobers/validate.go`, `internal/capability`.
- **Test plan:** table of malformed foreign-layout fixtures → expected diagnostic + exit
  code; a valid foreign layout passes; capability typo is caught.

### H2 — Per-gaggle runtime scoping (GAG-011) — load-bearing
- Scope `runs/` and `workcopies/` under `gaggles/<gaggle>/`; namespace the claim ledger
  key by `(gaggle, provider, externalID)` so two gaggles can't collide on the same issue
  number; keep telemetry a single gaggle-tagged DB. Migration note for existing flat roots.
- **Seams:** `internal/instance/instance.go` (Layout), runner wiring, `internal/localscheduler` (ledger).
- **Test plan:** two gaggles, same external issue id → two independent claims/runs, no
  workcopy collision, journals land in separate dirs; telemetry queries filter by gaggle;
  a flat legacy root still loads (or migrates) cleanly.

### H3 — Multi-gaggle daemon loop
- `goobers up` serves the scheduler across **all** manifest gaggles concurrently, each
  with its own readiness (`maxConcurrentRuns`/`maxRunsPerHour`) and claim scoping; clean
  drain across gaggles on shutdown.
- **Seams:** `cmd/goobers/up.go`, `internal/localscheduler`, runner wiring.
- **Test plan:** daemon with 2+ gaggles dispatches per-gaggle without cross-talk; per-gaggle
  concurrency caps honored independently; graceful drain completes in-flight runs in every gaggle.

### H4 — Provider rate-limit resilience + multi-gaggle fairness
- Under a bigger backlog and a token budget shared across gaggles: fair scheduling so one
  hot gaggle can't starve others; harden backoff on secondary (abuse) limits; surface
  `RateLimitEvent`s to telemetry for observability.
- Local fan-out uses work-conserving hierarchical round-robin: ready gaggles take one
  dispatch turn per pass, while the existing starvation-aged workflow order applies
  within each gaggle. With `G` continuously ready gaggles, a gaggle waits behind at most
  `G-1` successful dispatches by other gaggles once shared capacity becomes available.
  Gaggles without ready work are omitted rather than reserving a share, and `G=1`
  preserves the existing single-gaggle order.
- **Seams:** `providers/seams.go`, `providers/github_issues.go`, scheduler dispatch.
- **Test plan:** simulated 429/Retry-After + secondary-limit responses → correct backoff;
  fairness test (two gaggles, constrained budget → neither starves beyond a bound);
  rate-limit events recorded to telemetry.

### H5 — Onboarding guide (arbitrary repo, end-to-end)
- Operator guide: point an instance at any tier-1/2 repo — tokens/scopes, `goobers init`
  against a foreign repo, label taxonomy, multi-gaggle `instance.yaml`, what each cycle
  does, how to observe and stop. Generalizes the #28 dogfood guide.
- **Seams:** docs; exercises H1–H4.
- **Test plan:** a maintainer follows the guide against a second real repo and reaches a
  curation cycle + one implementation PR; doc-lint/link-check in CI.

## 5. End-to-end / integration test

Two-gaggle instance over two fixture repos through the **real local runner + fake harness**:
assert per-gaggle journals/workcopies, independent claim ledgers, gaggle-filtered telemetry,
and no cross-gaggle contamination. Journal-only assertions, no network.

## 6. Dependencies

- **#35** (per-goober credential injection) — a real multi-repo instance wants per-gaggle
  credentials; H4's token model assumes one shared token in V1 and notes the #35 upgrade path.
- **#38** (auth) — team instances; not required for single-operator multi-gaggle.

## 7. Open questions (for PM / PO)

- **OQ-1 — telemetry store:** keep one shared gaggle-tagged `telemetry.db` (recommend —
  the `gaggle` column already exists) vs. per-gaggle DBs? *(Recommend: shared + filtered.)*
- **OQ-2 — shared-repo workcopies:** if two gaggles target the **same** repo, isolate a
  workcopy per gaggle (safer) or share one (cheaper)? *(Recommend: isolate per gaggle.)*
- **OQ-3 — credential model in V1:** one instance token shared across gaggles, or per-gaggle
  credentials now (pulls #35 forward)? *(Recommend: shared token in V1, per-gaggle via #35.)*
- **OQ-4 — legacy flat roots:** auto-migrate an existing flat instance root to the per-gaggle
  layout, or require re-init? *(Recommend: auto-migrate with a one-time move + journal note.)*
