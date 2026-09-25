# Test timing artifacts and budgets

The `unit coverage gate (linux)` job (`unit-linux-coverage` in `ci.yml`)
captures the unit test tier's wall-clock, package, and test durations and
uploads `test-timings-Linux`, containing `unit-Linux.json`. That job runs for
merge validation and again on the landed main SHA so successful main pushes
continue to provide the canonical artifact for trend comparisons without
rerunning the full merge-gate matrix. The post-merge lane also retains the
platform-independent `checks` job as a safety smoke for the exact landed tree;
all other merge-gate jobs run only for pull requests and merge groups. The
report step compares the test tier with
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

Refresh the table when the package mix or measured shard balance changes
enough to matter; there is no enforced cadence. Stale weights degrade shard
balance, not correctness, so nothing times the table out or fails a build over
its age. Use the latest successful `main` `test-timings-Linux` artifact and
its GitHub API metadata:

```sh
REPOSITORY=Agent-Clubhouse/Goobers
RUN_ID=$(gh run list --repo "$REPOSITORY" --workflow CI --branch main --status success --limit 1 --json databaseId --jq '.[0].databaseId')
ARTIFACT_ID=$(gh api "repos/$REPOSITORY/actions/runs/$RUN_ID/artifacts?per_page=100" --paginate --jq '.artifacts[] | select(.name == "test-timings-Linux") | .id')
JOB_ID=$(gh api "repos/$REPOSITORY/actions/runs/$RUN_ID/jobs?per_page=100" --paginate --jq '.jobs[] | select(.name == "unit coverage gate (linux)" and .conclusion == "success") | .id')
TIMING_DIR=$(mktemp -d)
gh run download --repo "$REPOSITORY" "$RUN_ID" --name test-timings-Linux --dir "$TIMING_DIR"
gh api "repos/$REPOSITORY/actions/artifacts/$ARTIFACT_ID" > "$TIMING_DIR/artifact.json"
gh api "repos/$REPOSITORY/actions/jobs/$JOB_ID" > "$TIMING_DIR/job.json"
go run ./test/testtiming weights \
  -timing "$TIMING_DIR/unit-Linux.json" \
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
the measurement is independently traceable, but `generatedAt` is informational
provenance only -- nothing checks its age.

The Linux coverage job is the canonical source because it captures the
complete unit suite in one artifact on every successful main push (the macOS
job that previously served this role was retired by the post-#5002 macOS
consolidation, which stopped uploading a `test-timings-macOS` artifact
entirely). Its ordinary (non-race) package durations are relative LPT weights
for the Linux `-race` shards, not a prediction of their absolute runtime: race
instrumentation costs can scale packages differently even on the same
platform. Keep the three-second floor to avoid encoding noise from tiny
packages, and review actual Linux shard elapsed times after a refresh. Loader
validation refuses missing or malformed provenance, but never rejects a
weights table for being old.

## Test-level splits for heavy packages

Package-level LPT cannot put one package on more than one runner, so a package
heavier than a fair share of the suite sets the critical path however many
shards there are (`cmd/goobers` alone ran ~20 minutes under `-race`, with
almost no `t.Parallel()`). `.github/unit-shard-splits.json` lists those
packages, each with a piece count and relative per-test seconds:

```json
{
  "schemaVersion": 1,
  "source": {"run": 1, "commit": "<sha>", "generatedAt": "...", "timingJobs": ["unit"], "platform": "linux"},
  "packages": {
    "github.com/goobers/goobers/cmd/goobers": {"pieces": 3, "tests": {"TestExample": 0.12}}
  }
}
```

The hermetic runner schedules a split package as `pieces` items, each weighing
the package's `unit-shard-weights.json` seconds divided by `pieces`, alongside
the whole packages. A shard that receives a piece:

1. enumerates the package's top-level tests, examples, and fuzz targets with
   `go test -list .` using the suite's own flags (so the listed binary is the
   one the run reuses from the build cache);
2. partitions them by LPT over the recorded per-test seconds. A test absent
   from the table (new or renamed) weighs the package's mean measured test, so
   it is still assigned to exactly one piece and new tests spread across
   pieces instead of piling into one. Every test weighs at least 10ms, so the
   long tail of near-zero tests is dealt evenly and each piece's name list
   stays near 1/`pieces` of the package. The arithmetic is integer
   milliseconds, so every runner derives the identical partition;
3. runs its piece as a separate `go test` process, concurrently with the
   shard's whole packages, filtered by an exact anchored `-run` over the
   piece's names or `-skip` over the other pieces' names, whichever is
   shorter. A filter over 96 KiB fails the shard loudly (Linux caps a single
   argument at 128 KiB): raise that package's `pieces`.
   `TestCheckedInSplitFiltersKeepHeadroom` fails at 72 KiB, while that is
   still a routine change.

Subtests always follow their top-level test. `TestMain` runs once per piece,
so per-package guards such as the `cmd/goobers` package-directory guard run in
every shard holding a piece. Coverage profiles are not produced by the
sharded run (the unsharded `unit-linux-coverage` job owns them).
`TestSplitShardsRunEveryTestExactlyOnce` and
`TestSplitPiecesRunEveryFixtureTestExactlyOnce` (`test/hermetic`) prove every
enumerated test runs in exactly one piece.

Only proportions inside a package matter, but they differ under `-race`: the
coverage job's non-race per-test times left the `cmd/goobers` pieces at 247s,
682s, and 614s in their first race run. Each race shard therefore uploads its own
race-mode timings as `test-timings-race-linux-<n>` (job `unit-shard`, one
`unit-race.part<k>.json` per `go test` process); refresh the table from those
so the pieces are balanced by the runs they split:

```sh
REPOSITORY=Agent-Clubhouse/Goobers
RUN_ID=$(gh run list --repo "$REPOSITORY" --workflow CI --branch main --status success --limit 1 --json databaseId --jq '.[0].databaseId')
TIMING_DIR=$(mktemp -d)
gh run download --repo "$REPOSITORY" "$RUN_ID" --pattern 'test-timings-race-linux-*' --dir "$TIMING_DIR"
gh api "repos/$REPOSITORY/actions/runs/$RUN_ID" > "$TIMING_DIR/run.json"
go run ./test/testtiming splits \
  $(find "$TIMING_DIR" -name 'unit-race.part*.json' -exec printf -- '-timing %s ' {} \;) \
  -run-metadata "$TIMING_DIR/run.json" \
  -split github.com/goobers/goobers/cmd/goobers=3 \
  -split github.com/goobers/goobers/release=2 \
  -out .github/unit-shard-splits.json
```

The generator requires a completed successful run, one platform across all
parts, and no test measured twice, and records the run's branch. Refresh from
`main`; a PR that changes the split itself may seed from its own run's race
artifacts. Revisit the piece counts and the matrix's shard count together:
splitting helps until the largest single tests and per-job setup (checkout,
module download, and compiling the piece's test binary before it can start)
become the floor.

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
