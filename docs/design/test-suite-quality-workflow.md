# Decision: `quality-sprint` owns scheduled repository quality review

> **Status:** implemented — by #1568 (2026-07-30).
>
> **Canonical workflow:** [`quality-sprint`](../../reference-workflows/gaggles/goobers/workflows/quality-sprint.yaml)
>
> **Related:** #507 (original test-suite-quality decision), #506 / PR #1037
> (per-check CI evidence), [`ARCHITECTURE.md` §5 and §8](../ARCHITECTURE.md),
> [`requirements/telemetry.md`](../requirements/telemetry.md), and
> [`static-fan-out-fan-in.md`](static-fan-out-fan-in.md).

## Decision

The shipped canonical producer workflow is named **`quality-sprint`**. It runs
a scheduled, evidence-based review of a target repository across six quality
lenses. Test coverage is one lens; the workflow is not a test-suite-specific
telemetry analyzer and there is no shipped workflow named
`test-suite-quality`.

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
| `quality-sprint` | Scheduled repository review through six fixed lenses, within-run deduplication, and nomination of warranted backlog work | Product changes, test quarantine, quality-gate changes, maintainer approval, or instance configuration |
| `work-nomination` | General target-product signal mining and nomination from its own gathered signals | Running or duplicating `quality-sprint`'s six-lens review |
| `tutor` | Proposals to improve gaggle configuration, workflows, gates, skills, and goober instructions | Product-code or product test-suite quality campaigns |

The same boundary applies when Goobers is its own target: a flaky Go test is a
product finding that a quality lens may nominate, while a misconfigured
workflow validation stage is a process/configuration concern for the Tutor.

## Aspirational test-suite telemetry design

The original decision proposed a narrower workflow named
`test-suite-quality`. That proposal would consume immutable check-, test-, and
coverage-level telemetry; compare like-for-like observations over time; apply
configured sample counts and thresholds; and emit versioned, typed findings
such as `flake-candidate`, `coverage-trend`, and `persistent-slow-test`.

None of that is current `quality-sprint` behavior. In particular, the shipped
workflow has no telemetry-query input, longitudinal or cross-run trend
tracking, deterministic quality thresholds, severity taxonomy, typed
`test-suite-quality-findings` artifact, or automatic quarantine action. It
also nominates through its own terminal stage rather than publishing a
findings signal for a separate `work-nomination` run.

Those capabilities remain aspirational. If adopted, they require a separate
design and implementation that defines how a specialized telemetry analyzer
composes with, extends, or replaces part of `quality-sprint`; the old
`test-suite-quality` name and data flow must not be read as shipped aliases or
current runtime behavior.
