# Telemetry load and v0.5.0 validation

Treat telemetry performance as a release gate, not just a successful upload.
The goal is tenant-owned visibility without appreciable workflow, scheduler,
startup, or interactive latency. This guide separates repeatable automated
checks from platform measurements still required before fleet rollout.

See [Azure Monitor setup](azure-monitor.md) for opt-in configuration and
delivery semantics. Use synthetic data and a dedicated test destination. Never
load-test a production tenant without its approval.

## Hardening covered by this change

- Journal export batches up to 128 records / 256 KiB of charged input, with a
  maximum 100 ms idle coalescing delay. One individually valid larger record
  travels alone. The byte threshold is not an encoded HTTP request limit.
- The existing 1,024-record / 8 MiB admission queue includes the in-flight
  batch. No second lossy queue is added. Commit does not upload or spool.
- Durable replay submission acknowledges local persistence; the replay worker
  owns HTTP. New arrivals cannot bypass exponential retry backoff.
- HTTP holds no filesystem lock or database transaction. Each replay pass sends
  at most 32 requests with the worker's five-second context; acknowledgement
  cleanup has a separate maximum five-second allowance.
- Replay combines tiny files into bounded 128-record / 1 MiB requests, preserving
  stable identities and at-least-once delivery. Existing larger batches travel
  alone. Cross-process leases protect uploads from concurrent pruning/delivery.
- A private incremental manifest replaces per-operation directory/header scans;
  legacy metadata is cached on reconciliation. Payloads are still validated at
  delivery. The journal producer lock is released before replay inspection.
- Independent, rate-limited stderr/rotating-file warnings expose backlog pressure
  and counted losses even when ingestion is down. See the Azure Monitor guide
  for thresholds, counter scope, storage overhead, and PVC requirements.
- Cursor-store initialization failures acknowledge flush requests promptly.
  Shutdown gives source catch-up at most five seconds for a last-chance copy;
  retained journal records can resume on restart. Remote Azure delivery errors
  remain visible through health/retry accounting without making daemon shutdown
  fail; local exporter errors are not reclassified as remote failures, including
  in mixed error chains. A known failed replay initialization rejects admission
  promptly and counts the failure while background initialization keeps retrying.
  Shutdown does not wait for an unavailable manifest to attempt a final upload.
- Repeated filesystem failures while writing replay temporary files share one
  error-log signature per directory/operation/cause. Random `.pending-*`
  filenames cannot bypass suppression or churn its bounded table; emitted
  diagnostics still retain the actual error path. Failure counters continue
  counting every occurrence independently of log suppression.
- Local stream health does not declare recovery merely because an idle stream
  stops producing new failures. It retains a rate-limited `recovery_unconfirmed`
  warning until delivery advances after the last observed problem and active
  warning causes clear. This is conservative: no-traffic recovery remains
  unconfirmed rather than being treated as a successful end-to-end probe.
- Journal coalescing stops as soon as a batch reaches its record or byte limit
  (including a valid large singleton). The short idle delay remains for sparse
  traffic and is not restarted by new arrivals. A full durable catch-up batch
  does not pay the sparse-traffic delay before every acknowledgement.
- Live durable-source hints also coalesce on the background worker for at most
  100 ms from the first hint. Count/charged-byte pressure ends that window early;
  new arrivals cannot extend it. This avoids starting sparse catch-up disk work
  immediately after event fsync while the producer is still checkpointing its
  state. Known retained-backlog continuations and explicit source flushes do not
  add a coalescing timer. Cancellation interrupts the window. Hints remain
  bounded and nonblocking; retained journals, not hints, provide durability.
- A persisted, one-minute full-audit cadence supplements directory timestamps
  on the next replay operation. This finds external/interrupted file changes
  even when a filesystem coalesces timestamps. Short-lived CLI processes share
  that cadence. A locally rolled-back mutation forces the next reconciliation.
- Service-health journal export uses only operational fields from the already
  scrubbed bytes; host/account names and recovery paths remain local to that
  journal. Identity consent is applied through the independent diagnostics
  channel. The HTTP boundary also projects old queued Azure envelopes, so replay
  cannot bypass this protection. Tests must inspect JSON message bodies as well
  as top-level dimensions; field-only privacy assertions are insufficient.

These are background goroutines, **not an OS low-priority scheduling class**.
Asynchronous work still consumes CPU, memory, disk bandwidth, and filesystem
operations. Local authoritative journal fsync remains a separate cost.

Azure uploads reuse at most four gzip compressors per process. Cache misses
allocate rather than waiting; unused compressors above the bound are discarded.
Request output buffers are not cached, and a compressor is released before HTTP
starts. This bounds retained compression workspace while reducing repeated
allocation for sparse uploads. It does not change batching, durable admission,
retry behavior, compression level, or the per-stream HTTP concurrency limit.

Redaction skips replacement only when the regexp engine's required literal
prefix is absent; credential patterns and their ordering are unchanged.
This avoids copying ordinary payloads once per pattern without a second full
regex scan for actual secrets. Differential tests cover the original behavior,
including Unicode/capture replacements, and paired benchmarks include secrets
near the end of large payloads. The allocation budget runs without `-race`:
the race runtime deliberately discards regexp pool entries. Security/ownership
tests still run under the race detector.

## Run the automated checks

From the repository root, with the pinned Go toolchain:

```sh
go test -race ./internal/telemetry -run 'TestJournalLogs|TestAzureReplay|TestAzureMonitorJournalReplay|TestClientShutdownExportsFinalJournalDropCauses|TestTenantTimeline' -count=1 -timeout=3m
go test ./internal/telemetry -run '^$' -bench '^BenchmarkJournalLogsDurableHTTP$' -benchtime=4096x -count=3
go test ./internal/telemetry -run '^$' -bench '^BenchmarkAzureReplayBacklogAdmission$' -benchtime=5x -count=3
go test ./internal/telemetry -run '^$' -bench '^BenchmarkAzureReplayIndexedStats$' -benchtime=100x -count=3
go test ./internal/telemetry -run '^$' -bench '^BenchmarkAzureReplayIndexAudit$' -benchtime=3x -count=3
go test ./internal/telemetry -run '^$' -bench '^BenchmarkJournalCatchupAcknowledgedHistory$' -benchtime=14400x -count=3 -timeout=3m
go test ./internal/telemetry -run '^$' -bench '^BenchmarkJournalCatchupRetainedDirectorySweep$' -benchtime=3x -count=1 -timeout=45m
go test ./internal/telemetry -run '^$' -bench '^BenchmarkJournalCatchupCommitHint$' -benchtime=100000x -count=3
go test ./internal/telemetry -run '^$' -bench '^BenchmarkJournalCatchupRateControlledBurst$' -benchtime=1x -count=3 -timeout=30m
go test ./internal/telemetry -run '^$' -bench '^BenchmarkJournalCatchupNormalRate$' -benchtime=1x -count=3
```

The commands also work in PowerShell. Benchmarks are measurements, not hard
wall-clock CI assertions. Keep their text output with the release evidence.
The Go test timeout does not bound benchmark-only execution: the testing
package stops its test alarm before running benchmarks. Use an external process
watchdog on load hosts, retain timed-out output, and verify the actual child
exit code. Do not treat a successful remote-command submission as a test pass.
The normal-rate component benchmark offers 127 records/minute across ten
durable journals and measures Append p95/p99 directly. It is a one-minute
component measurement, not the 30-minute real-daemon or stage-dispatch gate.
Build the driver from the candidate source when replay schemas change. Its
progress output distinguishes unavailable accounting from zero backlog, and
an enabled scenario cannot pass with unavailable final accounting.
The journal/replay correctness tests are selected by the Windows CI gate and
its coverage contract; this does not substitute for native deployment testing.

For a larger normal-rate tail sample, use the separate
`BenchmarkJournalCatchupNormalRateSustained` experiment. It offers 3,810 records
over thirty minutes at the same rate, retains all append samples for p95/p99,
and reconciles the ten additional initial records. Select one subcase, for
example `-bench '^BenchmarkJournalCatchupNormalRateSustained$/bytes=32768$/enabled=true$'`,
with `-run '^$' -benchtime=1x -count=1`. Use an external watchdog of at least
35 minutes and matched disabled/enabled runs on an otherwise idle host.
Keep earlier short-run failures in the evidence; this larger sample does not
retroactively turn them into passes or measure isolated stage dispatch.

The test-only `BenchmarkRunnerTelemetryDispatchNormalRate` in `internal/runner`
measures a separate boundary: entry into task-span setup through entry into the
deterministic executor. It includes durable stage-start append/checkpoint and
scratch-workspace/envelope preparation. Executor work is stubbed; process spawn,
repository checkout, daemon scheduling and remote pod queue latency are excluded.
Identical timing wrappers run in both disabled/enabled cases. The benchmark
offers 100 runs across ten workflow definitions over thirty minutes and retains
400 task-preparation samples (logged as raw nanoseconds after measurement), then reconciles source sequences with stable
received IDs. Invoke one `enabled=false` or `enabled=true` subcase with
`-run '^$' -bench '^BenchmarkRunnerTelemetryDispatchNormalRate$/enabled=true$'
-benchtime=1x -count=1`, an external watchdog of at least 35 minutes, and the
matched disabled comparison. Ordinary regression tests check the probe boundary,
bounded sample storage, duplicate refusal and real runner/export integration;
they do not execute the thirty-minute benchmark in CI.
The timer starts at `SpanStarter.StartTask`, after the runner builds span
attributes. That earlier attribute construction is outside this measurement;
do not label this component interval as total scheduling-to-execution latency.

The HTTP fixture exercises production journal admission, batching, Azure
envelope serialization, gzip, fsynced replay files, and loopback HTTP. It sends
2,048 ~1 KiB synthetic records in 256-record waves, verifies delivery accounting
and valid envelopes, limits batches to 128, requires zero admission drops,
and requires at least 32 records/request overall. Separate deterministic tests
cover count/byte pressure, overload accounting, shutdown deadlines, no ambiguous
OTLP retry, replay restart, pruning, malformed files, stalled upload isolation,
and retry backoff under continued admission.
The indexed-spool tests also cover cross-connection claims, interrupted updates,
expired leases, initialization recovery, successful work-budget continuation,
and deterministic handoff to a waiting producer before a hot drainer reenters.
`TestAzureReplayNetworkFaultRecovery` exercises real envelope encoding, gzip,
HTTP and durable replay across 429/503 rejection, connection closure before
acceptance, and acceptance followed by a lost acknowledgement. DNS failure and
DNS timeout are injected at the HTTP transport's dial boundary, without public
DNS dependencies. It checks retained/retried accounting, continued admission,
and exact stable identities after recovery; the ambiguous case must produce
two copies of the first ID and one of the second. These short deterministic
checks do not replace the duration and workflow-latency network gates below.

The benchmark's `commit-p95-ns` samples Commit only. Its `ns/op` includes
periodic drain waits and coalescing, so it is not a maximum throughput estimate.
It does **not** execute workflows, append the authoritative journal, measure
Azure service ingestion latency, or exercise all three streams together.
`encoded-B/record` is uncompressed envelope size, not transferred gzip bytes.
The backlog benchmark starts with 0, 32, or 256 ~127 KiB batch files and grows
by one file per measured iteration; use the fixed iteration count above when
comparing results.
The indexed-stats benchmark compares empty and 12,000-file manifests after
initialization. Fixture creation is excluded; it is not a cold-start benchmark.
The audit benchmark separately forces complete metadata reconciliation with
0 or 12,000 actual files, excluding creation and initial manifest construction.
Keep its periodic cost separate from ordinary stats/admission costs.
The acknowledged-history benchmark repeatedly visits 128 warm, enrolled run
journals and scales the per-visit measurement to 14,400 visits (ten runs every
three minutes across a 72-hour retention window). It is not an actual
14,400-directory cold-cache scan. On the same local host, an EOF fingerprint
reduced this estimate from 2.21–2.28 seconds to 1.16–1.23 seconds and allocations
from about 35.5 KB to 3.2 KB per visit. Native filesystem results still matter.
The persisted fingerprint checks size/mtime of events, run identity and schema
metadata; it is a cache, not an integrity signature. External edits/restores
must publish changed size/mtime. Appends invalidate the cache. Scheduler
generation/identity checks are not skipped. Cursor schema migration preserves
enrollment and acknowledged positions; older entries validate before caching.
The separate retained-directory benchmark creates **14,400 actual run
directories**, each with a journal and acknowledged persisted cursor. One
operation visits all 14,400 directories; `actual-14400-sweep-ms` is measured,
not extrapolated from 128 fixtures. Setup/enrollment/first fingerprint pass
are excluded, and fixture creation itself can take minutes on fsynced storage.
Place the Go test temporary directory on the filesystem being validated.
This remains a warm reader/cursor-path measurement, not a cold-cache guarantee,
paced discovery timing, or concurrent daemon latency measurement.

The commit-hint benchmark measures the production catch-up notification path
with both an empty and a full 1,024-entry hint queue and a 32 KiB input body.
It checks deferred-hint accounting and reports Commit p99; it does not measure
journal fsync or background recovery. The durable-HTTP benchmark also reports
Commit p99 separately from its overall coalescing/drain time.

The rate-controlled burst benchmark compares disabled/enabled export for
1 KiB and 32 KiB event payloads. Ten concurrent durable journals receive
212 records/second for 60 seconds (12,720 offered records, plus ten initial
run-start records). This is approximately 100 times the illustrative rate of
ten workflows × 38 events every three minutes, not a known deployment rate.
It reports achieved rate and journal-append p95/p99, fails if production of
the offered records takes over 66 seconds, then reconciles actual persisted
journal sequences against received stable IDs within 60 seconds. Duplicate
copies must retain identity; pruning is not allowed in this healthy-export
case. Set `TMPDIR` (or Windows `TMP`/`TEMP`) to the test filesystem and use
`-benchtime=1x`; each subcase is a fixed experiment, not benchmark calibration.
The fixture is a journal/telemetry component test, not a daemon stage-dispatch
or full workflow benchmark. Keep it separate from the real-daemon matrix and
do not describe an offered rate as achieved unless its result confirms it.

## Local evidence, not fleet certification

Measured on macOS/arm64, Apple M4 Max, 2026-09-27, with real replay-file fsync:

| Check | Result |
| --- | --- |
| 2,048-record durable HTTP check | 16 requests; 128 records/request; zero drops |
| 4,096-record HTTP benchmark, three final runs | 128 records/request; zero drops; Commit p95 875–1,125 ns |
| Admit a batch with ~32 MiB / 256 files before metadata optimization | ~90.6 ms; ~82.0 MB allocated/admission |
| Same fixed fixture after metadata optimization | ~10.0 ms; ~1.44 MB allocated/admission |

The last comparison is historical, before the incremental manifest. It is a
small five-iteration diagnostic, not a statistically
established platform guarantee. It identifies and substantially reduces a real
full-backlog-read cost. At that point directory enumeration and legacy reads
still scaled with file count. Do not extrapolate it to a 512 MiB cap, slow
Windows disks, a PVC, or the subsequent manifest implementation.

Local validation also passed the full telemetry race suite, focused repeated
race regressions, journal/engine/CLI
integration-boundary tests, `make verify-fast`, `make lint-fast`, complexity
and Markdown checks, and the Windows test-selection contract. Passing the
selection contract verifies CI wiring, not execution on a Windows host.

### Follow-up real-daemon stress findings: not a passing sign-off

A local scratch harness subsequently ran the real daemon and credential-free
four-stage workflows, with real fsync, two gaggles, a loopback faulting receiver,
and two-minute scenarios. The initial six-case matrix completed 716 workflows
without workflow or health-check failures, but exposed these blockers:

- 12,000 tiny replay files (~5 MiB initially): 92.9% mean sampled daemon CPU,
  210 MiB peak sampled RSS, and 15,985 records still pending after approximately
  one minute of restored ingestion with continuing work. Byte caps alone do not
  bound file-count scan cost or guarantee catch-up.
- 64 MiB of legacy-format replay files: 330 MiB peak sampled daemon RSS and
  69.2% mean sampled CPU; repeated payload reads during accounting remain costly.
- Healthy export with 16 concurrent callers: 51 of 3,952 authoritative run
  journal sequence keys absent after clean shutdown and an empty replay spool.
  The daemon logged queue-lock contention drops. Two separate API submissions
  were explicitly rejected by the mutation concurrency guard.
- An unusable telemetry spool path caused otherwise-valid daemon configuration
  to fail startup. Export initialization is not fully failure-isolated.

These are serial, warm-cache macOS measurements from an uncommitted local
harness, not portable benchmarks or statistically isolated causal estimates.
They nevertheless demonstrate concrete delivery and resource gaps. Fix bounded
admission, incremental/cross-process-safe spool accounting, small-batch replay,
legacy metadata handling, and runtime spool-failure isolation before treating
the exporter as volume-safe. Then rerun the matrix; passing unit tests does not
override these findings.

### Indexed-spool follow-up

The incremental manifest, combined uploads, bulk acknowledgement, work-budget
continuation, and fair admission gate were subsequently exercised on the same
local setup. A hot drainer could otherwise starve new admissions between OS-lock
polls; a deterministic regression now checks producer-first handoff.

With 180 seconds of continued work and ingestion restored at 60 seconds:

| Fixture | Completed workflows | Mean sampled CPU / peak RSS | First sampled empty spool |
| --- | ---: | --- | --- |
| 12,000 tiny files | 154 | 18.2% / 114.7 MiB | 141 s; 81 s after restoration |
| 520 MiB prefill / 512 MiB cap | 157 | 20.2% / 120.5 MiB | 122 s; 62 s after restoration |

Both had zero workflow/health-check failures, zero sampled failed admissions,
and no received duplicate IDs; near-cap replay stayed below its logical byte
ceiling. The independent warnings reported backlog pressure, intentional pruning,
and queue drops while the receiver was offline. A real kill/restart recovered
all 1,372 persisted IDs captured before the kill.

These are **not zero-loss or full timing passes**. Thirty of 5,852 run sequences
were missing after the tiny-file run, and 22 of 5,966 after near-cap shutdown,
despite empty spools. The crash trial missed eight run sequences outside its
durable checkpoint. Journal queue admission and startup isolation still need
corrective work; the one-minute recovery target was missed. The longer trials
do not replace the shorter failed checks. Shared-workstation conditions and
unreplicated timing comparisons prevent a fleet-overhead guarantee.

## Updated validation and repeatable driver

### Durable-source follow-up

After atomic queue admission, nonfatal storage initialization and journal
cursor catch-up, the same macOS synthetic workload produced:

| Fixture | Workflows | Reconciled run events | Duplicate copies |
| --- | ---: | ---: | ---: |
| Healthy, 60 s | 60 | 2,280 / 2,280 | 0 |
| 12,000 tiny files, 180 s, restore at 60 s | 169 | 6,422 / 6,422 | 0 |
| 520 MiB prefill, 180 s, restore at 60 s | 175 | 6,650 / 6,650 | 0 |
| Kill/restart, 60 s each lifetime | 117 | 4,446 / 4,446 | 17 |

All had zero workflow/health-check failures; the restarted run also recovered
all 3,254 captured pre-kill spool IDs. Tiny-file and near-cap cases reached an
empty spool by the roughly 105-second sample. These are correctness results,
not matched performance guarantees; some checks overlapped other local builds.
The earlier failed tests above remain historical evidence, not passing results.

A small real Azure canary made all 38 run sequences query-visible under its
run operation ID. Azure held 76 copies with 38 distinct stable IDs: dedupe is
mandatory, and a successful upload alone must not be called reconstruction.
A native AKS Azure Disk CSI PVC smoke run completed 89 workflows and reconciled
3,382 / 3,382 run events with no duplicates or workflow/health failures. Native
Windows journal/replay regression binaries passed with Defender enabled.
These preliminary checks do not replace the service-account and 24-hour gates.

### Maintained real-daemon driver

The integration fixture builds the actual daemon and runs a short loopback-only
reconciliation test. It requires Go, Git, and `ps` (PowerShell on Windows):

```sh
go test -tags=integration ./test/telemetryload -count=1 -timeout=5m
go build -o bin/telemetry-load ./test/telemetryload/testdata/driver
go build -o bin/goobers ./cmd/goobers
bin/telemetry-load -bin bin/goobers -out /tmp/telemetry-enabled -scenario enabled -duration 30m -workers 10 -poll-interval 3m -sample-interval 10s
```

Use a **new or empty** output directory each time. On Windows use `.exe` paths
and explicitly add `-windows-insecure-demo`: the bundled deterministic,
credential-free demo has no native Windows network isolation. This flag supplies
both scaffolding consent and the trusted-local execution environment opt-out
only to the fixture's child processes. This does not
change the deployment's sandbox policy or certify isolation. Do not substitute
untrusted workflows. Child processes have ambient provider/telemetry credential
variables removed. On Linux make the daemon executable readable/executable by
the workload identity, not just the host root user.

Scenarios: `baseline`, `enabled`, `outage-recovery` (503 and stalled requests),
`tiny-files`, `near-cap`, `legacy`, `spool-failure`, and `crash` (two lifetimes).
`all` runs the first seven sequentially. `-recovery-after 60s` sets restoration
time for prefills; use at least three minutes for their initial drain checks.
`-profile health|standard|diagnostic` selects collection. Health-only and disabled
profiles do not assert run-journal export. The driver fails on workflow/health
errors, measurement errors, shutdown over 20 seconds, or unexplained missing
run/spool records in eligible loopback scenarios. Burst overload tests need a
separate explicitly justified loss policy, not silently relaxed assertions.

After workload submission stops, eligible scenarios wait up to `-settle-timeout`
(default two minutes) for all authoritative run sequence keys before shutdown.
`CatchupWaitMS` reports that time separately. An empty replay spool is not a
catch-up barrier: source discovery may still have retained journals to read.
A 90-second storage-obstruction trial without this wait ended with 456 pending
run records; a repeated trial with reconciliation allowed recovered all 3,496
records, including 3.3 seconds of post-workload catch-up. The first trial remains
a failed immediate-shutdown reconciliation, not evidence of permanent loss.

For a 24-hour representative run use `-duration 24h -workers 10 -poll-interval 3m
-sample-interval 10s`. This exercises actual runs, so it overstates a deployment
where most polls find no work. Sample JSONL, authoritative journals and daemon
logs stay in the output directory.
Positive poll intervals stagger the workers' initial schedules. Zero intervals
retain simultaneous burst submissions. Ten concurrent CLI mutations can hit
the API's four-request admission guard even with telemetry disabled; preserve
those rejected-request results as API-pressure evidence, not telemetry losses.
Result JSON stays in the output directory. Disk size is logical file bytes,
not filesystem allocation. Unix CPU values use platform-dependent `ps` estimates; Windows
PowerShell uses interval process CPU and itself has sampling overhead. Final
process CPU seconds are also recorded. Normal daemon heartbeats remain enabled
and capture heap/retained memory, goroutines and container pressure in the daemon
log. Samples include Windows handle counts or Linux file descriptors (`-1` on
macOS). These are trend samples, not allocation/stack profiles or disk-latency
and stage-dispatch instrumentation; collect those separately for the proposed
gates below. It does not automatically certify every table entry. Replay
inspection supplies pending counts, not other processes' cumulative loss counters;
use daemon health warnings and journals for those, not zero-valued inspector fields.

### Repeated startup and shutdown measurement

Use the separate `startup` scenario for idle daemon lifecycle comparisons:

```sh
bin/telemetry-load -bin bin/goobers -out /tmp/telemetry-startup-half-cold-stalled -scenario startup -startup-rounds 20 -startup-prefill half -startup-index cold -startup-endpoint stalled
```

Run this only on an otherwise idle validation host, with an external watchdog.
It creates fresh matched disabled/enabled instances and alternates pair order.
`startup-results.json` retains every raw launch-to-ready and shutdown duration,
request counts, and actual pre/post replay file counts and bytes. Both `/readyz`
and the instance API must answer successfully; readiness polling has 50 ms
resolution. No workflows are submitted. Shutdown uses a 15-second drain setting;
the larger watchdog is not the acceptance limit. Independently compare disabled
and enabled p95 against the 250 ms added-startup budget, and each shutdown
against 15.250 seconds. Use at least 20 samples per variant; the one-pair CI
smoke exercises harness correctness only, never performance acceptance.

Prefills are `empty`, `half` (256 MiB), `near-cap` (460 MiB), `cap` (512 MiB),
`tiny-files` (12,000 single-record files), or `legacy` (64 MiB without header
record counts). Byte prefills stop after a complete file, so inspect actual
occupancy rather than assuming an exact size. These are separate file-shape
cases, not every combination of payload size and file count. Warm priming can
prune an over-cap seed; actual measurement-start occupancy is recorded.

`cold` requires a missing replay manifest. `warm` first runs and cleanly stops
the daemon against a rejecting endpoint, waiting for usable accounting when
export is enabled, then restarts the same instance without copying its index.
The measured endpoint is either healthy or stalled for seven seconds, longer
than the exporter request deadline. Five seconds of idle settling separates
preparation from timing (`-startup-settle` overrides it). Seed file writes are
synced before measurement. **Neither mode means OS-cache-cold**; the driver does
not flush shared machine caches. Credentials cannot direct this scenario to an
external Azure endpoint, and it refuses a present `GOOBERS_DISABLE_FSYNC`.

After each child exits and occupancy is captured, the driver removes only its
marked instance's known synthetic seed payload files to bound fixture disk
growth. Logs, manifests, local journals, and timings remain. Those post-cleanup
fixtures are deliberately **not delivery-reconciliation evidence**. Failed
samples retain their fixture for inspection; a command success or final empty
spool does not certify all lifecycle budgets or crash durability.

Only the explicit `azure` scenario accepts `-azure-connection-env NAME`, with
the connection string already in that environment variable. It never uploads
volume prefills. It reports `MissingRunEvents: -1` until a separate Azure query
reconciles instance/run/sequence and stable IDs. A connection string is not an
Azure query credential. Never put it in arguments, evidence, or source control.

The explicit `network-faults` scenario divides its duration into six equal
phases: 503, request timeout, 429, connection closure before acceptance,
acceptance with a lost acknowledgement, and healthy recovery. Use 30 minutes
with the normal ten staggered workers and three-minute polling interval.
`NetworkModes` reports request counts by fault; validation fails if any phase
received no requests or ambiguous acknowledgement produced no duplicate replay.
The existing unique journal-sequence reconciliation still must pass. Durations
too short for retry backoff may legitimately fail phase coverage. This tests
loopback HTTP faults, not real DNS outages, WAN behavior or a 30-minute outage
for each individual fault; dial-level DNS errors have separate unit coverage.

For actual filesystem exhaustion on Linux, `-scenario disk-full` additionally
requires `-disk-full-volume /absolute/path`. Prepare a **dedicated** empty
tmpfs (for example a Kubernetes memory-backed `emptyDir` with a 16Mi limit)
containing only a regular file named `.goobers-telemetry-load-volume`. The
driver rejects ordinary filesystems, unmarked/nonempty directories, symlink
roots, and tmpfs volumes over 64MiB. Keep the instance and result directory on
a different filesystem. Only `telemetry-export` is linked to this fault volume.
During the middle third of a 30-minute run, an exclusively created filler
forces real `ENOSPC`; the final third removes only that same owned filler and
checks normal journal reconciliation. `DiskFullConfirmed` must be true and
`DiskFullBytes` positive. Deferred cleanup also removes the owned filler on an
ordinary failure. Preserve the result and independent daemon warnings.
`DiskKiB` does not follow this export-directory symlink, so it excludes the
fault filesystem; collect its `df` usage and replay statistics separately.
This tests exporter failure isolation/recovery, not persistent tmpfs delivery,
slow disk behavior, or Windows disk-full behavior. Never fill a general-purpose
disk to emulate this fixture.

## Establish the representative workload

Start with two gaggles per instance and five polling workflows per gaggle,
every three minutes. That is 200 poll opportunities/hour/instance, not
necessarily 200 runs. Confirm whether five workflows means per gaggle or per
instance; the latter halves that estimate. Measure actual records/run and
record sizes, including successful polls that do no work.

For illustration only, 20 journal events per poll opportunity gives about
1.1 records/second/instance and 111 records/second across 100 instances. The
fleet total stresses ingestion; the per-instance rate stresses the daemon.
Do not treat 100 users as 100 instances until deployment topology is known.

Run identical deterministic workloads in export-disabled, `health`, `standard`,
and `diagnostic` configurations. Keep local journaling and fsync unchanged.
Use all configured streams concurrently. Measure a normal rate, 10× that rate,
and a 60-second 100× burst. Also include synchronized poll boundaries and
1 KiB / 32 KiB / near-limit records. These multipliers are test levels, not
claims about supported capacity. Full-workflow snapshots are not included.

## Required platform matrix

- Windows service, normal enterprise account, Defender/endpoint protection
  enabled, actual deployment filesystem and ordinary developer hardware.
- Kubernetes, actual CPU/memory requests and limits, PVC storage class and
  filesystem, one active owner per instance root. Test container restart and
  replacement pod on the same PVC, plus CPU throttling and storage latency.
- Native macOS and Linux smoke/load runs. A macOS result does not certify
  Windows or Linux; a Linux unit run does not certify a Kubernetes PVC.

Record build SHA, OS/CPU, resource limits, storage, profile, payload sizes,
rates, replay caps, and whether antivirus and fsync were enabled. Run at least
three matched disabled/enabled comparisons after a five-minute warmup.

## Proposed release acceptance criteria

These are initial sign-off targets, **not achieved results or shipped SLOs**.
If a target fails, retain the evidence and either fix the issue or explicitly
agree a revised capacity envelope; do not silently relax it.

| Scenario | Duration and acceptance target |
| --- | --- |
| Normal operation | 30 min/profile/platform. Zero queue drops, pruning, or unexplained missing records. Incremental daemon CPU ≤5% of one core; steady incremental RSS ≤32 MiB. |
| User-facing latency | At normal load, journal append and stage-dispatch p95 regression ≤max(1 ms, 10% of baseline), p99 ≤max(5 ms, 20%). Interactive health/status calls remain responsive. Measure telemetry Commit separately: p99 <1 ms for ≤32 KiB inputs. |
| Batching | Under sustained backlog, small-record batches average ≥32 records/request, never >128; input-byte threshold respected except valid singletons. Idle/sparse records are allowed to send singly. No one-spool-file-per-event behavior during a burst. |
| 10× sustained / 100× burst | 30 min / 60 s. No deadlock or unbounded memory growth. At 10×, no drops on the declared supported hardware. Burst overload may drop exported copies only if fully counted; local journals remain complete and workers recover within 60 s after load returns to normal. |
| Network failure | 30 min of timeout, DNS failure, 429, 503, and connection reset, including an ambiguous acknowledgement. Normal workflow latency budgets still hold. Retry spacing respects backoff despite new records; one in-flight request per replay stream. No retry/log storm. |
| Backlog / storage | Repeat at 0%, 50%, 90%, and cap with both large batches and many tiny batches. Include pre-upgrade spool files, read-only/unwritable directory, disk-full, and slow disk/PVC. Steady incremental RSS ≤64 MiB during outage, with no sustained upward trend. Age/byte pruning must be visible and remain within the configured published-file bound after enforcement. Allow temporary write/file-system overhead separately. |
| Recovery | Restore the endpoint while new runs continue. For a backlog sized to 10 min of normal work, catch up within 10 min while preserving normal-work latency. Compare unique record IDs; retries may duplicate delivery. Larger backlogs need an explicitly measured drain rate and ETA. |
| Startup / shutdown | Compare empty, half-full, and near-cap cold/warm starts, online and offline. Added startup-to-ready p95 ≤250 ms and no waiting for HTTP; shutdown respects its configured deadline within 250 ms. Persisted unacknowledged batches survive forced termination; unspooled memory queues are not crash-durable. |
| Soak | 24 h on Windows and the deployment PVC, including outages and a restart. No growing goroutine/handle count, no sustained heap/RSS growth, no unexpected disk growth beyond configured telemetry storage and authoritative journal retention. |

Do not size storage alone and assume the problem is solved: file count, scans,
fsync latency, antivirus, GC, and shared disk traffic can be the limiting factor.
Near-cap behavior remains a required gate. Test both a cold manifest rebuild and
warm incremental accounting, and include many tiny files rather than only large
batches. Raising the storage cap is not a substitute for efficient catch-up.

For every scenario retain CPU/RSS and Go heap/allocation/goroutine profiles,
disk bytes/operations/latency, append/dispatch/Commit percentiles, startup and
shutdown timings, queue/drop causes, replay pending age/bytes/records, retries,
pruning, request count/size, and unique received record IDs. Compare journal
sequence/run identity to destination records; queue admission is not proof of
remote delivery. Use local stats during an outage because remote health
telemetry cannot report through a broken endpoint.

Check the independent health files and stderr explicitly: a sustained outage
must produce a backlog warning, injected journal/diagnostic losses must be
counted, repeated warnings must be rate-limited, and restoration must produce a
recovery transition without feeding warnings back into the export queue. Capture
admission failures separately from queue drops and intentional cap pruning.
Keep retry-discovery time separate from catch-up throughput, but include both
when judging the total recovery target. A longer follow-up run must not overwrite
or relabel a failed shorter timing check.

## Sign-off record

- Build SHA / platform / hardware / storage:
- Profile / instance count / workload / observed events per second:
- Disabled baseline versus enabled results:
- Backlog sizes and file counts, including legacy files:
- Failure/restart scenarios and reconciled losses/duplicates:
- Evidence paths and remaining exceptions:
- Owner / date / rollout decision:

Start with a small opted-in canary group; expand only after native Windows and
Kubernetes gates pass. This change does not certify a 100-user deployment by
itself, nor does it add full workflow-content export.
