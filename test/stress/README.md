# Stress shard measurements

The runner enumerates every current top-level test, removes explicit enrollment
exclusions, and uses the shared `internal/testshard` partitioner to place the
longest measured tests on the lightest shard. Equal weights sort by test name;
equal shard totals sort by shard index. Every test still runs in exactly one
shard, with the enrollment's existing repetition count and timeout.

`weights.json` is embedded in the runner so all jobs use the same immutable
snapshot. Values are integer milliseconds. Measured zero-duration tests have a
10ms scheduling floor. New or renamed tests use `defaultMillis` (2 seconds),
which is a scheduling estimate, not a measurement. Missing package data or
invalid weights fail the run instead of silently reverting to count balancing.

The initial snapshot comes from all 32 CLI shard logs in
[run 38877589113](https://github.com/Agent-Clubhouse/Goobers/actions/runs/38877589113)
at commit `7da8bb4b88bca9ab96b839657512d3a2a0156898`. The overall run failed when
ten shards timed out. Each stored value averages only completed top-level
`--- PASS: TestName (seconds)` measurements; interrupted executions and subtests
are omitted. There are 4,606 measured tests, with 4–20 completed samples each.
Means are rounded to the nearest millisecond. These are the observed costs
before this PR's fixture changes; projected improvements are not measurements
and are deliberately absent from the snapshot.

To refresh, retain the full logs from every CLI shard of one Linux race stress
run (`gh run view RUN_ID --repo Agent-Clubhouse/Goobers --log`) and its run
metadata. Group the completed top-level PASS durations by exact test name,
average each group's seconds, round to integer milliseconds, and review the
result together with the run, commit, platform, conclusion, and sample counts.
Prefer a successful run. If using partial runs, document their failures and
exclude incomplete measurements explicitly. Do not combine measurements from
different commits or write projected fixture costs into this file. Re-run the
partition coverage tests and verify hosted stress on the resulting commit.

The ordinary CI timing table and its refresh policy remain separate. Both
runners share only the deterministic partition algorithm.
