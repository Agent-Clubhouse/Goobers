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

The snapshot comes from all 32 CLI shard artifacts in successful
[run 39013119025](https://github.com/Agent-Clubhouse/Goobers/actions/runs/39013119025).
Each stored value averages the completed top-level `pass` event's `Elapsed`
seconds from the JSONL event stream. Subtests are omitted. There are 4,617
measured tests, with 20 completed samples each (92,340 samples total). Means
are rounded half-up to the nearest millisecond. These measurements include the
fixture improvements; projected costs are not stored as measurements.

Three existing tests skipped all 20 repetitions: the deferred detached-worker
test and two subprocess helpers. Their exact names are recorded in
`source.skipOnlyTests`; they have no measured PASS cost and retain the 2-second
fallback. A completed PASS with zero reported elapsed time is a measurement
and receives the separate 10ms floor.

Jobs checked out merge commits `4d91f7f61257f2edd90271766af6acdaa86406f8` and
`7cfd8b4bd056888bc7a435955d46a87acb84b083`. Both have identical runtime and test
sources, fixtures, modules, weights, Makefile, and stress workflow. All 6,963
tracked paths, modes, and blobs outside `.github/workflows/ci.yml` and
`test/ci/runtime-acquisitions.json` match; those two files differ only by four
lines adding Windows CI coverage and its acquisition declaration. The recorded
input digest hashes the NUL-delimited `git ls-tree -r -z` records after removing
those two paths. Actual job checkout logs establish provenance; the workflow's
dispatch SHA does not identify the PR merge checkout.

To refresh, retain the JSONL event streams and summaries from every CLI shard's
artifact, plus the run metadata and actual checkout logs. Verify run, attempt,
package, platform, race mode, repetition counts, and complete test coverage.
Group completed top-level PASS durations by exact test name, average each
group's seconds, round half-up to integer milliseconds, and review the result
together with the source checkouts, conclusion, and sample counts. Keep
skip-only names explicit and on the fallback rather than assigning zero cost.
Prefer a successful run. If using partial runs, document their failures and
exclude incomplete measurements explicitly. Do not combine differing runtime
or test inputs; combining CI-only revisions requires an explicit content
identity proof and a narrow list of differing metadata files. Do not write
projected fixture costs into this file. Re-run the partition coverage tests and
verify hosted stress on the resulting commit.

The ordinary CI timing table and its refresh policy remain separate. Both
runners share only the deterministic partition algorithm.
