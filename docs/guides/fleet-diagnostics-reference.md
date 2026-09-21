# Fleet diagnostics reference collector and queries

The executable reference consumer demonstrates company-owned collection of
opted-in diagnostic records. It does not deploy a hosted service, enroll an
instance upstream, file an issue, or contact an owner. Production fleet storage,
receiver authentication, TLS, inventory maintenance and alert delivery remain
company responsibilities.

Run the synthetic example from the repository root:

```sh
go run ./test/fleetdiagnostics > fleet.json
```

This starts an ephemeral loopback OTLP/gRPC Logs server, sends real protobuf
exports for two synthetic companies and two gaggles each, and prints JSON query
snapshots. Both companies deliberately use the same deployment/instance IDs;
authenticated tenant identity prevents collisions. The example uses fake bearer
credentials on loopback, not a production authentication scheme. A fake backend
clock crosses the exact missing-heartbeat boundary without sleeping.

Useful queries against that output:

```sh
# Inventory and explicit owner routing for one company.
jq 'select(.tenant=="company-a" and .phase=="initial") | .reports[] | {deploymentId,gaggleId,state,reason,ownerRef,ownerRoute}' fleet.json

# Missing heartbeats; this does not prove that the daemon crashed.
jq 'select(.phase=="two_missed_intervals") | .reports[] | select(.liveness=="unreachable") | {organization,gaggleId,reason,receivedAt}' fleet.json

# Actionable older builds, excluding intentional pins and maintenance windows.
jq '.reports[] | select(.freshness.status=="outdated" or .freshness.status=="pin-mismatch") | {organization,gaggleId,ownerRoute,freshness}' fleet.json

# Features actually observed in use, distinct from configured-but-unused.
jq 'select(.phase=="initial") | .reports[] | {organization,gaggleId,used:[.features[] | select(.state=="used")]}' fleet.json

# Queue stalls, missing workers, retry loops and recovery transitions.
jq '.reports[] | select(.state=="stalled" or .reason=="worker_unavailable" or .reason=="repeated_no_work") | {organization,gaggleId,reason,transitions,ownerRoute}' fleet.json
```

A production alert evaluator must run queries periodically. It compares the
receiver's current time against the last **new** heartbeat or enrollment time.
At `heartbeatInterval * missedIntervals` without a new observation, the
reference result becomes `unreachable / missing_heartbeat`. Duplicate sequences
and replayed old boots do not refresh this clock. An independent indication
that the collector itself is unavailable changes the result to
`unobserved / collector_unavailable`; absence alone cannot locate the outage.

## Version-one wire contract

Records are OTLP Logs with a string body naming `goobers.fleet.heartbeat` or
`goobers.feature.usage`. All attributes are scalars. The decoder rejects unknown
fields, unknown versions, duplicate keys, nested payloads and invalid values.
It also accepts the independent export transport's
`goobers.diagnostics.schema_version=1` attribute. Raw prompts, code, issues,
configuration and arbitrary error text are not part of this contract.

Both events require `schemaVersion=1`, `deploymentId`, `instanceId`, `component`,
`bootId`, `bootStartedAt`, `sequence`, `observedAt`, `windowStart` and
`windowCoverage`. Timestamps are RFC3339Nano strings; sequence is a positive
integer. Coverage is `complete`, `partial` or `unknown`. `gaggleId` is omitted or
empty for a deployment-level observation. Optional `organization`,
`environment` and `ownerRef` are explicit operator metadata. All string fields
are limited to 256 UTF-8 bytes and exclude control characters.

Organization is a label, **not authentication**. The reference receiver obtains
tenant identity from its required authorizer, then admits only explicitly
enrolled deployment/instance/gaggle keys with matching company/environment.
Queries require a tenant scope; they never return another company's reports.
Owner references resolve only through that company's configured owner map.
Unknown references produce no route; no runtime account is used as a guess.

Heartbeats additionally require `state` and `reasonCode`. State is one of
`productive`, `idle`, `paused`, `waiting`, `backoff`, `stalled` or `unknown`.
Optional evidence consists of `lastUsefulProgressAt`, `oldestEligibleAt`,
`eligibleCount`, `inflightCount`, `admissionLimit`, `missingWorkerCount`,
`retryCount`, `noWorkCount`, `version`, `buildCommit`, `channel` and `platform`.
Counters are non-negative; omission means unknown, never zero. Reason codes
are a fixed catalogue in `internal/fleetdiagnostics/decode.go`, including
`worker_unavailable`, `waiting_for_gate`, `provider_throttled`,
`stage_within_deadline` and explicit observation-gap reasons. A live observation
is distinct from useful progress. Intentional pause, user wait and a valid
long stage must not be inferred to be stalled simply from elapsed time.
Partial coverage cannot prove healthy idle or a stalled condition.

Feature events require `featureId`, `configured` and `windowEnd`; `count` is an
optional absolute count, not an increment. The finite catalogue is:

- `runner.local`, `runner.engine`
- `adapter.copilot`, `adapter.claude`, `adapter.codex`
- `provider.github`, `provider.gitea`, `provider.azure-devops`
- `dsl.v1`, `dsl.v2`, `dsl.v3`

These DSL identifiers describe the major interpreter families, not every
configuration revision. Arbitrary `capability.*` values are rejected until an
explicit catalogue addition defines their meaning. A missing feature record
has unknown configuration and usage. Positive partial counts prove observed
use; zero requires complete coverage to mean unused.

The backend keeps the latest absolute feature window per current boot and
feature. Duplicate or decreasing sequences do not add counts. Restart clears
the previous boot's feature view; new observations must identify the new boot.
This reference deliberately does **not** sum overlapping windows or claim a
cross-restart historical usage total. Boot start time plus boot ID prevents a
replayed old boot from replacing a newer one. Future observations beyond the
configured skew budget are rejected; stale source timestamps remain unknown
even if a delayed packet arrives now.

## Approved versions and bounded storage

The company supplies a catalogue mapping release channels to desired SemVer
versions, its source, observation time and maximum age (24 hours by default).
The consumer performs no network lookup. Invalid/dev builds, unknown channels,
unavailable catalogues and stale catalogues produce `unknown`, not upgrade
alerts. Matching versions are `current`, older versions `outdated`, and newer
versions `ahead`. Explicit enrollment pins report `pinned` or `pin-mismatch`.
A scheduled maintenance window reports `scheduled` before its start and
`maintenance` during it; ordinary comparison resumes at its end. Every result
includes the comparison source/time and last observed version.

This is an in-memory executable reference, not a durable fleet database. Bounds
are enforced at the actual ingest/enrollment paths: 1,024 enrolled identities
across at most 64 tenants, one heartbeat and one window per documented
feature per identity, 16 recent condition transitions, 32 catalogue channels,
and 128 owner routes per tenant. `Remove` explicitly retires enrollment and
its observations. Silence never automatically deletes inventory.

A request is limited to 1 MiB and 128 records; a record to 64 KiB and 48 fields.
Receiver servers must also set `grpc.MaxRecvMsgSize(MaxRequestBytes)` as the
fixture does, so the transport rejects excess bytes before decoding. Rejected
records produce an OTLP partial-success count without echoing private content.
The consumer retains no raw input log. Local diagnostic collection/export and
support bundles are separate surfaces; this fixture does not replace them.

A local parser/store benchmark (`go test ./internal/fleetdiagnostics -run '^$'
-bench '^BenchmarkFleetHeartbeat$' -benchtime=1000x -benchmem`) measured 1.74 µs,
1,702 allocated bytes and 9 allocations per heartbeat on darwin/arm64 (Apple
M4, two Go processors). This measures in-process decode/store cost for an
already enrolled identity, not network, daemon sampling or whole-fleet cost;
it is a reproducible reference measurement, not a cross-platform guarantee.

The fixture sends records through the production `DiagnosticExporter`, including
its transport schema attribute and authenticated headers. The fleet receiver
intentionally discards the separate `goobers.service.health` six-hour operational
stream after authentication and request/record/scalar bounds checks. This does
not refresh fleet liveness or persist its payload; a company collector can route
that stream to its own operational log store instead. Unknown event names still
reject. Filtered service-health records count as delivered, not transport losses.

Freshness recognizes stable SemVer and `alpha.N`, `beta.N`, or `rc.N` prereleases.
Development, nightly, arbitrary prerelease suffixes and local build metadata stay
unknown, including when an operator pins them. Approved prereleases remain
comparable through the explicitly supplied channel catalogue. Feature records
within allowed future clock skew are retained but remain unknown until their
observation time; the skew allowance never makes future usage already true.

### Required MCP conditions

The heartbeat's optional `requiredMcp` report is independent of work health. An
`active` condition means a retained required-tool failure has no newer verified
recovery for the same workflow, stage, parallel branch, adapter and server. It
does not mean every workflow in the gaggle is blocked. The report includes the
selected context, coded reason, decisive evidence timestamp, and coverage.

Availability and authorization have separate evidence clocks. A later
unsupported authorization check cannot clear a prior denial; a newer verified
recovery in another run can. Equal-time contradictory evidence retains failure
because cross-run ordering is unknown. At most 256 contexts across the bounded
run list are folded; missing, truncated, future or uninstrumented evidence stays
partial/unknown. Zero unresolved contexts is reported only with complete
coverage. Missing heartbeats also make the current MCP report unknown.

For example, a company can select unresolved tool conditions with
`select(.requiredMcp.state == "active")`, then route using `ownerRoute` and the
reported workflow/stage. `transitions` records changes under
`condition: "required_mcp"`, including recovery and loss of observation, within
the same bounded history as work-health transitions. This is a query/routing
example, not automatic outreach. Offline support bundles retain the coded
condition and context without raw MCP errors or responses.
## Observed feature windows

Feature use is separate from configuration. The daemon reads configured labels
from the current admitted definition snapshot on each sample; rejected edits do
not replace it. Effective runner configuration is supplied by daemon startup
wiring. Configuring a feature never increments its use counter.

The version-one catalogue adds `capability.nested-agents`. Counts describe:

- `runner.local` / `runner.engine`: actual `run.started` records, classified by
  the pinned journal driver.
- `dsl.v1` / `dsl.v2` / `dsl.v3`: those starts' digest-verified pinned workflow
  language versions (`1.4`, `2.0`, `3.0`). Definition revision numbers are unused.
- `adapter.*`: calls that reached the actual adapter dispatch boundary and
  returned, including failed calls. Executor preflight/configuration is excluded.
- `provider.*`: actual Goobers provider HTTP-client attempts observed inside a
  deterministic stage. Requests, URLs, credentials and bodies are excluded.
  Counts are journaled when the stage returns; interrupted stages, contended
  sidecars, daemon polling and arbitrary external clients may be unobserved.
- `capability.nested-agents`: recorded child-agent starts with explicit parent
  identity, deduplicated by stage/agent/attempt inside each run.

Counters are absolute observations in a window of at least five minutes,
clipped to the current daemon boot. For large fleets the window extends to one
full sampling rotation plus one heartbeat (at most 14 hours at the largest
allowed heartbeat interval). Re-reading the same journal does not add another count.
New windows can have lower counts; a restart starts a new boot/window. Journal
sequence checks and the remote journal plane's operation keys prevent replay
from silently adding usage. Child identifiers remain local to the scan.

Each scan covers at most 128 run directories, 8 MiB including metadata and
pinned definitions, 1 MiB per journal/definition and 64 KiB per event. At most
4,096 child identities are retained per run. A stage's provider counter sidecar
is at most 1 KiB, with three closed keys capped at one million attempts each;
its cross-process file lock never waits. Missing, unreadable, oversized,
truncated, future-clock or legacy evidence yields partial/unknown coverage.
Provider counts always remain partial because the stage boundary cannot prove
coverage of every provider client. Active or recently active runs likewise
prevent complete adapter/capability-zero claims. A completely inspected idle
window can report zero for run/DSL/adapter/child activity; a missing directory
cannot.

The sampler rotates through at most eight of 100 gaggles each heartbeat. At the
default 30-second interval, a full round of 100 gaggles takes 13 ticks (6 minutes
30 seconds). Only sampled gaggles emit feature records (at most 96 per tick); the backend
expires older feature counts to unknown between visits. The default window for
100 gaggles is seven minutes, overlapping successive rotations. Read-budget or
source gaps still remain partial; these bounded observations are not a billing
or audit ledger. Unknown configuration omits the feature record.

A local Apple M4/darwin-arm64 benchmark with `GOMAXPROCS=2`, 100 iterations,
measured an empty scan at 31.4 microseconds / 2,320 allocated bytes and 16 small
journal scans at 1.30 milliseconds / 453,328 allocated bytes. This measures the
reader only, not daemon/network overhead or other operating systems. Portable
subprocess and remote journal-plane regressions exercise the real provider and
recorder paths; CI remains the cross-platform gate.

Before adopting the bounded snapshot below, the same machine's real local
journal append benchmark (including existing
fsync and tail accounting, ten iterations) measured two records at 0.193 ms /
1,005 journal bytes per batch and 197 records at 94.2 ms / 99,550 journal bytes.
The latter allocated 85.7 MB per batch in the existing append path. Sustaining
that synthetic loaded batch every 30 seconds produces about 287 MB/day before
retention or compaction; queue batching does not remove local journal cost.
These historical measurements motivated replacing that frequent journal append
path with the bounded snapshot; they do not describe the current fleet writer.

## Pending issue observations

The independent `backlog` condition consumes the scheduler's actual bounded
issue-counter polls. Health sampling never queries a provider or acquires a
claim. A complete first/only page can establish an empty matching backlog.
Positive partial pages provide a lower bound; continuation pages, failures,
and unknown counters never establish zero. Overlapping workflow selectors use
the maximum observed page count, avoiding duplicate issue counts across workflows.

Five scalar fields carry pending count, source time, coverage, state and reason.
`attention / pending_without_confirmed_progress` means continuously observed
matching issue work has outlasted the configured progress period without
confirmed useful progress. It is independent of the main gaggle state and does
not prove claim availability or a stuck worker. The existing last-useful-progress
timestamp supplies context. Pauses, accepted definition changes, complete empty
pages, partial coverage, stale evidence and observation gaps reset pending age.
The source expires after 60 seconds, even if daemon heartbeats remain live.
Sparse sampling therefore remains conservative and cannot bridge unobserved gaps.

This counter does not apply the full stage claim transaction's blocked-item,
local-lease and shared-lease policy. Positive work therefore remains explicitly
claimability-unknown. A reusable bounded read-only claimability adapter is tracked in
[#5489](https://github.com/Agent-Clubhouse/Goobers/issues/5489) for v0.6.0; this release does not assert definitive issue-work stalls from the
provider label/field selector alone. No issue identifiers, titles or URLs are
retained or exported by this observation.

### Export loss evidence

The deployment heartbeat includes `diagnosticsDroppedRecords` only when its
diagnostic exporter exists. This is a cumulative record count for the daemon
boot, sampled before the current heartbeat is queued; it includes queue, size,
shutdown and collector-rejection losses observed so far. It is historical, not
a claim that the collector is currently reachable. A recovery heartbeat can
carry losses accumulated during an outage. Disabled or uninitialized export
omits the count rather than reporting zero. Gaggle records do not duplicate
the deployment counter. Offline bundles preserve the last observed count and
its boot/time context; an abrupt death can lose evidence after that observation.
## Actual execution deadlines

A quiet running stage can report `waiting / stage_within_deadline` only when
its current stage/branch/attempt has actual runtime deadline evidence. The
harness owned-process launcher and deterministic executor record their bounded
context deadline after a successful process launch, then clear it on return.
Copilot's controlled SDK session records the existing whole-session context
bound and clears it after session/process cleanup; nested process observations
are suppressed under that same bound.

The evidence follows the existing local or remote journal recorder and the
current active-stage projection. Stage configuration plus start time is never
used to synthesize a deadline. Retry, resume, terminal events, old attempt
records, future observation clocks, truncated active-stage inventory, missing
coverage and overlapping process-only observations cannot establish a bound.
Serial recovery calls establish their own actual bounds only after the previous
execution ends. The fleet classifier uses the earliest deadline only when all
active stages of all in-flight runs are covered; a valid quiet sibling never
hides an uncovered one. Deadline evidence is intentional waiting, not useful
progress or a proof of the eventual success of a process.

Local parallel journals stamp the actual branch ordinal and can retain these
bounds. The current remote pod invocation contract has no authoritative journal
branch ordinal; its unscoped observations cannot cover nonzero parallel branches.
Those remote parallel deadlines remain unknown. A Git workspace branch is not
used as a substitute for workflow branch identity.

### Observed retry backoff

A daemon can classify a gaggle as `backoff` while every nonterminal run is
in an observed retry timer and no active stage or human gate remains. This
is based on `retry-backoff.v1` journal annotations emitted at the actual
local or engine wait decision, with the failed stage, attempt, retry class,
observation time and scheduled deadline. It is never inferred from a retry
count. Parallel branch timers remain visible in run status, but do not establish
whole-gaggle backoff: an unobserved sibling may be between stages or awaiting
admission. Once a run has parallel-branch evidence, this conservative coverage
limit remains for that run, including after resume. The earliest root-stage timer deadline ends the classification. Expired, missing
or truncated evidence does not establish either continued backoff or progress.

The bounded run projection retains at most 64 stage/branch timers and clears
them on the next attempt, terminal event or explicit resume. A daemon restart
disregards old local process timers; local crash resume durably clears them.
Engine timers survive worker/daemon restart through Temporal history. The
engine records the annotation without adding a publication activity or changing
the scheduled sleep, so existing journal delivery/repair may expose the event
only after the wait. Live health remains unknown about that retry until evidence
arrives. Older engine histories replay without the new annotation. Retry budgets
and cancellation behavior are unchanged.
## Bounded local diagnostic history

Frequent fleet and feature observations live in the private
`scheduler/diagnostics/history.json` snapshot, separate from the durable scheduler
journal and its configurable retention. Each pulse replaces at most 4 MiB and
4,096 validated records atomically. One fixed scratch file is also limited to
4 MiB: data-file usage peaks at 8 MiB, plus an empty writer-lock file. A record
is at most 64 KiB and a pulse at most 256 records. Writer-lock acquisition never
waits; cancellation is checked before writes, synchronization and replacement.
OS filesystem flush calls themselves still depend on the filesystem completing.

The writer scrubs a batch before persistence and synchronizes once per pulse.
Oldest observations are evicted to satisfy both limits. Startup reads are
bounded, clean only the fixed scratch, and mark a reset after corrupt or
oversized prior evidence. Failed replacement preserves the prior snapshot;
known local write failures and invalid/oversized record omissions are reported
in the next successful snapshot. A failed filesystem cannot guarantee that its
last failure was durably recorded. Sparse, reset, expired or missing history is
never evidence of healthy inactivity or lossless delivery.

The offline bundle reads this retained window without contacting a daemon and
falls back to the bounded legacy scheduler tail when the dedicated store is
unavailable. Its public export-loss history carries existing per-boot
`diagnosticsDroppedRecords` observations, separately from local eviction and
write-failure metadata. It omits owner routing details and never treats zero
recorded drops as proof that a collector received every observation. The
six-hour service-health record remains in the existing scheduler journal.

A full snapshot can rewrite up to 4 MiB per pulse; the bounds limit disk
occupancy, not lifetime write volume. The `BenchmarkBoundedHistoryPulse`
benchmark exercises the real atomic replace and fsync path for a 197-record
pulse. Cross-platform CI validates the implementation; no local benchmark was
run while the shared build disk was critically full.

The normal `TestHistoryPulseMeasurement` also measures replacement with a full
4,096-record retained window. A [hosted Linux coverage run](https://github.com/Agent-Clubhouse/Goobers/actions/runs/35557571839/job/106204661982)
measured 29.94 ms and 1,777,807 snapshot bytes for one incoming record, and
44.80 ms and 1,794,193 bytes for 197 incoming records. Each pulse made one file
sync call and one directory sync call. These are coverage-instrumented test
measurements, not throughput guarantees or evidence of a physical directory
flush on Windows, where that sync operation is a no-op. The Windows privacy
implementation was subsequently corrected; complete CI on the final commit
remains required.

A [native Windows run](https://github.com/Agent-Clubhouse/Goobers/actions/runs/35558360961/job/106207104280)
with the corrected privacy implementation measured 52.09 ms and 1,761,421
snapshot bytes for one incoming record, and 62.56 ms and 1,761,423 bytes for
197 incoming records, again retaining 4,096 records. Its ACL repair and unsafe
alias regressions passed. Fixture timestamps vary between runs;
these measurements do not establish a Linux-versus-Windows performance ratio.
That run was superseded by the portal API correction, so its successful history
tests do not replace complete validation of the final commit.

On Windows, the dedicated history directory has a protected DACL granting the
current user, SYSTEM, and Administrators access, with inheritance for newly
created files. Existing snapshot and lock ACLs are repaired explicitly; each
scratch file is verified private before its first payload write. Repair uses
validated, single-link, non-reparse handles and does not recursively change
existing children or anything outside the dedicated directory. Native Windows
regressions exercise permissive parent ACLs, protected child ACL repair, private
scratch creation, and refusal of outside file/directory aliases.


## Downgrade and re-upgrade

Keep a stopped-instance backup and the last working configuration tree before
upgrading. The compatibility regression targets v0.4.1; it does not establish
compatibility with arbitrary older releases. Follow the
[instance restore contract](instance-restore-contract.md) when copying journals,
SQLite databases and their sidecars. Never run two daemon versions against the
same instance root at once.

To downgrade, stop the daemon and workers, then restore configuration accepted
by the selected older release before starting its binary. v0.4.1 rejects the
new `telemetry.diagnostics` block, even when its exporter is disabled, and the
new `telemetry.otlp.enabled` field. Restore the complete old configuration tree:
new gaggle/workflow `spec.enabled` pause switches also cannot be assumed to work
on that release. Keep admission stopped until the restored configuration's
scheduling behavior has been checked.

Do not simply delete `telemetry.otlp.enabled: false` while retaining its endpoint:
that can turn journal export back on in the older release. For a deployment
that must not export, restore an old-compatible configuration with no journal
OTLP destination and remove ambient exporter settings from the service
environment. For a deployment that should export, restore its prior approved
journal destination and credentials. The independent diagnostic destination is
unavailable in v0.4.1; a company collector should show stale/missing observations
during the downgrade, rather than interpreting silence as health.

The diagnostic annotations use the existing journal format and the diagnostic
projection adds no read-model SQL migration. Older readers ignore unfamiliar
operator fields. An older writer can discard those fields from cached run rows,
so a downgrade does not preserve diagnostic projections losslessly. Keep the
canonical run journals and private `scheduler/diagnostics/history.json` snapshot.
The older release does not consume that snapshot; existing observations retain
their original timestamps and must not become fresh merely because a process
restarts.

On re-upgrade, stop the older processes and back up the instance again, then
restore the newer configuration and approved exporter credentials. To recover
cached diagnostic fields from retained journals, the existing offline command
`goobers telemetry stats --rebuild /path/to/instance` rebuilds both analytics and
the run read model. Require the `read model rebuilt:` completion line and no read-model rebuild
warning, as well as a successful command exit status, before treating recovery
as complete. This is a full rebuild: it cannot restore already-pruned journal
evidence. Lifetime first-success milestones are carried forward from a readable
prior analytics database, but other analytics without retained source evidence
cannot be reconstructed. The rebuild does not preserve read-model retention
floors/tombstones. Keep the backup and account for that retention behavior before
using this recovery operation. Do not delete the original databases as an
initial troubleshooting step.

After restarting the newer daemon, verify new heartbeat delivery and the
expected company/deployment identity. Retained history is bounded, and cached
fields without surviving source evidence remain unknown. Downtime does not
produce retroactive feature observations or proof of uninterrupted health.
