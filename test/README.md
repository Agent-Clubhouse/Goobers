# `/test` — CI & Acceptance Harness (M5)

Owner: Goobers-QA-1 (with QA-2). This directory holds the cross-cutting test harness and
the **QA merge gate**. Per-package unit tests live next to the code they test; this
directory holds **acceptance / contract suites** that span packages and the fixtures
they run against.

## The merge gate

A PR is mergeable only when CI is green, where green means:

| Check | Tool | Gate |
|---|---|---|
| Lint | golangci-lint (`make lint`) | zero issues |
| Vet | `go vet` (`make test` or its own step) | clean |
| Unit tests | `go test ./...` (`make test`) | all pass |
| Coverage | `make cover-check` | total ≥ **70%** (ratcheting target) |
| Acceptance | suites in this dir | all pass |

**One CI workflow.** Dev-3 owns `.github/workflows/ci.yml` and the build/vet/lint/unit
jobs; QA extends it with the coverage-gate + acceptance jobs. We do not add a second
workflow file. Gate logic lives in `Makefile` targets the workflow invokes, so the YAML
stays thin and the gate is runnable locally (`make ci`).

### Coverage gate
`make test`/`make cover` writes `coverage.out`; **`test/coverage_gate.sh [threshold]`**
(default 70, or `$COVERAGE_THRESHOLD`) fails with exit 1 if coverage is below the
threshold, printing the excluded file set + per-function coverage + the total.

It measures coverage over the module's **testable logic**: non-logic code is excluded
from the denominator so the number stays a precise signal (and won't be diluted, or
mask a real per-package regression, as the codebase grows). Excluded by default
(override via `$COVERAGE_EXCLUDE`): `cmd/*` main entrypoints, generated code
(`zz_generated*`, `*.deepcopy.go`), and embed-only packages (`api/schemas`). The set of
excluded files is printed every run — never a silent cap. Tip: `go clean -testcache`
before trusting a number on a freshly-mutated tree (stale cached coverage reads low).

Wiring: once the skeleton is merged, this backs a `make cover-check` target on the shared
workflow (coordinated with Dev-3, not a second workflow). In the interim gate QA runs it
directly on the PR head. Threshold starts at 70% and ratchets up as the codebase matures;
it is never lowered to pass a specific PR (see QA checklist §5).

## Acceptance suites

### M1 — config validation + envelope schemas (`config/`)
- Runs Dev-1's `validate` CLI against `fixtures/config/good/*` (must pass) and
  `fixtures/config/bad/*` (must be rejected, with a non-zero exit).
- Asserts the invocation-envelope / result / verdict **JSON Schemas** accept valid
  fixtures and reject malformed ones (missing required fields, wrong types).
- *Blocked on:* Dev-1 landing the `validate` CLI entrypoint + published JSON Schemas.

### M2 — provider contract (`providers/`)
- One shared contract suite run twice — once against the GitHub impl, once against ADO —
  over **mocked HTTP responses**, asserting identical observable behavior (list/claim/
  update work items, error mapping, pagination, rate-limit handling).
- Table of provider-agnostic expectations; each provider plugged into the same asserts.
- *Blocked on:* Dev-2 landing the provider interface + a way to inject a mock transport.

## Layout
```
test/
  README.md            # this file
  QA_CHECKLIST.md      # the review bar applied to M1/M2 PRs
  fixtures/
    config/{good,bad}/  # config manifests for the validate CLI  (pending Dev-1 schema)
    envelopes/{valid,invalid}/  # invocation/result/verdict JSON  (pending Dev-1 schema)
  config/              # M1 acceptance suite  (Go; pending skeleton + validate CLI)
  providers/           # M2 contract suite    (Go; pending provider interface)
```

## Status
- [x] QA checklist drafted (`QA_CHECKLIST.md`)
- [x] Harness layout + gate design (this file)
- [ ] CI coverage gate — coordinating with Dev-3; lands as a job on the shared workflow
- [ ] M1 fixtures + acceptance suite — pending Dev-1's validate CLI + schemas
- [ ] M2 contract suite — pending Dev-2's provider interface
