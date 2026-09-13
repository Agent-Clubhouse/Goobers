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
three-second measurement threshold and packages added later use
`defaultSeconds`.

Refresh the table at least every **30 days**, and sooner when the package mix or
measured shard balance changes. Use the latest successful `main`
`test-timings-macOS` artifact and its GitHub API metadata:

```sh
REPOSITORY=Agent-Clubhouse/Goobers
RUN_ID=$(gh run list --repo "$REPOSITORY" --workflow CI --branch main --status success --limit 1 --json databaseId --jq '.[0].databaseId')
ARTIFACT_ID=$(gh api "repos/$REPOSITORY/actions/runs/$RUN_ID/artifacts?per_page=100" --paginate --jq '.artifacts[] | select(.name == "test-timings-macOS") | .id')
JOB_ID=$(gh api "repos/$REPOSITORY/actions/runs/$RUN_ID/jobs?per_page=100" --paginate --jq '.jobs[] | select(.name == "unit behavioral suite (macos)" and .conclusion == "success") | .id')
TIMING_DIR=$(mktemp -d)
gh run download --repo "$REPOSITORY" "$RUN_ID" --name test-timings-macOS --dir "$TIMING_DIR"
gh api "repos/$REPOSITORY/actions/artifacts/$ARTIFACT_ID" > "$TIMING_DIR/artifact.json"
gh api "repos/$REPOSITORY/actions/jobs/$JOB_ID" > "$TIMING_DIR/job.json"
go run ./test/testtiming weights \
  -timing "$TIMING_DIR/unit-macOS.json" \
  -artifact-metadata "$TIMING_DIR/artifact.json" \
  -job-metadata "$TIMING_DIR/job.json" \
  -out .github/unit-shard-weights.json \
  -minimum-seconds 3
```

The generator accepts only a completed successful canonical job on `main`,
cross-checks the run and full commit SHA in both API records, and requires the
artifact's `created_at` to fall within that job's execution window. It records
that authoritative artifact timestamp as `source.generatedAt`; it never uses
the command time or the eventual patch time. Run, job, artifact, commit,
platform, architecture, and threshold remain in the checked-in source record so
the measurement is independently traceable.

The macOS job is the canonical source because it captures the complete unit
suite in one artifact on every successful main push. Its ordinary (non-race)
package durations are relative LPT weights for the Linux `-race` shards, not a
prediction of their absolute runtime: race instrumentation and platform costs
can scale packages differently. Keep the three-second floor to avoid encoding
noise from tiny packages, and review actual Linux shard elapsed times after a
refresh. `TestCheckedInShardWeightsAreFresh` enforces the 30-day artifact-age
ceiling, while loader validation refuses missing or malformed provenance.

Timing budgets are intentionally soft, and the comparison command always
succeeds regardless of what the timing data shows -- test failures and
malformed timing data remain the only ordinary CI failures this step can
produce. `budgetSeconds` is purely informational: shared runners (macOS in
particular) contend unpredictably enough that a fixed-second ceiling is either
stale noise or a number someone has to keep chasing upward (#3323 -- every
green macOS run had already exceeded the 300-second budget in place at the
time, with day-to-day swings over 20% and no code change at all). It still
appears in the ledger row/summary table because the trend is genuinely
useful, but it no longer drives any signal.

The advisory GitHub workflow warning (an `OVER BUDGET`-style annotation) is
driven entirely by `regressionTolerance`: the fraction of growth over the
previous successful run's `elapsedSeconds` that counts as a genuine
regression rather than contention noise. Growth at or below that fraction
never fires; growth beyond it does, independent of whether the (informational)
budget was also exceeded. With no previous measurement to compare against
(first run, cross-fork PR missing the artifact), nothing distinguishes noise
from regression, so no advisory fires.

## Adjusting timing budgets

Treat `.github/test-timing-budgets.json` like any other reviewed source file.

- **`regressionTolerance`** is the field that matters for signal quality.
  Before lowering it, compare several successful `main` artifacts and confirm
  day-to-day contention swings stay comfortably below the new value -- a
  tolerance the shared runner's own noise floor can clear defeats the whole
  point (see #3323). Before raising it, make sure a real regression wouldn't
  slip through silently.
- **`budgetSeconds`/`baselineSeconds`** only need updating to keep the ledger
  honest (so the summary table's status column reflects reality), not to
  chase contention. Update them together with the `baseline` measurement
  description and the date, in the same pull request.

The stress tier tracked by #661 is not present yet. When it lands, route its
`go test` invocation through `test/testtiming capture`, add a `stress` entry
to the budget file, and publish the same schema rather than introducing a
second format.
