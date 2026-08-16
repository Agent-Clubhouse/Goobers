# Design: EvalSuite CI gating, baseline management & alerting

> Status: **draft — proposed** (research-phase deliverable; not yet wired to a runner)
> Area prefix: evals
> Related: #2662 (EvalSuite epic), #2663 (DSL & schema), #2664 (judge harness),
> #2665 (sandbox/adapter API), #2666 (adapter prototype), #2667 (runner
> integration), #2668 (this issue), #2669 (docs & onboarding)
> Touches (once #2667 lands): `evals/`, `.github/workflows/evals-gate.yml`,
> `.github/CODEOWNERS`

## 1. Problem

EvalSuite (#2662) gives Goobers a way to run deterministic, reproducible
evaluations of agentic workflows — side-by-side and shadow comparisons of a
baseline version against a candidate, scored by deterministic checks and LLM
judges (see the judge design and DSL drafted alongside the research prototype:
`eval_schema.json`, `EVALS_JUDGE_DESIGN.md`). What that research phase
deliberately left out, per its own scope doc, is what happens with a suite
*result*: whether it can block a merge, where the "known-good" comparison
point lives, and who finds out when a candidate regresses.

This doc defines those three things — CI gating rules, baseline storage, and
alerting/escalation — as a design ready for #2667 (runner integration) to
implement against, and #2669 (docs) to link from onboarding. It does not add
a working runner or a required CI check: EvalSuite has no runner in this repo
yet (#2665–#2667 are still open), so wiring a *required* gate today would
either have nothing to run or would silently no-op, which is worse than not
having a gate. The example workflow in §6 is real, lints cleanly, and follows
this repo's established pattern for a not-yet-provisioned automation (see
`provider-fixture-drift.yml` / `docs/guides/provider-fixture-drift.md`): it
exists, it is `workflow_dispatch`-only, and it fails loudly with a clear
"blocked on #2667" message rather than pretending to gate anything.

This is a distinct, higher-level gate from `evals-tests.yml` (landed
alongside #2670–#2672), which already runs `evals/`'s Python schema and unit
tests (`pytest`, path-filtered to `evals/**`) on every push and PR — that one
protects the DSL and judge-harness code itself, and needs no design doc; it's
an ordinary test suite. What's missing, and what this doc defines, is the
gate one level up: given a *runner* that can execute a suite's scenarios
against real (or mocked) workflow stages and score the result, what turns
that score into a merge decision. `evals-tests.yml` already covers the "is
this code correct" question; this design covers "did this change regress
workflow behavior."

## 2. Terms

- **Suite**: one `EvalSuite` JSON document (`suite_name`, `description`,
  `scenarios[]`), analogous to a test file.
- **Scenario**: one case in a suite (`id`, `mode`, `input`, `stages[]`,
  `expected`, `judge`, `tags[]`). `mode` is `side-by-side`, `shadow`,
  `single`, or `synthetic`.
- **Baseline**: the frozen, previously-approved output for a scenario —
  what the candidate is compared against.
- **Candidate**: the output produced by the version under evaluation (a PR's
  branch, or a scheduled run against `main`).
- **Judge**: whatever scores a candidate — a deterministic checker, an LLM
  judge, a classifier, or a human. Produces a `score` in `[0, 1]` and a
  `pass` boolean once compared against the scenario's threshold.
- **Regression**: a scenario whose candidate result crosses from acceptable
  to unacceptable relative to its baseline (defined precisely in §3).

## 3. CI gating rules

### 3.1 What triggers a gate run

Once a runner exists, the gate workflow runs:

- on every pull request that touches `evals/**` (suite definitions, DSL
  schema, judge templates, or the runner itself) — the PR-time check;
- on a schedule against `main` (proposed: nightly) — catches drift from
  environment, model, or tool changes that no PR triggered, the same
  motivation `provider-fixture-drift.yml` has for polling an external
  contract;
- via `workflow_dispatch` for a manual re-run.

PR-time gating gets `evals/**` as its path filter, not "always run": most PRs
don't touch evaluated workflows, and a suite run is not free (LLM judge calls
cost tokens and wall-clock). A PR that changes reference-workflow or gaggle
behavior evaluated by an existing suite should tag the suite in its
`metadata` so the path filter can include the workflows it covers — that
mapping is a #2667 runner concern, not a CI-workflow concern.

### 3.2 Pass/fail per scenario

A scenario's outcome is decided by its judge's score against its declared
threshold, exactly as designed in `EVALS_JUDGE_DESIGN.md`:

| Score range | Outcome |
|---|---|
| `>= threshold` (default `0.8`) | **Pass** |
| `[0.6, threshold)` | **Gray zone** — routes to required human review, does not auto-pass or auto-fail (§3.4) |
| `< 0.6` | **Fail** |
| any range, `tags` includes `safety-critical` | threshold raised to `0.95`; no gray zone — below it is always a hard fail (§3.4) |

Deterministic stages are the one place variance never applies: two
deterministic stages given the same seed either produce byte-identical
output or they don't. A deterministic-stage mismatch is always a hard
failure, regardless of any judge score on the scenario overall — mirroring
`providerfixtures`' split between "contract" (structural, always-fail) and
"drift" (content, reviewed). A deterministic stage that is *supposed* to
vary belongs in an `agentic` stage or needs a documented reason it's
deterministic-in-name-only; "it's usually fine" is not a passing contract.

### 3.3 Regression vs. acceptable variance

A **regression** is: a scenario that passed against the previous baseline and
now fails, or a deterministic-stage output that no longer matches its
baseline at all. Both are hard gate failures — no variance band, because
there's nothing to have variance about (the baseline is exact).

**Acceptable variance** applies only to judged (non-deterministic) scores: a
scenario still above threshold but with a score drop of more than 0.05 from
its last recorded baseline score is *not* a gate failure, but the gate
workflow flags it (in its summary output and PR comment, §4) as
"score drifted, still passing" so a reviewer can decide whether it's noise
or the leading edge of a real regression before it crosses the threshold on
a later run. This is deliberately informational, not blocking: gating on
every score wobble inside the passing band would make the check as noisy as
the flake problem `docs/guides/flake-management.md` already solved for
ordinary tests, and this repo's flake policy is zero anonymous retries — a
gate that flaps on judge noise invites exactly the "just rerun it" habit
that policy exists to prevent. If a scenario's judge is noisy enough to flap
across the pass/fail line on unchanged input, that's a defect in the judge
or the scenario's threshold, filed the same way a flaky test is (§5.2), not
something CI silently absorbs.

### 3.4 Gray zone and safety-critical: required human review

A gray-zone or safety-critical-failing scenario does not resolve
automatically in either direction. The gate job reports a **non-passing,
non-simply-failing** status (GitHub Actions "neutral"/annotated-failure, not
a silent pass) and requires an approving review from the suite's declared
owner (§5.3) before merge — the same mechanism CODEOWNERS already uses for
`/reference-workflows/` (`docs/guides/tutor-write-boundary.md`). This is
the CI-side half of the human-in-the-loop sampling `EVALS_JUDGE_DESIGN.md`
already specifies; the annotation UI/queue itself is out of scope here and
tracked under #2669.

### 3.5 No anonymous retries

Consistent with `docs/guides/flake-management.md`, the gate workflow must
never auto-retry a failed or gray-zone scenario. A judge result that
disagrees between two dispatches of the same commit is a signal to fix the
scenario's determinism (tighten `seed`, mock the tool, or narrow what the
judge is asked to score), not something to paper over with a rerun loop.

## 4. Baseline storage model

### 4.1 Location and format

One committed baseline file per suite:

```
evals/testdata/baselines/<suite_name>.json
```

directly analogous to `test/providers/testdata/github_contract.json` — a
hermetic, reviewed fixture, not something CI regenerates on its own. Each
baseline file is the last-approved run report shape already produced by the
prototype runner (`runs/<suite_name>_report.json`), trimmed to just the
`baseline`-role fields the gate needs to diff against, plus provenance:

```json
{
  "suite_name": "mvp-evals",
  "suite_schema_version": "1.0.0",
  "approved_at": "2026-08-08T00:00:00Z",
  "approved_by_pr": "https://github.com/Agent-Clubhouse/Goobers/pull/NNNN",
  "runs": [
    {
      "id": "scenario-1",
      "stages": [{ "name": "summarize", "output": "..." }],
      "judge_score": 0.91
    }
  ]
}
```

### 4.2 Update process

A baseline changes only through an ordinary, reviewed PR that replaces the
checked-in file with a new candidate report — never an automated commit from
the gate workflow. This is the same rule `docs/guides/provider-fixture-drift.md`
states explicitly: *"do not accept a new baseline merely to make this step
green."* Bumping a baseline is a judgment call (the new candidate really is
the new correct behavior) and belongs in a human-reviewed diff, not a bot
commit — `git diff --no-index` against the old baseline is the reviewer's
primary tool, exactly as it is for provider fixtures.

### 4.3 Retention

- **Baselines** (`evals/testdata/baselines/*.json`): retained indefinitely
  via git history. Every commit that touches a baseline is itself a
  retained, diffable, revertible snapshot — there is no separate expiry
  policy to manage, and reverting a bad baseline update is `git revert`.
- **Run artifacts** (full per-run report JSON, candidate outputs, judge
  rationale): uploaded as workflow artifacts with `retention-days: 30`,
  matching the convention already used by `flake-watch.yml` and
  `provider-fixture-drift.yml`. Thirty days covers a sprint's worth of
  investigation into a filed regression issue (§5) without accumulating
  unbounded storage for data nobody is looking at.
- **Versioning**: a baseline file embeds `suite_schema_version`. When
  `eval_schema.json`'s version changes incompatibly, every baseline built
  against the old version is stale by construction and must be regenerated
  and re-reviewed — the gate should refuse to diff a candidate against a
  baseline on a mismatched schema major version rather than produce a
  meaningless comparison.

## 5. Alerting rules & owner escalation

### 5.1 PR-time: the failing check is the alert

For the common case — a PR's own gate run fails — no separate notification
mechanism is needed. The required status check itself is the alert, the same
philosophy `docs/guides/provider-fixture-drift.md` states for its own
gate ("the failing CI check is the alert"). The gate job additionally posts
one PR comment (updated in place on rerun, not reposted) summarizing:
per-scenario pass/fail/gray-zone, the score-drift list from §3.3, and a link
to the uploaded run-artifact for anyone who wants the full judge rationale.

### 5.2 Scheduled/main-branch regressions: a fingerprinted issue ledger

A regression caught by the nightly `main` run (§3.1) has no PR to comment
on, so it needs the same treatment `test/flakeledger` already gives
scheduled flake discoveries: a dedicated `evalsledger`-style publisher
(new, under #2667's runner-integration scope, reusing the existing
`providers.WorkItem*` abstraction `flakeledger` is built on rather than a
new notification library — there is no Slack/webhook sender anywhere in
this codebase to reuse, and this repo's pattern for "someone needs to look
at this" is a labeled, fingerprinted GitHub issue, not a chat message) that:

- computes a fingerprint from `suite_name` + `scenario.id` + a normalized
  failure signature (mirroring `test/flakeledger`'s fingerprint, which
  already normalizes volatile durations/IDs out of a failure signature);
- files one issue per novel fingerprint, labeled `evals:regression`
  (safety-critical failures additionally get `evals:safety-critical`);
- appends a comment with the new occurrence, run link, and score to an
  existing open issue for a repeat fingerprint, rather than filing a
  duplicate — same dedup rule flake-watch uses;
- never lets a regression issue drift into the automated implementation
  queue by accident: apply `evals:regression` and strip any `goobers:*` /
  `goobers/status:*` labels on file/refresh, exactly as `flakeledger` does
  for `ci:flake`.

### 5.3 Owner escalation

Extend `.github/CODEOWNERS` with an `/evals/` entry once that directory
exists (as part of #2667 landing the runner), naming the suite owner(s):

```
# EvalSuite definitions and baselines.
/evals/                 @<evals-owner>
```

That one line gets two things for free through mechanisms this repo already
enforces: CODEOWNERS review is required before an `evals/` change merges
(including a baseline bump, §4.2), and it gives `evalsledger` a concrete
owner to `@`-assign filed regression issues to, the same way a human reading
CODEOWNERS would know who to page today. A suite with multiple stakeholders
lists a team, not an individual — the existing CODEOWNERS header already
flags "replace @masra91 with an org team" as the direction to grow in, and
evals ownership should follow that from day one rather than pointing at one
person.

Safety-critical failures (§3.2) skip any sampling or batching: the ledger
issue is filed/updated immediately (not batched into the nightly run's
summary) and the owner is `@`-mentioned in the issue body, not just
assigned, since assignment alone is easy to miss.

### 5.4 What's explicitly out of scope here

The annotation queue for gray-zone/low-confidence/sampled-pass human review
(`EVALS_JUDGE_DESIGN.md`'s HITL workflow) is a UI/tooling concern for #2669,
not a CI-gating concern. This design's obligation to that workflow is narrow
and concrete: the gate must emit its full per-scenario judge output
(score, labels, reason, confidence) as a machine-readable artifact (the
run-report JSON, §4.3) so a future annotation tool has something to read —
not build the annotation tool itself.

## 6. Example workflow

The file below is real (`.github/workflows/evals-gate.yml`), not just a
doc snippet — it lints and runs today. It follows `provider-fixture-drift.yml`'s
shape for a designed-but-not-yet-provisioned gate: `workflow_dispatch`-only
(no `schedule`, no `pull_request` trigger yet — enabling those is a
follow-up once #2667 ships a runner), least-privilege `permissions`, a
`concurrency` group, and a provisioning guard that fails immediately with a
clear pointer rather than attempting to invoke a runner that doesn't exist:

```yaml
name: EvalSuite gate

# Dormant until #2667 (runner integration) lands. workflow_dispatch only —
# no schedule, no pull_request trigger — until there is a real runner and a
# committed evals/testdata/baselines/ fixture for it to diff against. See
# docs/design/evals-ci-gating.md.

on:
  workflow_dispatch:
  # Enable once #2667 lands a runner and baselines are committed (see
  # docs/design/evals-ci-gating.md §4):
  # pull_request:
  #   paths: ["evals/**"]
  # schedule:
  #   - cron: "17 6 * * *"

permissions:
  contents: read

concurrency:
  group: evals-gate-${{ github.ref }}
  cancel-in-progress: true

jobs:
  gate:
    name: EvalSuite baseline gate
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - name: Checkout
        uses: actions/checkout@v7

      - name: Verify runner is provisioned
        run: |
          if [ ! -d evals/cmd ] || [ ! -d evals/testdata/baselines ]; then
            echo "::error title=EvalSuite runner not yet implemented::evals/cmd/ (runner) and evals/testdata/baselines/ (§4) don't exist yet. This is a distinct gate from evals-tests.yml, which already runs the evals/ schema/unit tests (#2670-#2672); this one is blocked on #2667 (runner integration) landing. See docs/design/evals-ci-gating.md."
            exit 1
          fi

      # Once #2667 lands, the remaining steps become real:
      #
      # - name: Set up Go
      #   uses: actions/setup-go@v7
      #   with:
      #     go-version-file: go.mod
      #     cache: true
      #
      # - name: Run EvalSuite against committed baselines
      #   run: >-
      #     go run ./evals/cmd/evalsrun gate
      #     -suites evals/suites
      #     -baselines evals/testdata/baselines
      #     -out evals-run-report.json
      #
      # - name: Upload run report
      #   if: ${{ !cancelled() }}
      #   uses: actions/upload-artifact@v7
      #   with:
      #     name: evals-run-report-${{ github.run_id }}
      #     path: evals-run-report.json
      #     retention-days: 30

      # Guards partial activation: if a future edit uncomments the trigger
      # and this guard starts passing (evals/cmd + baselines exist) without
      # also uncommenting the run step above, this is the only thing left to
      # catch a green "gate" that never actually ran anything.
      - name: Verify the gate actually ran
        run: |
          if [ ! -f evals-run-report.json ]; then
            echo "::error title=EvalSuite gate activation incomplete::evals-run-report.json was not produced. Uncomment the commented 'Run EvalSuite against committed baselines' step above (not just the provisioning guard) before enabling this workflow's triggers."
            exit 1
          fi
```

## 7. Open questions for #2667 / #2669

- Exact shape of the runner CLI (`go run ./evals/cmd/evalsrun gate`, above,
  is illustrative — #2667 owns the real interface) and how it maps
  score/threshold/gray-zone into a process exit code the gate step can act
  on directly.
- Whether suite-to-workflow tagging (§3.1, for the PR path filter) lives in
  suite `metadata` or is inferred from which reference-workflow stages a
  scenario's `stages[]` name.
- The `evalsledger` publisher (§5.2) is scoped as reusing
  `test/flakeledger`'s provider abstraction, not a new package from
  scratch — #2667 should confirm that's still the right split once the
  runner's package layout is decided.
- Annotation queue / HITL tooling (§5.4) — entirely #2669's scope.
