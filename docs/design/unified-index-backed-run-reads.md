# Unified index-backed run reads

> **Status: superseded — replaced by
> [`portal-read-architecture.md`](portal-read-architecture.md).** Its
> read-projection conclusion is carried forward; the parts it left open
> (ordering, the writer set, request budgeting, in-process isolation, topology,
> authorization, the hosted shape) are answered there, and several of its
> positions are revised on measured evidence — notably one store versus two,
> request-path reconciliation, and the absence of an authorization dimension in
> the schema.
>
> **The child issues filed from this doc (#1888–#1892) should be re-scoped
> against the superseding design before implementation**, not started as
> written: they assume the current single-connection, in-process-ingest,
> filesystem-polling shape.
>
> *Original status:* Design for #1883. Extends the read-path direction
> established by #1200 / PR #1270 and the portal architecture in
> [`dashboard.md`](dashboard.md#14-read-path-performance---lists-must-not-rescan-the-journal).
> It does not change the run journal, stage-result envelopes, or existing public
> query contracts.

## Decision

The run journal remains the authoritative record. The local SQLite rollup is
extended into the shared **run read projection**: additive, disposable, and
deterministically rebuildable from retained journals. List and aggregate reads
may trust a healthy projection without reopening the selected journals. A
projection failure can make a read stale or unavailable, but cannot change a
run, manufacture history, or become the only copy of data.

This is one projection lifecycle and query vocabulary, not one response shape.
Overview, Workflows, Runs, and Insight read bounded list or aggregate views from
it. Run detail reads its header from the same projection but continues to read
the durable event ledger for the one selected run.

The current `telemetry.db` is the physical starting point. A second database
would duplicate ingestion, reconciliation, retention, and rebuild failure modes
without creating a new authority. The implementation may rename or split
tables behind `internal/readservice`, but callers must see one read model.

The core run projection is required for daemon read performance even when
`telemetry.enabled` is false. That setting continues to disable span capture,
usage/error analytics, and external telemetry; it must not select between a
bounded portal and the journal-scanning fallback. The core journal-derived
tables contain no data that is not already retained in the run journal.
Offline readers without an instance database may retain the existing scanning
fallback because they do not serve bursty portal traffic.

Usage-derived population facts are not part of that unconditional core.
Token-, premium-, cost-, and retry-waste filters remain available only when
telemetry collection is enabled and preserve the existing
`ErrTelemetryUnavailable` behavior otherwise.

## Current paths and cost

Let:

- `H` be retained run history;
- `W` be provisioned workflows;
- `P` be a requested page size; and
- `E(r)` be the event count in one run.

| Surface | Current path | Work that grows |
|---|---|---|
| Overview | `useOperationalOverview` makes five capped `listRuns` calls, one per phase. `ListRuns` selects candidates in SQLite, then opens and parses each selected journal. Its request-time reconciliation periodically materializes every indexed run ID before applying directory mtime watermarks. Instance, gaggle, and per-gaggle workflow inventory reads also invoke the journal-walking active-run count from #1741. | Normally five bounded pages, `O(P)` journal hydrations per phase; periodically `O(H)` ID reconciliation. Inventory can additionally pay multiple `O(H)` active-count scans per load. |
| Workflows | `useOperationalSnapshot` pages all inventory and makes one capped `listRuns({limit: 5})` call per workflow to find its latest terminal outcome. Instance, gaggle, and workflow inventory routes invoke #1741's full-history active count. Every matching run SSE invalidation reloads the whole snapshot. | `O(W)` HTTP requests and up to `O(5W)` journal hydrations, multiple `O(H)` active-count scans, plus periodic `O(H)` reconciliation per refresh. This combines total-history work with current-working-set fan-out and SSE amplification. |
| Run detail | `loadRunDetail` concurrently calls `getRun` and `listRunEvents`; each calls `openRun` and parses the complete journal. Selecting a stage can parse it again for attempts. | `O(E(r))` for one run, currently paid at least twice on initial load. It does not scale with `H`. Matching SSE events can repeat that work. |
| Insight | The page makes a fixed number of telemetry calls. SQLite groups `runs`, `stage_attempts`, usage, and error rows for the selected time window; it does not open run directories or journals. | Work grows with indexed rows in the selected window (and with all history for "all time"), not journal bytes or HTTP requests per workflow. |

Insight is fast under the reported load because it asks an aggregate question
inside SQLite in a constant number of round trips. It is not proof that every
query is constant-time: an all-time aggregate can still scan all matching index
rows. The reusable pattern is server-side indexed projection and aggregation,
not the particular Insight endpoint.

PR #1270 was the correct first step, but its index only chooses candidate run
IDs. Hydrating every result from the journal and reconciling on the request path
means it is not yet a complete indexed read path.

## Indexed read model

The logical projection has three parts. Existing rollup tables may satisfy a
field when they already carry equivalent data.

### Run summary

One row per run contains every field required by `RunSummary` and list filters:

- immutable identity: run ID, gaggle, workflow name/version/digest, trigger, and
  start time;
- mutable execution state: phase, terminal flag, current stage, finish and last
  activity times, last projected sequence, and retry/repass counts;
- terminal business outcome fields already derived for run detail; and
- semantic work disposition (`produced`, `no-work`, or `unknown`) once #1429
  defines that contract.

Duration is computed at query time from projected start/finish times. It is not
stored as a frozen value for a running run, so a quiet in-flight run continues
to age exactly as it does in the journal-derived path.

The base ordering key is `(started_at DESC, run_id ASC)`. Secondary indexes
begin with the common equality dimensions before that key, including
`(gaggle, workflow, started_at, run_id)` and phase/outcome/disposition variants.
The exact physical indexes are chosen from query-plan and scale evidence rather
than by creating every possible permutation.

### Run-stage facts

One row per `(run_id, stage)` records stage presence and filter facts:

- whether attempts exist and whether a completed measurement exists;
- latest/terminal attempt outcome facts needed by the existing stage-scoped
  outcome vocabulary; and
- when telemetry is enabled, token-, premium-, cost-, and
  retry-waste-population flags derived from `stage_attempts` and usage rows.

Stage and population queries use indexed `EXISTS`/joins against these rows.
They never open candidate journals or issue one telemetry query per candidate.
Stage-oriented indexes include stage/filter dimensions plus the run ordering
key where benchmarks show that avoids a sort over history.

### Projection state

Projection metadata records:

- schema and projection generation;
- each run's last projected journal sequence and source fingerprint;
- per-run-root reconciliation watermark and resumable scan cursor; and
- rebuild/backfill state and the last error.

This metadata replaces request-time `IndexedRunIDs()` materialization. It also
makes lag and incomplete backfills explicit rather than treating an absent row
as proof that a run does not exist.

Daily aggregate buckets may continue to accelerate Insight windows and
all-time totals, but they are downstream of the same run and stage facts.
Metrics and lists therefore share filter meanings, freshness, retention, and
rebuild behavior even though aggregate queries return a different shape.

## Query behavior

All current run-list dimensions must be pushed into SQLite:

| Query | Projection behavior |
|---|---|
| Recency | Seek on `(started_at, run_id)` and fetch `limit + 1`. |
| Workflow | Equality on gaggle/workflow followed by the recency seek. |
| Gaggle | Equality on gaggle followed by the recency seek. |
| Stage | Indexed join/`EXISTS` through run-stage facts before applying the seek. |
| Outcome | Run phase/outcome columns, or stage outcome facts when stage-scoped. |
| Population | Run-stage measurement flags; no per-candidate `StageAttempts` query. |

Pages retain the existing newest-first order and opaque keyset cursor. Cursors
encode at least the ordering key, projection schema version, and a hash of the
normalized filters so they cannot be reused with a different query. They never
use an offset. The existing default and maximum limits remain 50 and 200, and a
query reads at most `limit + 1` result rows after indexed filtering.

No compatibility fallback may hydrate an unbounded candidate set. If a current
public filter cannot be expressed by the projection, implementation must add
the required fact/index before routing that filter to the projection. During a
projection outage the service returns its existing read-error shape rather
than silently switching to an `O(H)` journal scan.

The Workflows page uses one additive read-service aggregate that returns the
latest terminal run for each workflow in a requested gaggle/current inventory.
Its result size may grow with `W`, because the page renders `W` workflows, but
the HTTP and SQL query counts do not. Existing run-list routes remain valid;
this is an additive consumer optimized for a different response shape.

Run detail uses the projected row for its summary and freshness token. Graph,
escalation evidence, artifacts, and event-ledger data still come from the
selected journal. A shared single-run loader keyed by `(run_id, source
fingerprint)` prevents `getRun`, `listRunEvents`, and stage-attempt reads from
independently reparsing unchanged bytes. Event pagination or tail reads, if
live evidence confirms they are necessary, are additive behavior under #1665;
the complete existing event route remains compatible.

## Update and reconciliation

### Normal writes

1. Append and fsync the authoritative journal record.
2. Queue an idempotent projection transaction for that run.
3. Project only the unprocessed event tail, or reproject that one run when a
   schema requires it. Commit all run summary, stage facts, aggregate deltas,
   and `last_seq` updates atomically.
4. Publish the scoped run invalidation after the projection commit.

Run creation, every query-visible state transition, and run completion follow
this order. Duplicate delivery is harmless because `last_seq` and per-run
replacement are idempotent. Low-value events may share a short batching window,
but the freshness target below still applies.

Projection failure does not rewrite or fail an already durable run. It marks
the projection unhealthy and leaves the run queued for retry. Reads whose
completeness cannot be established fail explicitly; they do not omit the dirty
run or scan all journals. The health/read-service layer exposes projection
freshness additively when that implementation lands.

### Reconciliation

Reconciliation is a background repair path, never part of an HTTP request:

- The daemon persists an mtime/fingerprint per run root. An unchanged root
  needs no directory walk after restart.
- A changed root is scanned in bounded, resumable batches. Candidate run IDs
  are checked by indexed lookup; the reconciler does not load all indexed IDs
  into memory.
- New recorded run directories discovered under a changed root are queued for
  per-run projection.
  Unpublished directories without `run.yaml` remain ignored. Retention deletes
  the journal and projection row through the existing coordinated path.
- Cancellation or restart resumes from the last committed batch. A later pass
  checks the root again so additions concurrent with a scan are not lost.

A flat run directory necessarily costs `O(H)` to inspect after an out-of-band
change while the daemon was stopped. That exceptional scan is moved off the
request path, checkpointed, and never multiplied by page or SSE request count.
Parent-directory mtimes detect entries added or removed, not in-place edits to
an existing child's files. Normal appends are covered by the write-through
updater; an operator who modifies an existing retained journal out of band uses
the explicit rebuild, which verifies every per-run source fingerprint.

### Rebuild and backfill

Missing, incompatible, or operator-requested projections rebuild from retained
journals in deterministic run order. Rebuild writes a shadow generation in
bounded transactions while the last healthy generation remains readable.
Journal updates committed during rebuild are replayed by per-run sequence
before the new generation is atomically selected. A crash discards or resumes
the shadow generation; it cannot partially publish it.

Schema migration may backfill an additive field in place when old rows remain
valid. Otherwise it uses the same shadow rebuild. `goobers telemetry
--rebuild` remains the operator entry point; rebuilding the run read projection
must not require a new source or alter retained journals.

## Invalidation and burst behavior

The projection commit produces one invalidation carrying run ID, gaggle,
workflow, affected read models, and projection generation. Client cache
dependencies use that scope:

- unrelated run events do not refresh a gaggle, workflow, or run-detail view;
- a run-only invalidation does not repage definition inventory;
- burst coalescing permits at most one active refresh and one pending refresh
  per query family; and
- a newer event does not abort an otherwise useful in-flight request.

The generation lets a client discard an older response that completes after a
newer projection. It is not a replacement for fixing each hook's subscription
scope. SSE transports invalidation; it does not prescribe a full page reload.

## No-work runs

No-work is an indexed semantic dimension, not a duration heuristic and not only
a client-side filter. Baking the eventual #1429 disposition into run summary
rows lets lists and metrics exclude it before pagination/aggregation, which is
required for scaling and consistent denominators. `unknown` remains distinct
and included unless the caller explicitly requests otherwise.

#1439 still owns the user control, URL state, denominator disclosure, excluded
count, and separate no-work wakeup metrics. This design only ensures that its
classification can be applied consistently and cheaply. Journals, spans,
telemetry retention, and raw counts remain unchanged.

## Boundaries with existing issues

| Issue | Shared model contribution | Work that remains in that issue |
|---|---|---|
| #1712 | One latest-outcome aggregate removes the per-workflow run-list fan-out. Scoped projection invalidations provide the correct dependency data. | Honor invalidation models/page scope and stop reloading inventory on run-only events. |
| #1882 | None; configuration warnings are not run-index data. | Fix that hook's abort/restart behavior and burst test independently. |
| #1439 | Stores and pushes down the semantic disposition once defined. | Define/consume #1429, UX, URL state, denominators, excluded counts, and no-work metrics. |
| #1782 | Stage, outcome, and population filters become fully index-pushable; every page has a hard row bound. | Land the immediate candidate-scan safety and filter parity work without waiting for the whole projection migration. |
| #1665 | Projected headers and a shared per-run loader remove avoidable duplicate parsing. | Confirm journal-size versus SSE-churn mechanism, then add scoped coalescing and/or additive event paging as evidence requires. |
| #1741 | The indexed running phase supplies active counts grouped by gaggle/workflow. | Move daemon inventory and scheduler consumers off the historical journal walk while preserving the offline fallback. |

The shared model solves total-history scans, journal hydration for lists,
per-workflow query fan-out, and inconsistent filter execution. It does not make
all SSE hooks correct, define no-work semantics, or eliminate the intrinsic
cost of displaying one very large event ledger.

## Scale and conformance criteria

The complete program (projection slices plus the separately owned hook fixes
mapped above) is complete only when automated fixtures demonstrate:

1. **Bounded lists:** with 100,000 runs and at least 1,000,000 stage-attempt
   rows, first and next pages for recency, gaggle, workflow, stage, run/stage
   outcome, and every population filter read at most `limit + 1` result rows,
   open zero journals, and read zero run directories. Query-plan assertions
   verify an intended index rather than a full table scan/sort.
2. **Flat page latency:** on the repository's reference benchmark host, warm
   p95 latency for a 50-row page is at most 250 ms and grows by no more than 2x
   when retained history grows from 10,000 to 100,000 runs. The benchmark
   reports hardware and cold-cache results rather than treating them as the
   same target.
3. **Bounded workflow loading:** a 2,000-workflow page obtains latest outcomes
   with one aggregate request and no per-workflow run requests. Response work
   is `O(W)` rows rendered, not `O(W)` network/SQLite queries.
4. **Bursty SSE:** at 100 relevant events/second for 60 seconds with 500 ms
   artificial response latency, projection invalidations are scoped and
   unrelated scopes trigger zero data refreshes. End-to-end portal assertions
   that each query family has at most one active and one pending refresh,
   completes useful responses, and performs no abort solely because a newer
   event arrived are completion criteria for #1712, #1882, and the SSE branch
   of #1665, not blockers for the projection slices alone.
5. **Freshness and recovery:** a query-visible journal append is reflected in
   the projection and its scoped SSE generation within one second in steady
   state. Killing projection update or rebuild at every transaction boundary
   either preserves the previous generation or resumes idempotently; no
   partial generation is readable.
6. **Rebuild equivalence:** rebuilding from the same retained journals produces
   identical canonical projection rows. For every existing filter and cursor
   fixture, projected `RunSummary` pages are byte-equivalent to the
   journal-derived reference path under the same injected observation time.
7. **Single-run isolation:** run detail work is independent of `H`. Initial
   detail loading opens only the selected run and parses an unchanged journal
   at most once per source fingerprint; large-ledger paging targets, if needed,
   are set by #1665 after measurement.

## Follow-up backlog

Each filed issue links both #1197 and #1883, states its dependencies and
non-goals, and carries independently testable acceptance criteria:

| Slice | Issue | Depends on | Boundary |
|---|---|---|---|
| Projection schema and query primitives | [#1888](https://github.com/Agent-Clubhouse/Goobers/issues/1888) | - | Complete summary/stage facts, indexed filters, direct row projection, and parity/query-plan tests. |
| Write-through updater and freshness | [#1889](https://github.com/Agent-Clubhouse/Goobers/issues/1889) | #1888 | Idempotent per-run projection before scoped SSE publication, with explicit health/lag and no full-scan fallback. |
| Background reconcile and shadow rebuild | [#1890](https://github.com/Agent-Clubhouse/Goobers/issues/1890) | #1888, #1889 | Bounded resumable repair outside HTTP reads and atomic shadow-generation rebuild/backfill. |
| Workflow latest-outcome aggregate | [#1891](https://github.com/Agent-Clubhouse/Goobers/issues/1891) | #1888, #1889 | One bounded aggregate for Workflows/Gaggle consumers; coordinates with but does not absorb #1712. |
| Scale and conformance harness | [#1892](https://github.com/Agent-Clubhouse/Goobers/issues/1892) | #1888-#1891 | The 100k-run/filter, workflow fan-out, scoped-invalidation, crash, and rebuild-equivalence fixtures above. Component hook burst/coalescing remains with #1712, #1882, and #1665. |

#1782 and #1741 can land before slices 1-3 as immediate bounds. #1712, #1882,
#1439, and #1665 remain separately actionable at the boundaries in the table
above; they should not be folded into the projection implementation.

## Explicit non-goals

- No journal event or directory-layout change.
- No stage invocation/result envelope change.
- No breaking change to existing `/api/v1` query parameters, response fields,
  ordering, or cursor semantics; optimized aggregate routes and freshness
  fields are additive.
- No inference of no-work from duration or missing telemetry.
- No request-time full-history fallback presented as a successful indexed read.
