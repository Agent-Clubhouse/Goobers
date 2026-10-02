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
changes a job's result: the upload steps run with `if: always()` and
`continue-on-error: true`.

## What publishes what

| Job (`ci.yml`) | Artifact | Files |
|---|---|---|
| `unit race shard N/5 (linux)` (`unit`) | `test-results-race-linux-<job-index>` | `unit-race.part<k>.junit.xml`, one per `go test` process in the shard |
| `unit coverage gate (linux)` (`unit-linux-coverage`) | `test-results-Linux` | `unit-Linux.junit.xml` |

`<job-index>` is the matrix index (0-4), so shard `1/5` uploads
`test-results-race-linux-0`. Each JUnit report has one `<testsuite>` per
package and one `<testcase>` per test, subtests included. A package that failed
without a failing test (a compile error, a panic in `TestMain`, a timeout
outside any test) gets a synthetic `[package]` case carrying the build or
package output.

Other jobs (`preflight`, `checks`, `lint`, `shipped`, `integration`,
`windows-smoke`, `sandbox`, and the rest) do not publish JUnit yet; their logs
remain the record.

## Annotations

On a failing unit job, each failure becomes an `::error` annotation titled
`<Test> failed (<package>)`, `build failed: <package>`, or
`package failed: <package>`. Where the failure names a source line (the
`file_test.go:42:` prefix `t.Error`/`t.Fatal` print, or a compiler diagnostic),
the annotation points at that repository file and line. The message holds the
first 50 lines the test printed; the JUnit report has the rest.

The hourly `Flake watch` workflow (`test/flakewatch`) reads these annotations
before it falls back to job logs, so a unit failure is fingerprinted from its
own output even when parallel tests interleave in the log, and a pull-request
failure whose annotated file the pull request changed is treated as a
regression rather than a flake candidate.

A failed parent test whose failure comes from a failed subtest is not
annotated separately: only the most specific failure is. GitHub shows at most
ten error annotations per step, so the capture stops at ten and adds one
`More failing tests` notice giving the remaining count. The JUnit artifact is
always complete.

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
   failing tests and lines. For other jobs, open the failed step's log.
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
