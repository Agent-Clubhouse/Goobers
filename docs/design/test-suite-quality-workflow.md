# Decision: dedicated workflows own repository and test-suite quality

> **Status:** implemented — repository review by #1568 (2026-07-30);
> recurring-flake analysis by #1489.
>
> **Canonical workflows:** [`quality-sprint`](../../reference-workflows/gaggles/goobers/workflows/quality-sprint.yaml)
> and [`test-suite-quality`](../../reference-workflows/gaggles/goobers/workflows/test-suite-quality.yaml)
>
> **Related:** #507 (original test-suite-quality decision), #506 / PR #1037
> (per-check CI evidence), [`ARCHITECTURE.md` §5 and §8](../ARCHITECTURE.md),
> [`requirements/telemetry.md`](../requirements/telemetry.md), and
> [`static-fan-out-fan-in.md`](static-fan-out-fan-in.md).

## Decision

The shipped `quality-sprint` producer runs a scheduled, evidence-based review
of a target repository across six quality lenses. The separate canonical
`test-suite-quality` workflow owns longitudinal test telemetry. Its first
shipped slice detects recurring flaky tests and produces bounded fix or
quarantine recommendations; coverage trends and persistent slow-test tracking
remain assigned to #1490.

The workflow reports and nominates findings. It does not edit product code,
tests, gates, quarantine configuration, or instance configuration.

## Shipped workflow

Each run has this fixed shape:

```text
churn-analysis
      |
      +-> security -----------+
      +-> performance --------+
      +-> maintainability ----+
      +-> test coverage ------+-> collate -> nominate
      +-> dependencies -------+
      +-> latent bugs --------+
```

1. `churn-analysis` creates a deterministic digest of files changed during the
   configured lookback period.
2. `focus-areas` statically fans out six read-only `quality-researcher`
   branches: security, performance, maintainability, test coverage,
   dependencies, and latent bugs. All six belong to one parallel, although
   `maxConcurrentBranches: 4` limits execution to four branches at a time.
3. The parallel uses `continue_on_error`, so one failed, timed-out, or
   no-output lens does not discard successful reports from the others.
4. `collate` fans the available reports in to one read-only `quality-lead`
   stage, which deduplicates overlapping findings within the current run.
5. `nominate` evaluates the collated findings and may create backlog issues.
   Those issues remain unapproved; a maintainer still supplies the SEC-047
   trust decision before curation.

The checked-in reference runs weekly on Monday and permits one workflow run at
a time and one run per hour. Operators may tune the live cadence and budget
without changing the canonical fan-out/fan-in contract.

## Inputs and outputs

The repository and the churn digest are the shipped inputs. Each lens writes a
freeform `findings.md` artifact and emits a scalar `findingsRef`; an honest
empty report is valid. The join supplies the branch completeness record and
available reports to `collate`, which writes `collated-findings.md` and emits
`collatedFindingsRef`. The terminal `nominate` stage consumes that result and
files any warranted backlog items.

The reports must identify concrete repository locations and evidence, but they
are deliberately judged and deduplicated by the agents rather than by a typed
finding schema. Artifact pointers and stage outputs continue to follow the
standard envelope, journal, digest, and redaction boundaries in
[`ARCHITECTURE.md`](../ARCHITECTURE.md).

## Ownership boundaries

| Workflow | Owns | Explicitly does not own |
|---|---|---|
| `quality-sprint` | Scheduled repository review through six fixed lenses, within-run deduplication, and nomination of warranted backlog work | Longitudinal test telemetry, product changes, test quarantine, quality-gate changes, maintainer approval, or instance configuration |
| `test-suite-quality` | Recurring test-failure analysis and bounded fix or quarantine recommendations | Enacting quarantine, automatic retries, product-code changes, coverage/slow-test tracking before #1490, or instance configuration |
| `work-nomination` | General target-product signal mining and nomination from its own gathered signals | Running or duplicating `quality-sprint`'s six-lens review |
| `tutor` | Proposals to improve gaggle configuration, workflows, gates, skills, and goober instructions | Product-code or product test-suite quality campaigns |

The same boundary applies when Goobers is its own target: a flaky Go test is a
product finding that a quality lens may nominate, while a misconfigured
workflow validation stage is a process/configuration concern for the Tutor.

## Test-suite telemetry implementation

`test-suite-quality` queries a seven-day window for CI checks that failed in at
least two distinct runs. Its read-only analyst resolves the bounded journal
pointers and durable `ci-checks.json` evidence, requires a stable test and
suite/package identity, and suppresses single failures, unrelated assertions,
and likely regressions. Confirmed findings carry distinct run IDs and either a
scoped fix or an issue-backed quarantine proposal with an owner and expiry.
The nominator then performs backlog deduplication and opens only warranted
issues; neither stage edits tests or weakens CI.

This is intentionally the #1489 flake slice. Typed coverage comparisons and
persistent slow-test budgets remain unimplemented pending #1490.
