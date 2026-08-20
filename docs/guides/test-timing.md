# Test timing artifacts and budgets

The macOS behavioral unit job captures the unit test tier's wall-clock,
package, and test durations and uploads `test-timings-macOS`, containing
`unit-macOS.json`. That job runs for merge validation and again on the landed
main SHA so successful main pushes continue to provide the canonical artifact
for trend comparisons without rerunning the full merge-gate matrix. The
post-merge lane also retains the platform-independent `checks` job as a safety
smoke for the exact landed tree; all other merge-gate jobs run only for pull
requests and merge groups. The report step compares the test tier with
`.github/test-timing-budgets.json`, appends actual-versus-budget data to the
workflow summary, and compares with the latest successful `main` artifact when
one is available. Capture runs inside `test/hermetic`, preserving the unit
tier's isolated tool `PATH` and offline Go environment.

For local feedback, the Go validation orchestrator prints elapsed time after
every check. `make verify-full` uses that orchestrator to run `ci` and each
additional Make gate serially, so integration, e2e, envtest, coverage, sandbox,
platform, and shipped-workflow durations are all visible without producing CI
artifacts.

The artifact is JSON with `schemaVersion: 1`:

```json
{
  "schemaVersion": 1,
  "job": "unit",
  "platform": "linux",
  "architecture": "amd64",
  "elapsedSeconds": 123.4,
  "packages": [
    {
      "package": "github.com/goobers/goobers/internal/example",
      "status": "pass",
      "elapsedSeconds": 1.2
    }
  ],
  "tests": [
    {
      "package": "github.com/goobers/goobers/internal/example",
      "test": "TestExample",
      "status": "pass",
      "elapsedSeconds": 0.1
    }
  ]
}
```

The Linux race shards use reviewed package measurements checked in at
`.github/unit-shard-weights.json`. The hermetic runner assigns the longest
packages first to the currently lightest shard; packages below the table's
measurement threshold and packages added later use `defaultSeconds`. Refresh
the table from successful race-shard logs or timing artifacts when the package
mix or measured shard balance changes, and update its source metadata.

Timing budgets are intentionally soft. An over-budget run emits a GitHub
workflow warning and an `OVER BUDGET` summary row, but the comparison command
still succeeds. Test failures and malformed timing data remain ordinary CI
failures.

## Raising a budget

Treat `.github/test-timing-budgets.json` like any other reviewed source file.
Before raising a budget:

1. Compare several successful `main` artifacts and identify the packages or
   tests responsible for the sustained increase.
2. Decide explicitly whether to recover the regression or accept it. Do not
   raise a budget for a single noisy runner.
3. If the increase is accepted, update `baselineSeconds`, `budgetSeconds`, and
   the `baseline` measurement description in the same pull request. Record the
   new observed main duration and retain deliberate contention headroom.

The initial unit baseline records the roughly two-minute main-branch test tier
observed before this budget was introduced; its five-minute budget leaves
headroom for hosted-runner contention. The stress tier tracked by #661 is not
present yet. When it lands, route its `go test` invocation through
`test/testtiming capture`, add a `stress` entry to the budget file, and publish
the same schema rather than introducing a second format.
