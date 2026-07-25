# Decision: the `test-suite-quality` workflow owns test-suite quality

> **Status:** Accepted design decision (2026-07-25); runtime implementation is
> tracked by #1489 and #1490.
>
> **Related:** #507 (this decision), #506 / PR #1037 (per-check CI evidence),
> [`ARCHITECTURE.md` §5 and §8](../ARCHITECTURE.md), and
> [`requirements/telemetry.md`](../requirements/telemetry.md).

## Decision

A dedicated canonical producer workflow named **`test-suite-quality`** owns
longitudinal analysis of a target repository's test suite. Its responsibility
is deliberately narrow:

- detect tests that are flaky across comparable observations;
- track coverage trends and identify sustained regressions; and
- identify tests that are persistently slow, rather than merely slow once.

It converts check- and test-level telemetry into versioned, evidence-backed
quality findings. It does not edit tests, quarantine tests, change gates, or
file backlog items itself.

## Inputs

The workflow reads immutable telemetry snapshots through the standard
telemetry-query connector. Every observation must retain the target revision,
check identity, execution environment, and a journal or artifact evidence
pointer so unlike runs are not silently compared.

| Input | Required content | Use |
|---|---|---|
| CI check observations | Stable check name, terminal outcome, duration when available, target revision, environment, and bounded failure detail | Establish the check-level series and link findings to durable evidence. The existing `ci-checks.json` artifact from #506 / PR #1037 is one source. |
| Test observations | Stable test and suite/package identity, outcome, duration, attempt or rerun identity, check identity, revision, and environment | Detect outcome instability and persistent duration outliers. A failed check without test-level records remains check-level evidence; the workflow must not invent a test diagnosis by scraping an unstructured log. |
| Coverage snapshots | Scope identity (for example package/module), covered and total units, revision, check identity, and environment | Compare like-for-like coverage over time and distinguish a trend from one isolated measurement. |
| Quality policy | Analysis window, minimum sample counts, comparison dimensions, flake threshold, coverage regression threshold, slow-test budget, and proposal limits | Make classification deterministic and keep noise bounded. |

All inputs remain subject to the journal's redaction-before-digest boundary.
The workflow owns analysis of these records, not their collection or retention.

## Outputs

Each run emits a versioned **`test-suite-quality-findings`** artifact, including
an empty artifact when no threshold is crossed. Each finding contains:

- a kind: `flake-candidate`, `coverage-trend`, or `persistent-slow-test`;
- the affected test, suite, package, or coverage scope;
- the observation window, comparison dimensions, sample count, threshold, and
  measured values;
- digested evidence pointers back to the source checks, tests, or coverage
  snapshots; and
- a bounded recommendation. Flake recommendations may propose quarantine with
  an owner and expiry, but never enact it.

The workflow publishes the artifact pointer as a signal for
`work-nomination`. That workflow decides whether a finding is actionable,
deduplicates it against existing work, and, when warranted, turns it into an
evidence-backed backlog item. Curation and implementation continue to own
readiness and code changes.

```text
CI/test producers -> telemetry + journal evidence
                  -> test-suite-quality
                  -> test-suite-quality-findings
                  -> work-nomination -> backlog -> curation/implementation
```

## Ownership boundaries

| Workflow | Owns | Explicitly does not own |
|---|---|---|
| `test-suite-quality` | Test-specific time-series analysis, classification, and bounded quality recommendations | Backlog admission, product-code changes, quarantine enforcement, quality-gate changes, or instance configuration |
| `work-nomination` | General target-product signal mining, actionability judgment, duplicate suppression, and creation of evidence-backed backlog work | Reimplementing test-specific flake, coverage-trend, or slow-test analysis once a quality finding exists |
| `tutor` | Improving how a gaggle works by proposing changes to its instance configuration, workflows, gates, skills, and goober instructions | Product test health, product-code changes, test quarantine, or target-repository coverage and performance campaigns |

The same boundary applies when Goobers is its own target: a flaky Go test is a
product test-suite finding and flows through nomination and implementation. A
misconfigured workflow validation stage is a process/configuration concern for
the Tutor. Sharing telemetry does not transfer ownership between those domains.

## Implementation boundary

This decision adds no runtime workflow or telemetry schema. #1489 implements
flake detection and quarantine proposals; #1490 implements coverage-trend and
persistent-slow-test tracking. Until those changes land, the existing
`work-nomination` and `tutor` workflows continue unchanged.
