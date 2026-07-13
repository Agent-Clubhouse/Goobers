# Design: Tutor workflow (journal-mining → config PRs) — V1 epic #36

> Status: **Draft for review** · Area prefix: `TUT` · Milestone: **V1**
> Requirements: [`docs/requirements/tutor.md`](../../requirements/tutor.md) ·
> Architecture: [`docs/ARCHITECTURE.md`](../../ARCHITECTURE.md) §4 (journals), §8
> (telemetry), §9 (security), §12 (roadmap)
>
> This is the detailed-design artifact for epic **#36**. The dispatchable work
> items (T1–T5) each link back to the correspondingly-named section here.

## 1. Verdict

**V0 primitives are ~80% sufficient.** The Tutor is structurally the **nomination
workflow** ([`internal/workflow/testdata/shipped/work-nomination.yaml`](../../../internal/workflow/testdata/shipped/work-nomination.yaml))
with a different output target: a schedule-triggered *producer* workflow whose
deterministic step queries telemetry and whose agentic step, instead of filing
*issues*, opens a **PR against `config/`**. `docs/requirements/tutor.md` has
TUT-Q1–Q4 already resolved, so this epic is build-out, not open design.

Three things are genuinely new and carry the risk: **(a)** cross-run *aggregate*
telemetry queries (today's rollup queries are mostly per-run), **(b)** a cross-run
**journal-evidence handoff** into the agentic diagnosis step, and **(c)** the
**config-only write-boundary**, whose *structural* enforcement depends on epic #35.

## 2. Scope boundary

**In scope (V1, tiers 1–2):** the Tutor as a standard scheduled workflow reading the
local run journals + `telemetry.db` rollup and opening `config/`-scoped PRs, gated by
ordinary `config`-repo PR governance.

**Out of scope — V2, do NOT build:** ADX as a telemetry source, tier-3 `infra`-repo
identity separation, Entra ID auth. Where `tutor.md` says "ADX at tier 3," read "local
`telemetry.db` rollup" for this epic. Per PO: **no V2 work at all.**

## 3. Architecture

### 3.1 Reuse map (what already exists)

| Need | V0 primitive | Status |
|------|--------------|--------|
| Workflow model, schedule trigger | Workflow DSL + `internal/localscheduler` (`type: schedule`, 5-field cron / `@every`) | reuse unchanged |
| Producer-workflow shape (deterministic gather → agentic act) | `work-nomination.yaml` | direct template |
| Telemetry read | `internal/telemetry/rollup` (`runs`, `stage_attempts`, `gate_verdicts`, `provider_mutations`, `run_errors`, `spans`, `span_events`) | extend (see T2) |
| Capability-scoped injection, fail-closed | `internal/capability` (#74), envelope capability grants (SEC-042) | extend (new caps) |
| Agentic file-artifact output | harness declared-output-file seam (#73) | reuse |
| PR output | GitHub PR provider (#13) | reuse, path-confined |
| Evidence resolution | journal `ContextPointer` resolve (read-only, digest-verified, path-escape-refused) | extend to cross-run |

### 3.2 Data flow

```
schedule (cron)
  → [deterministic] telemetry-query: cross-run aggregates over telemetry.db
        → emits candidate-findings artifact (metrics + journal pointers for flagged runs)
  → [agentic] analyst: resolve flagged journals/traces read-only, diagnose, draft change
  → [agentic] config-author: open PR confined to config/**  (evidence links in body)
  → journal: findings + rationale recorded (TUT-007)
gated by: standard config-repo PR governance (branch protection / CODEOWNERS) — no in-product gate
```

The deterministic→agentic handoff is the **nomination pattern**: the deterministic
step writes a declared-output-file artifact (#73); the agentic step consumes it. The
only extension is that the artifact must carry **resolvable journal pointers** for the
runs it flagged, so the diagnosis step can read those runs' traces read-only.

## 4. Missions (dispatchable, single-PR-sized)

Each becomes one backlog issue linking to this section.

### T1 — Tutor workflow definition + goober defs (config)
- `tutor.yaml` shipped workflow: schedule producer mirroring `work-nomination.yaml`;
  `analyst` + `config-author` goober definitions tuned to this repo (Go, `make ci`,
  docs conventions); wire into the dogfood self-host config (#28, merged).
- **Seams:** Workflow DSL, scheduler, config-as-code layout.
- **Satisfies:** TUT-001, TUT-002, TUT-004 (output modeling).
- **Test plan:** definition validates/compiles; schedule expression parses and fires;
  a run produces a config-PR artifact against a fixture config repo (fake harness);
  journal shows the run start→finish.

### T2 — Cross-run detection queries (largest; the deterministic half of TUT-003)
- Extend `internal/telemetry/rollup` + `goobers telemetry-query` with **cross-run
  aggregates**: failure-rate-by-test, repeated-gate-verdict counts, retry-count
  distribution, reviewer-repetition. Emit a structured **candidate-findings artifact**.
- Thresholds are **config-tunable** in the tutor goober def with sane defaults (see OQ-2).
- **Seams:** `rollup/query.go`, `rollup/schema.go` (read-only; no schema change expected —
  the columns already exist), CLI `telemetry-query`.
- **Satisfies:** TUT-003 (metrics half).
- **Test plan:** unit tests per aggregate over a **seeded `telemetry.db` fixture** with
  known failure rates → expected candidate set; threshold-boundary tests (just-under /
  just-over); artifact output is deterministic for a fixed input.

### T3 — Journal-evidence handoff
- Candidate-findings artifact carries resolvable **journal artifact pointers** for each
  flagged run. Add a `journal:read` (cross-run) capability so the agentic diagnosis step
  resolves them read-only (digest-verified, path-escape-refused). Builds on #73 + #74.
- **Seams:** journal `ContextPointer` resolution, `internal/capability`, envelope.
- **Satisfies:** TUT-003 (agentic half), TUT-007 (evidence links).
- **Test plan:** flagged-run pointers resolve read-only + digest-verified; path-escape
  refused; **capability fail-closed** when `journal:read` is undeclared; evidence links
  land in the PR body.

### T4 — Config-only write-boundary
- Confine the Tutor's PR to `config/**`: path-scoped diff + CODEOWNERS / branch
  protection on those paths + operator docs. The *structural* credential enforcement
  (a token that structurally cannot push platform changes) is deferred to **#35**; this
  mission ships path-scoping + governance + the negative test.
- **Seams:** PR provider, config-as-code layout, `#35` (hard dependency for structural
  enforcement).
- **Satisfies:** TUT-005, TUT-006, TUT-009.
- **Test plan:** a PR touching any non-`config/` path is rejected; CODEOWNERS present on
  `config/**`; negative test proves platform paths are unreachable through this workflow.

### T5 — Change-efficacy assessment (fast-follow, SHOULD)
- Segment runs by `workflow_digest` / config version **before vs after** a merged Tutor
  PR; compare aggregate deltas; churn-guard so a definition is not repeatedly flip-flopped.
- **Substrate exists:** the `runs` table already records `workflow_digest` + `workflow_version`.
- **Seams:** rollup queries, tutor workflow.
- **Satisfies:** TUT-008 (SHOULD).
- **Sequence:** after T1–T4 prove the loop end-to-end.
- **Test plan:** seeded before/after telemetry → correct helped/regressed verdict; churn-
  guard prevents re-flipping the same definition within a window.

## 5. End-to-end / integration test

Extend the walking-skeleton pattern (#29): seed a **recurring failure** into a fixture
`telemetry.db`, run `tutor.yaml` through the **real local runner + fake harness**, and
assert (a) a `config/**`-confined PR artifact is produced, and (b) journal findings with
resolvable evidence links are recorded. Journal-only assertions, no network.

## 6. Dependencies

- **#35** (per-goober credential injection) — required for the *structural* half of the
  write-boundary (T4). T4 ships path-scope + governance without it; structural
  enforcement lands when #35 does.
- **#73** (agentic declared-output-file artifact) — the deterministic→agentic handoff. *Landing.*
- **#74** (canonical capability registry) — new `journal:read` / `config:write` constants. *Merged.*

## 7. Open questions (for PM / PO)

- **OQ-1 — write-boundary mechanism:** in the single-repo dogfood, `config/` is a
  *directory*, not a separate repo, so the tier-1/2 boundary is **path-scoped PR +
  CODEOWNERS** until #35 adds credential scoping. Acceptable as the V1 mechanism, with
  structural cred-enforcement riding on #35? *(Recommend: yes.)*
- **OQ-2 — detection thresholds:** config-tunable in the tutor goober def, or hardcoded
  defaults? *(Recommend: config-tunable, sane defaults.)*
- **OQ-3 — no in-product gate:** per TUT-006 humans gate purely via `config`-repo PR
  review, so the Tutor's own runs get **no reviewer-goober gate**. Confirm.
- **OQ-4 — T5 timing:** V1-must or fast-follow? `tutor.md` says SHOULD. *(Recommend: fast-follow.)*
