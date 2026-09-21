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
