# Design: Tutor workflow (journal-mining → config PRs) — V1 epic #36

> Status: **Draft for review** · Area prefix: `TUT` · Milestone: **V1**
> Requirements: [`docs/requirements/tutor.md`](../../requirements/tutor.md) ·
> Architecture: [`docs/ARCHITECTURE.md`](../../ARCHITECTURE.md) §4 (journals), §8
> (telemetry), §9 (security), §12 (roadmap)
>
> This is the detailed-design artifact for epic **#36**. The dispatchable work
> items (T1–T5) each link back to the correspondingly-named section here.

## 1. Verdict

**The V0 primitive *shapes* are right; several are not yet wired** (re-baselined
2026-07-13 after the architect post-merge review — see the V0.1 remediation
milestone, epic #130). The Tutor is structurally the **nomination workflow**
([`internal/workflow/testdata/shipped/work-nomination.yaml`](../../../internal/workflow/testdata/shipped/work-nomination.yaml))
with a different output target: a schedule-triggered *producer* workflow whose
deterministic step queries telemetry and whose agentic step, instead of filing
*issues*, opens a **PR against the instance config root**. `docs/requirements/tutor.md`
has TUT-Q1–Q4 already resolved, so this epic is build-out, not open design.

Three things are genuinely new and carry the risk: **(a)** cross-run *aggregate*
telemetry queries (today's rollup queries are mostly per-run), **(b)** a cross-run
**journal-evidence handoff** into the agentic diagnosis step, and **(c)** the
**config-only write-boundary**, whose *structural* enforcement depends on epic #35.

**V0.1 prerequisites (hard):** #126 (span pipeline unwired), #127/#128 (rollup
robustness + ingest completeness — gate repass/escalation/rationale are dropped at
ingest today), #131/#132 (the `telemetry-query`/provider stage subcommands the
template workflow calls do not exist yet), #121 (agentic stages cannot dereference
artifact pointers). Companion substrate design: `observability-substrate.md`
(TEL-040..045; missions #143–#150).

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
| Producer-workflow shape (deterministic gather → agentic act) | `work-nomination.yaml` | reuse — `goobers telemetry-query` emits the versioned candidate-findings connector artifact (#148) |
| Telemetry read | `internal/telemetry/rollup` (`runs`, `stage_attempts`, `gate_verdicts`, `provider_mutations`, `run_errors`, `spans`, `span_events`) | extend (see T2) — after #127/#128; span tables are empty in production until #126 |
| Capability-scoped injection, fail-closed | `internal/capability` (#74), envelope capability grants (SEC-042) | extend (new caps) |
| Agentic file-artifact output | harness declared-output-file seam (#73) | reuse — with the #120 containment fix |
| PR output | GitHub PR provider (#13) | reuse, path-confined — provider is not constructed on the live runner path until #131/#132 |
| Evidence resolution | journal `ContextPointer` resolve (read-only, digest-verified, path-escape-refused) | **build the agentic read surface first (#121)** — pointer resolution is inert for agentic consumers today — then extend cross-run (T3) |

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
- `tutor.yaml` shipped workflow: schedule producer mirroring `work-nomination.yaml`
  (on the #148 connector stage, not a bespoke gather command); `analyst` +
  `config-author` goober definitions tuned to this repo (Go, `make ci`, docs
  conventions); wire into the dogfood self-host config (#28, merged).
- The analyst/config-author instructions enumerate the **full TUT-011 proposal
  catalog**: add test/gate stages; change a goober's skills, instructions, stage
  `goal` prompts, or **model** (needs `Goober.spec.model` — #150, hard dependency
  for that proposal family); add or remove entire workflows to cover gaps; remove
  or loosen noisy gates.
- **Seams:** Workflow DSL, scheduler, config-as-code layout.
- **Satisfies:** TUT-001, TUT-002, TUT-004 (output modeling), TUT-011.
- **Test plan:** definition validates/compiles; schedule expression parses and fires;
  a run produces a config-PR artifact against a fixture config repo (fake harness);
  journal shows the run start→finish.

### T2 — Cross-run detection queries (largest; the deterministic half of TUT-003)
- Extend `internal/telemetry/rollup` + the `telemetry-query` connector (#132/#148)
  with **cross-run aggregates covering all four TUT-010 families** (substrate design
  §D7): **failure patterns** (failure-rate by stage/test, error-code clustering),
  **gate noise** (gates that never fail; reviewer-verdict repetition), **coverage
  gaps** (workflows never triggered, stages never reached), and **waste**
  (duration/token/cost percentiles, retry waste, always-eventually-pass repass
  loops). Emit findings in the **versioned candidate-findings schema** (#148), not a
  tutor-private format.
- **Sequencing:** failure/noise/coverage families ship on V0 rollup data once
  #127/#128 land (gate repass/escalation/rationale are dropped at ingest today, so
  "the columns already exist" does not hold until #128); the waste family follows
  usage accounting (#144).
- Thresholds are **config-tunable** in the tutor goober def with sane defaults (see OQ-2).
- **Seams:** `rollup/query.go`, `rollup/schema.go`, connector stage (#148).
- **Satisfies:** TUT-003 (metrics half), TUT-010.
- **Test plan:** unit tests per aggregate over a **seeded `telemetry.db` fixture** with
  known failure rates → expected candidate set; threshold-boundary tests (just-under /
  just-over); artifact output is deterministic for a fixed input.

### T3 — Journal-evidence handoff
- Candidate-findings artifact carries resolvable **journal artifact pointers** for each
  flagged run. **T3 ships the agentic read surface itself** — today pointer resolution
  is inert for agentic consumers (the prompt renders bare names; the journal lives
  outside the worktree, #121): resolve declared pointers into a read-only location in
  the stage workspace (or a capability-gated `goobers journal cat`), building on the
  single-run fix in #121, then scope it cross-run behind a new `journal:read`
  capability (digest-verified, symlink-refusing, path-escape-refused per #120).
  Builds on #73 + #74 + #121.
- **Seams:** journal `ContextPointer` resolution, `internal/capability`, envelope,
  harness context materialization.
- **Satisfies:** TUT-003 (agentic half), TUT-007 (evidence links).
- **Test plan:** flagged-run pointers resolve read-only + digest-verified; path-escape
  refused; **capability fail-closed** when `journal:read` is undeclared; evidence links
  land in the PR body.

### T4 — Config-only write-boundary
- Confine the Tutor's PR to the **instance's configured config root** — not a
  hardcoded `config/**`: in the dogfood instance the config root is `selfhost/`, and
  arbitrary V1 instances may back config with a separate repo. Path-scoped diff +
  CODEOWNERS / branch protection on that root + operator docs. The *structural*
  credential enforcement (a token that structurally cannot push platform changes) is
  deferred to **#35**; this mission ships path-scoping + governance + the negative test.
  **Before enabling on the dogfood repo:** CODEOWNERS must actually be in place on the
  config root, since Tutor PRs land in the same repo as platform code here.
- **Seams:** PR provider, instance config (config-root path), `#35` (hard dependency
  for structural enforcement).
- **Satisfies:** TUT-005, TUT-006, TUT-009.
- **Test plan:** a PR touching any path outside the configured config root is rejected
  — exercised against a **non-default** root; CODEOWNERS present on the root; negative
  test proves platform paths are unreachable through this workflow.

### T5 — Change-efficacy assessment (fast-follow, SHOULD)
- Segment runs by `workflow_digest` / config version **before vs after** a merged Tutor
  PR; compare aggregate deltas; churn-guard so a definition is not repeatedly flip-flopped.
- **Substrate exists:** the `runs` table already records `workflow_digest` + `workflow_version`.
- **Assumption:** before/after segmentation reads within the telemetry retention
  window (#149) — comparisons spanning a prune boundary are best-effort.
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

- **V0.1 remediation (epic #130):** #126 (span pipeline), #127/#128 (rollup
  robustness/ingest), #131/#132 (stage subcommands + provider on the live path),
  #121 (agentic artifact read surface — extended by T3), #120 (declared-file
  containment). The Tutor's data path does not exist until these land.
- **Substrate missions:** #148 (connector stage + candidate-findings schema — T2's
  output format), #144 (usage accounting — T2's waste family), #150
  (`Goober.spec.model` — T1's model-change proposals), #149 (retention — T5's window
  assumption).
- **#35** (per-goober credential injection) — required for the *structural* half of the
  write-boundary (T4). T4 ships path-scope + governance without it; structural
  enforcement lands when #35 does.
- **#73** (agentic declared-output-file artifact) — the deterministic→agentic handoff. *Merged.*
- **#74** (canonical capability registry) — new `journal:read` / `config:write` constants. *Merged.*

## 7. Open questions — RESOLVED (architect review, 2026-07-13; see PR #100 review comment)

- **OQ-1 — write-boundary mechanism:** **Resolved: yes** — path-scoped PR + CODEOWNERS
  is the V1 mechanism; structural credential scoping rides #35. Additionally required
  before enabling on the dogfood repo: CODEOWNERS actually in place on the config root
  (Tutor PRs land in the same repo as platform code here). Boundary is the *configured*
  config root, not literal `config/` (see T4).
- **OQ-2 — detection thresholds:** **Resolved:** config-tunable in the tutor goober
  def, sane defaults.
- **OQ-3 — no in-product gate:** **Confirmed** — TUT-006 stands; the Tutor's own run
  journal + TUT-007 evidence links are the audit trail.
- **OQ-4 — T5 timing:** **Resolved:** fast-follow, within the #149 retention window
  (see T5 assumption).
