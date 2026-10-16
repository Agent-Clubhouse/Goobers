# CI test results: JUnit artifacts and failure annotations

When a CI run goes red, start here. The Go unit tier publishes which tests
failed in two machine-readable forms, so you rarely need to read a job's full
log:

- **Failure annotations** on the run summary page, one per failing test (or
  per package that failed without a failing test, such as a build failure).
- **JUnit XML artifacts** that list every test the job ran, with its result,
  duration, and the output of each failure.

Both are produced by `test/testtiming capture`, which already parses the
`go test -json` stream for [test timing](test-timing.md). Producing them never
changes a job's result: a report that cannot be written is only a warning, and
the upload steps run with `if: always()` and `continue-on-error: true`.

## What publishes what

| Job (`ci.yml`) | Artifact | Files |
|---|---|---|
| `unit race shard N/5 (linux)` (`unit`) | `test-results-race-linux-<job-index>` | `unit-race.part<k>.junit.xml`, one per `go test` process in the shard |
| `unit coverage gate (linux)` (`unit-linux-coverage`) | `test-results-Linux` | `unit-Linux.junit.xml` |

`<job-index>` is the matrix index (0-4), so shard `1/5` uploads
`test-results-race-linux-0`. The macOS nightly (`macos-nightly.yml`) runs the
same capture, so it also writes `unit-macOS.junit.xml` and annotates failures,
but it does not upload the report. Each JUnit report has one `<testsuite>` per
package and one `<testcase>` per test, subtests included. A test still running
when its package failed (a test timeout, or a crash) is listed as failed with
the package's output, which includes the timeout panic and stacks. A package
that failed without any failing test (a compile error, a panic in `TestMain`)
gets a synthetic `[package]` case carrying the build or package output.

Other jobs (`preflight`, `checks`, `lint`, `shipped`, `integration`,
`windows-smoke`, `sandbox`, and the rest) do not publish JUnit yet. Every job
that runs the CI driver does record [CI failure diagnostics](#ci-failure-diagnostics),
however, so its failing check, tests, and output are available without the log.

## CI failure diagnostics

Every `ci.yml` job that runs `go run ./test/ci` (except `scope` and the
cache-warming job), along with the macOS nightly `runtime` job, ends with two
steps:

- **Record CI diagnostics** (`go run ./test/ci diagnose`, `if: always()`)
  writes a job summary: the failure class, an environment record (runner
  OS/arch/name, image and version, Go version, CPUs, run/attempt/ref/SHA), one
  row per driver invocation (result, start, duration, failed or unfinished
  check, slowest checks), and the failing tests and packages with their output.
  If no test is named, it shows the tail of the output instead.
- **Upload CI diagnostics** (only on failure or cancellation) uploads the
  `ci-diagnostics-<job>-<matrix-index>` artifact. It contains `summary.md`,
  `diagnostics.json`, and, for each driver invocation, its full `output.log`
  (capped at 64 MiB) and `state.json`.

The driver records only on GitHub Actions (under `$RUNNER_TEMP`), or when
`GOOBERS_CI_DIAGNOSTICS_DIR` is set, so local runs are unchanged. Both steps
use `continue-on-error`. They never change a job's verdict, and no check is
skipped or retried.

| Class | Category | Meaning |
|---|---|---|
| `test-failure` | test | `--- FAIL:` lines, or a package `FAIL` with no named test |
| `test-timeout` | test | a `panic: test timed out` in the failed check |
| `build-failure` | check | `[build failed]` / `[setup failed]` in the failed check |
| `check-failure` | check | a non-test check (lint, policy gate, …) failed |
| `step-timeout` | infrastructure | the driver was stopped mid-check while the job failed (step or job timeout, runner shutdown) |
| `cancelled` | infrastructure | the job was cancelled (superseded run, manual cancel) |
| `outside-driver-failure` | outside-driver | the job failed and the driver never ran (setup, checkout, or toolchain step) |
| `post-driver-failure` | outside-driver | every driver run passed, but a later step failed |

When a runner is lost, no later step runs at all. A failed job with no
`CI diagnostics` summary and no artifact therefore means runner loss.

The diagnose step also writes one `CI failure class: <class>` notice
annotation. That notice, and the job log, carry only the class: test names and
`FAIL` lines go to the summary and artifact. Flake watch keeps fingerprinting
the driver's own output exactly as before.

## Annotations

On a failing unit job, each failure becomes an `::error` annotation titled
`go test: <Test> failed in <package>`, `go test: build failed: <package>`,
`go test: package failed with unfinished tests: <package>` (a timeout or
crash), or `go test: package failed: <package>`. One compile error is annotated
once, not once per package that could not build because of it.

Where the failure names a source line (the `file_test.go:42:` prefix
`t.Error`/`t.Fatal` print, or a compiler diagnostic), the annotation points at
that repository file and line, provided the file exists at that path. The
message holds the first 50 lines the test printed; the JUnit report has the
rest.

The hourly `Flake watch` workflow (`test/flakewatch`) skips annotations titled
`go test: ` and keeps fingerprinting these failures from the job log, so
existing flake-ledger entries keep matching. Moving the ledger onto these
structured results would re-key fingerprints and is a separate decision.

A failed parent test whose failure comes from a failed subtest is not
annotated separately: only the most specific failure is. GitHub shows at most
ten error annotations per step. Each capture process stops at ten and adds one
`More failing tests` notice giving the remaining count; a race shard runs
several capture processes in one step, so there GitHub may drop annotations
past the first ten without a notice. The JUnit artifacts are always complete.

The capture prints annotations only when `GITHUB_ACTIONS=true`, so local runs
are unaffected. Pass `-annotations=false` to suppress them in CI, or
`-annotations` to print them locally.

## Fetching results with `gh`

```sh
# Find the run (latest CI runs on main, or the runs for a pull request's branch).
gh run list -R Agent-Clubhouse/Goobers --workflow ci.yml --branch main --limit 5

# Summary of a run, including its failed jobs and annotations.
gh run view <run-id> -R Agent-Clubhouse/Goobers

# Download every JUnit artifact of a run into ./results/<artifact-name>/.
gh run download <run-id> -R Agent-Clubhouse/Goobers -p 'test-results-*' -D results

# Or a single artifact.
gh run download <run-id> -R Agent-Clubhouse/Goobers -n test-results-Linux -D results

# List the failing test cases in the downloaded reports.
grep -B1 '<failure' -r results --include '*.junit.xml' | grep -o 'classname="[^"]*" name="[^"]*"'

# Failure diagnostics (failing check/tests, output, runner details) of failed jobs.
# The artifact is available once the failed job's upload step finishes.
gh run download <run-id> -R Agent-Clubhouse/Goobers -p 'ci-diagnostics-*' -D diagnostics

# Annotations of one job, as JSON (the job id is in `gh run view --json jobs`).
gh api repos/Agent-Clubhouse/Goobers/check-runs/<job-id>/annotations
```

Artifacts follow the repository's default retention, so very old runs may
have none left; rerun the job if you need fresh results.

## Why is main red?

1. Open the failing run (`gh run list ... --branch main`; the
   `escalate failed main validation` job also files or updates a
   `CI: main branch validation failed` issue linking it).
2. Read the annotations on the summary page. For a unit job, they name the
   failing tests and lines. Every driver job's summary page shows its
   `CI diagnostics` section, which gives the failure class (test vs.
   infrastructure) and the failing tests with their output. The
   `ci-diagnostics-*` artifact holds the full output log.
3. For the full output of a unit failure, download its `test-results-*`
   artifact and read the `<failure>` element of the failing case.
4. Decide flake versus break. Rerun the test locally with the CI flags
   (`go test -race -timeout 30m -count=1 -run '^TestName$' ./pkg`), and
   follow [flake management](flake-management.md): a failure that does not
   reproduce is a tracked flake, not a reason to retry silently.
5. A break reproduced on `origin/main` is fixed forward in a pull request; a
   flake gets its flake-ledger entry and issue.

The design behind this surface is section 3 (E5) of
[validation and CI enrichment](../design/validation-and-ci-enrichment.md).
Having merge-review and other agent consumers read these artifacts instead of
logs is tracked separately and is not wired up yet.
