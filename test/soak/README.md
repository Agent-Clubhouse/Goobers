# Real-stage soak driver

This is the driver delivered by #1479. It runs the real `goobers init --demo`,
`goobers up`, and `goobers run --force --no-wait` commands. It replaces the demo
workflow with the offline native Go fixture subcommand, which creates temporary files, opens
file descriptors in subprocesses, commits a small local git repository, and
cleans up. Every tenth submission deliberately fails with the exact structured
`soak_fixture_failure` code/message. No provider writes or model calls are used.

The **environment supervisor remains #1480**. This command does not launch Docker,
apply resource flags, verify block-device throttling, attribute host OOMs, collect
SIGQUIT dumps, or package evidence. The supervisor must provision one exclusive
container on native Linux with the selected profile's CPU, memory and disk limits,
install `goobers`, `soak`, `git`, and `stress-ng`, and permit the existing
`run.network: none` isolation mechanism. It must collect evidence before teardown
and override an inner verdict with `invalid/host-oom-killed` when host evidence
shows the observer or workload was killed. An inner pass alone does not attest
that the outer environment was correctly isolated. Scheduled execution is #1481.

Build the binaries before entering that runtime:

```sh
go build -o goobers ./cmd/goobers
go build -o soak ./test/soak
```

Inside the provisioned native Linux container:

```sh
./soak --profile smoke --goobers /opt/goobers --root /evidence/new-instance > /evidence/result.json
```

Workflow commands invoke the running driver's executable as `soak fixture success`
or `soak fixture failure`; no shell script or runtime Go build is used. Keep that
binary available through drain. The fixture uses a 50-second internal deadline,
context-bound git subprocesses, and a temporary repository inside the disposable
stage workspace. It removes the repository before returning, ignores ambient git
configuration, and holds a three-second overlap window before writing its result.

The root must not exist. The driver retains it on every outcome. It refuses native
macOS/Windows, missing Docker markers, recognized Docker Desktop/WSL kernels, and
missing, unlimited or mismatched cgroup-v2 CPU/memory limits before starting any
subprocess. These checks are prerequisites, not a substitute for #1480's runtime
verification. There is no bypass flag or native-host pressure mode. Do not run
pressure against a shared developer host.

| Preset | Runs | Ramp | Sustain | CPU | Memory MiB | Disk read/write MiB/s |
| --- | ---: | --- | --- | ---: | ---: | ---: |
| smoke | 2 | 30s | 75s | 2 | 1024 | 32/16 |
| standard | 4 | 30s | 30m | 2 | 2048 | 16/8 |
| hostile | 8 | 30s | 60m | 1 | 1024 | 8/4 |

Names are versioned contracts; changing a workload requires a new name. Unknown
profiles fail before runtime setup. The separate `stress-ng` process saturates
the assigned CPUs, exercises one memory worker using a quarter of the memory
ceiling, and uses one bounded 32 MiB disk worker. It starts after daemon health
and stays alive through the bounded drain. Its early exit invalidates measurement.

The driver opens slots gradually during ramp and replaces completed runs on a
one-second polling cadence. Durable trigger acceptances are tracked until the
public trigger-status API supplies run IDs; capacity holds are reobserved rather
than duplicated. Actual run `StartedAt`/`FinishedAt` intervals must show a
positive-duration overlap of N runs during ramp. Otherwise the ramp is refused,
new submissions stop, and drain begins. The ramp is checked again against final
terminal timestamps so stale running summaries cannot manufacture overlap.
During sustain, every continuous interval below N running workflows must stay
under 10 seconds, the fixed replacement grace matching the CLI submission
timeout. Reaching 10 seconds fails the distinct `sustainedConcurrency` signal,
even when completions remain frequent. Queued acceptances occupy outstanding
slots but contribute nothing to actual concurrency. Interval reconstruction also
counts short runs that start and finish between polls.

Run observations use the paginated public HTTP `readservice.RunListOptions` seam,
filtered by workflow, phase, and the outstanding submissions' start-time window.
Completed summaries are retained as slots are replaced, so polling does not
repeatedly scan the entire soak history. The
driver evaluates throughput using each completed run's `FinishedAt`, including
runs admitted during ramp but completed during sustain. Every empty 60-second
window fails, including gaps before the first completion, between completions,
and at the end. A late recovery cannot erase a stall. Expected fixture failures
never count as completed throughput. Only the exact failure workflow, failed
phase, and reserved terminal reason are exempted from the failure signal; an
infrastructure failure in that workflow is still a failure. Other unexpected
terminal outcomes fail conservatively, even if their cause cannot be classified.

Drain is bounded to the fixture's 60-second stage timeout after sustain (or ramp
refusal). Unresolved acceptances and nonterminal runs are reported as wedged.
Pressure and daemon children are interrupted after observation, with five-second
forced-exit bounds. Cancellation is invalid observation, never a pass. HTTP errors,
partial read responses, cursor loops, or admission-log write failures invalidate
the observation path.

The final JSON has `verdict` (`pass`, `fail`, `invalid`), the resolved profile
(durations are Go duration nanoseconds), distinct health signals, admission
decisions, completion count, expected failures, unexpected run IDs, and wedged
identities. Exit codes are 0/1/2 respectively. Invalid results have null health
signals, not fabricated zeros. `longestUnderfill` reports the longest continuous
sustained interval below N, in duration nanoseconds. A refused ramp has null
throughput, sustained concurrency, and longest underfill because no full sustained
window was measured. Invalid reasons are the closed set in the design:
`container-launch-failed`, `daemon-health-check-failed`, `load-injector-crashed`,
`host-oom-killed` (outer supervisor), and `observation-path-lost`.

`profile.json`, synced `admissions.jsonl`, `daemon.log`, `pressure.log`, the durable
trigger acceptance database, and actual run journals remain in the instance for
#1480 to package. Admission reason strings are the design's four stable codes;
ordinary acceptances awaiting dispatch are intermediate states, not fabricated
capacity refusals.

For development, ordinary package tests use a deterministic backend and clock:

```sh
GOMAXPROCS=2 go test -race -p 1 ./test/soak
```

An optional integration test uses a separately built real CLI and two overlapping
fixtures, one successful and one deliberately failed. It starts **no pressure**
and produces no soak verdict:

```sh
SOAK_TEST_GOOBERS=/absolute/path/to/goobers GOMAXPROCS=2 go test -race -p 1 ./test/soak -run TestRealCLIBackendWithoutPressure -v
```

Those checks validate driver behavior and CLI integration. They do not establish
extended throughput under saturation; that measurement requires #1480's runtime.
