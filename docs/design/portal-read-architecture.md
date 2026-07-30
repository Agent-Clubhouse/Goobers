# Portal read architecture — a rethink

> **Status:** Design proposal, third pass. Responds to
> `Goobers-Reviews/2026-07-29_portal-architecture-findings.md` (the diagnosis) and
> supersedes [`unified-index-backed-run-reads.md`](unified-index-backed-run-reads.md)
> (#1883), keeping its read-projection conclusion and replacing the parts it left
> open: ordering, the writer set, request budgeting, in-process isolation,
> topology, authorization, and the hosted shape.
>
> **Revision history is in §18**, including nine errors in this document's own
> first pass and seven correctness holes found in review of the second. The
> second pass over-simplified in two specific ways that review correctly
> identified as regressions: it removed the projection **epoch**, which is
> load-bearing because a rebuilt SQLite file resets `AUTOINCREMENT`; and it
> claimed one sequence could serve read-your-write, which it cannot, because the
> projector allocates that sequence *after* the mutation has already returned.
> Freshness must be **source-relative**. Both are restored here.

---

## 0. How to read this

§1 is the decision. §2 is measured evidence from the live instance — it changes
two of the diagnosis's conclusions and adds a defect the diagnosis did not have.
§3–§12 are the architecture. §13 is the cloud trajectory. §14 is the acceptance
bar. §15 answers the diagnosis's open questions. §16 is the harness. §17 is the
plan, with a risk and rollback posture per wave. §18 is revision history.

The shape of the plan: **Wave 1 is five small independent diffs targeting the
measured hot paths.** Waves 2–3 make it structural. Waves 4–6 make it
extensible. Wave 7 changes deployment topology and the store backend — not the
model, the contract, or the client.

---

## 1. The decision

Four changes, in dependency order.

1. **One read model, complete enough to answer without the journals, in its own
   small store.** Every list and aggregate is answered from indexed rows with
   **zero journal opens**. Run phase becomes a **stored, indexed fact**. The run
   read model lives in a new `read.db` (tens of MB), separate from the existing
   547 MB `telemetry.db`, so cold start is gated on 191 MB of run event logs
   rather than 2.5 GB including spans. Every list belongs to a declared **query
   family** with a covering index and **no residual predicate** — because
   `limit+1` returned rows does not bound rows *examined* (§5.7).

2. **Two explicit identities, not one overloaded number.**
   - **Source position** `(runID, journalSeq)` — what a writer commits and can
     return immediately. This is the read-your-write token and the basis of
     freshness, because only a source position can reveal an append the projector
     has not yet discovered.
   - **Projection position** `<schemaVersion>:<epoch>:<seq>` — the SSE cursor and
     ordering key, from a `change` row written in the same transaction as the
     fact it describes. The **epoch** is minted on every rebuild, because a new
     SQLite file resets `AUTOINCREMENT` and a client cursor would otherwise
     outrank the entire rebuilt store forever.

3. **One projector, one serialized commit loop, and reads never write.** The
   projector owns every write to projected facts, and change rows are committed
   by a **single serialized loop** — parallel workers on a shared sequence can
   commit out of order and strand a client past a lower uncommitted seq (§13.3).
   Discovery is a **durable per-run source watermark**, acknowledged inside the
   projection transaction; channels are wakeups only, never the correctness
   mechanism. Repair is a **rate bound** — a fixed I/O budget per second that
   always makes progress — because a bound that only holds when nothing is
   happening is not a bound.

4. **One read contract that states its own cost and its own freshness.** Every
   route declares a cost class and a server-side budget. Every response carries
   `readState`. A request that exceeds its budget returns a **fast 503** — an
   interrupted page is never dressed up as a successful `partial`, which would be
   silent omission in a new costume. `partial` is reserved for a *named,
   described* missing partition or stated lag.

Two framing notes:

- **"Zero timeouts" is a structural claim.** No request ends in an indefinite
  wait or a client-side abort, because every request has a server-enforced
  deadline, a declared bounded query plan, and a response shape that can express
  "stale by N" or "unavailable because X." It does not mean every question is
  O(1).
- **This is a net deletion.** §12 lists roughly 1,500 lines of accumulated
  mitigation this removes rather than reorganizes.

---

## 2. Measured baseline

Measured on the live self-hosting instance (`~/source/goobers-instances`) on
2026-07-29, against `main` at `899dbbdd`. **The daemon was not running and
`up.lock` was stale**, so every number is a *best case* with zero contention from
live execution.

| Fact | Measured |
|---|---|
| Run directories | **40,665** |
| …of which published (have `run.yaml`) | **29,759** |
| …of which unpublished (no `run.yaml`) | **10,906** (27%) |
| Directories containing a `.lock` file | **40,665** — *including all 10,906 unpublished* |
| Total run-journal `events.jsonl` | **191 MB** across 29,759 files |
| Run `events.jsonl` size: mean / p50 / p90 / p99 / **max** | 6.7 KB / 6.0 KB / 13.5 KB / 39.8 KB / **131 KB** |
| Span files | **2,263 MB** across 70,425 files |
| Instance journal `scheduler/events.jsonl` | **324 MB**, 156,765 records |
| `telemetry.db` | **547 MB** |
| Total under `gaggles/` | 5.1 GB (4.4 GB runs, 682 MB workcopies) |
| **`ActiveRunCountsByWorkflowDirs`** — "how many runs are live" | **17.2 s cold / 4.3 s warm** to return the answer **2** |
| **`journal.ReadInstanceLog`** — what one instance-journal append re-reads | **1.30 s** warm, ×3 consistent |

### 2.1 The active-run count is a confirmed hot path

17.2 s cold to count two live runs, on an idle host, against a client that aborts
at 10 s. `/v1/instance`, `/v1/gaggles`, and `/v1/gaggles/{g}/workflows` each
invoke it (`internal/readservice/inventory.go:332,380,512`), and
`ListRuns(LatestPerWorkflow)` invokes it again (`runs.go:366`). There is no cache
(`inventory.go:601`).

The configuration-warnings widget's only data source is `getInstance()`
(`portal/src/configurationWarnings.ts`), which is this call — so on a busy
instance the widget cannot populate. That is a read-model problem, not a widget
bug.

### 2.2 New defect, not in the diagnosis: every instance-journal append re-reads the whole journal

`journal.InstanceLog.Append` (`internal/journal/instance.go:81`) takes the
journal lock, then calls `readEvents` over the *entire*
`scheduler/events.jsonl` — **1.30 s per append at the current 324 MB**, growing
without bound. The scheduler calls it from 13 sites
(`internal/localscheduler/scheduler.go`) plus the claim ledger
(`internal/localscheduler/claim.go:554`).

This is a **write**-path cost that competes with the **read** path in the same
process, and it is a strong candidate explanation for the diagnosis's "critical
observation" — that the same page is fast one hour and unusable the next. The
scale harness's own comment noticed the shape (`test/scale/main.go`:
"`journal.OpenInstanceLog.Append` re-reads the whole log per append (O(n²))") but
only as an obstacle to *generating* fixtures. There is no open issue for it.

**Important constraint on fixing it, which the second pass got wrong:** that
reread is not redundant. It happens under a cross-process journal lock and *is*
the sequence-allocation mechanism for independent writers. See §17 Wave 1.1.

### 2.3 Run detail is not slow because of large event ledgers

The diagnosis (§9.8) and #1665 leave the mechanism unconfirmed and ask for
measurement. Measured: the **largest** run event log in 40,665 runs is **131 KB**;
p99 is 40 KB. Parsing 131 KB of JSONL three times is single-digit milliseconds.

**What this establishes:** "one very large event ledger" is ruled out as the
direct explanation for a 10-second request. Pagination of the event ledger is
therefore not the answer, and redundant parsing — real (`GetRun` and `RunEvents`
each call `openRun`; stage selection can call it a third time) — is worth fixing
on its own merits but cannot produce a 10-second timeout.

**What this does not establish.** The leading hypothesis is head-of-line blocking:
a single SQLite connection (`SetMaxOpenConns(1)`, `rollup/db.go:61`) shared by
every reader and writer, plus 1.3 s locked journal appends, plus 4–17 s active
count scans, plus a 100 ms filesystem-polling ticker. That is an inference from
measured component costs, **not itself a measurement**, and review notes
production evidence of other mechanisms — refresh/cancellation churn, and one
dashboard failure showing proxy cancellations while direct daemon health,
inventory, run, and SSE endpoints were healthy, which points outside the read
model entirely. Treat head-of-line blocking as **the leading measured
hypothesis**, to be validated or refuted by §16's mixed-load harness before it is
asserted as the cause.

### 2.4 Physical evidence that reads perform maintenance

Every one of the 40,665 run directories contains a `.lock` file — including all
10,906 that have no `run.yaml` and can never be ingested. Those locks were
created by `IngestRun` → `journal.WithPruneProtection` → `acquireJournalLock`
(`internal/journal/prune.go:51`), called from `reconcileIndex` **on the HTTP list
path** (`readservice/runs.go:921`), before failing to read an identity that does
not exist.

### 2.5 The scale harness does not measure the failing surfaces

`test/scale/measure.go` builds the read service with `minimalDefinitions()` — **no
gaggles and no workflows** — so it never calls `Instance()`, `Gaggles()`,
`Workflows()`, or `LatestPerWorkflow`. It never measures run detail and never
applies concurrent load. §16 fixes it first, because without it we cannot tell a
fix from a coincidence — and because §2.3's hypothesis can only be settled there.

### 2.6 What these numbers imply for store design

- **List data is 191 MB; analytics data is 2,263 MB** — a 12× difference in
  rebuild cost between the data the product *is* and the data that decorates it.
- **The rebuild cost driver is file opens, not bytes: 29,759 of them.** On local
  disk that is seconds. On any network- or blob-backed mount it is dominated by
  per-open latency, and no defensible figure exists — §16.8 measures it.

---

## 3. Authority and layers

```
┌──────────────────────────────────────────────────────────────────────┐
│ VIEW      portal/src — three bounded query primitives, scoped cache, │
│           in-place list patching, readState rendering                │
└────────────────────────────────▲─────────────────────────────────────┘
                                 │ HTTP /v1 + SSE  (cost class + budget
                                 │                  + readState envelope)
┌────────────────────────────────┴─────────────────────────────────────┐
│ SERVE     internal/httpapi + internal/readservice                    │
│           admission control per cost class; readmodel.Reader only —   │
│           no writes, no directory walks, no locks, no repair          │
└────────────────────────────────▲─────────────────────────────────────┘
                                 │ read-only pool (WAL, N connections)
┌────────────────────────────────┴─────────────────────────────────────┐
│ PROJECT   internal/readmodel                                         │
│           read.db  — run, run_stage, change, run_intake, state, buckets│
│           telemetry.db — spans, usage, analytics (ATTACHed)           │
│           the projector: sole writer, single serialized commit loop   │
└────────────────────────────────▲─────────────────────────────────────┘
                                 │ source watermark (intake) + read (journals)
┌────────────────────────────────┴─────────────────────────────────────┐
│ RECORD    internal/journal — authoritative, per run                  │
└──────────────────────────────────────────────────────────────────────┘
```

### 3.1 Three capabilities, three interfaces, enforced by types

```go
// internal/readmodel

// Reader is what SERVE receives. No write methods exist on it.
type Reader interface { /* queries only */ }

// Intake is the doorbell. It records a durable per-run SOURCE WATERMARK —
// the highest journal seq the writer has committed — and nothing else.
// It cannot touch a projected fact.
type Intake interface { Observed(runID string, journalSeq uint64) error }

// Projector owns the only writable handle to projected facts, and commits
// change rows through a single serialized loop.
type Projector interface { /* project, repair, rebuild */ }
```

`readservice.Local` holds a `Reader`, so it is a compile error for a read path to
write, backfill, or repair.

### 3.2 Invariants

- **RECORD is authoritative.** Both stores can be deleted and rebuilt from
  journals. This is a tier-1/2 statement: at tier 3, if Temporal history projects
  *into* the journal format, the journal is itself a projection, and recovery
  plans must not assume otherwise (§13.4).
- **The projector is the only writer of projected facts**, and change rows commit
  in a single serialized order (§6.1).
- **SERVE performs no I/O outside the read model**, except `single-run`-class
  routes which open exactly one journal, once per source fingerprint.
- **VIEW issues no query whose count grows with entities rendered.**

---

## 4. Identity, ordering, and discovery

Three distinct concerns. The second pass collapsed them into one number; two of
the three cannot be served by it.

### 4.1 Source position — what a writer can promise

`(runID, journalSeq)`. A writer that appends and fsyncs a journal record knows
this immediately, before any projection exists. It is therefore:

- the **read-your-write token** (§7.4);
- the basis of **freshness**, because a projection-side sequence cannot reveal a
  source append the projector has not yet discovered — the projection would
  report zero lag while being arbitrarily behind.

### 4.2 Projection position — ordering and the cursor

```sql
CREATE TABLE change (
  seq      INTEGER PRIMARY KEY AUTOINCREMENT,
  at       TEXT NOT NULL,
  kind     TEXT NOT NULL,   -- run.created|run.progressed|run.finished
                            -- |run.removed|definitions.reloaded
  run_id   TEXT,
  gaggle   TEXT,
  workflow TEXT
);
CREATE INDEX idx_change_run ON change(run_id, seq);
```

One row per projected transition, in the same transaction as the `run` row it
describes, committed through the serialized loop (§6.1).

**The cursor is `<schemaVersion>:<epoch>:<seq>`.** The `epoch` is a monotonic
value in `projection_state`, minted on every rebuild.

The epoch is load-bearing, not bookkeeping. **A rebuilt `read.db` is a new SQLite
file, so `AUTOINCREMENT` restarts at 1.** Without an epoch, a client holding
cursor `918342` reconnects to a store whose maximum is `100`: the cursor is
neither below the retention floor nor from a different schema version, so no
named condition fires, the client waits forever, and §8.2's rule discarding
responses with a lower position makes that permanent. The second pass removed the
epoch as redundant; that was wrong.

An epoch mismatch is the named condition **`epoch_changed`**, which instructs a
snapshot refetch.

### 4.3 Discovery — a durable per-run source watermark

```sql
CREATE TABLE run_intake (
  run_id     TEXT PRIMARY KEY,
  source_seq INTEGER NOT NULL,   -- highest journal seq the writer has committed
  noticed_at TEXT NOT NULL
);
```

Every writer, in-process or not, upserts on **every query-visible transition**
(not only on creation):

```sql
INSERT INTO run_intake (run_id, source_seq, noticed_at) VALUES (?, ?, ?)
ON CONFLICT(run_id) DO UPDATE SET
  source_seq = MAX(source_seq, excluded.source_seq),
  noticed_at = excluded.noticed_at;
```

The projector reads the row, projects the journal up to `source_seq`, and **in the
same transaction as the projection**:

```sql
DELETE FROM run_intake WHERE run_id = ? AND source_seq <= ?;
```

The `<=` guard is the whole protocol. If a newer append raced the projection, the
row's `source_seq` is now higher, the delete is a no-op, the marker survives, and
the projector revisits. Acknowledging before the commit would lose discovery on a
crash; acknowledging after would race a newer notification. Doing it inside the
transaction with a watermark guard does neither.

**Channels are wakeups only.** The in-process runner still signals the projector
over a channel so steady-state latency is a channel send rather than a poll, but
a dropped or missed wakeup costs latency, never correctness — the durable
watermark is the mechanism.

**Retention uses the same protocol.** It records its intent through `Intake`; the
**projector** emits `run.removed` and drops the rows, preserving the sole-writer
invariant. Retention never writes a projected fact.

**Restart.** Two bounded passes, no scan: drain `run_intake`, then reproject rows
the read model records as non-terminal, since only those can have advanced.
That is **O(active + pending)**, typically tens of rows.

Repair (§6.3) is then a genuine backstop for restores, manual copies, and
migrations — not the completeness mechanism.

### 4.4 Why there is no separate durable feed file

The first pass introduced a segmented, append-only change-feed file with a
retention floor, torn-record rule, and a lock protocol. It is cut, because it
conflated §4.1–§4.3 and was **unnecessary where it worked** (one process, where
the projection's own commit sequence and a table suffice) and **unworkable where
it would have been needed** (writers on separate nodes, where multi-node atomic
append is not a primitive and a lock on shared storage is precisely the
coordination mechanism shared storage must not be).

Cutting it also resolves the cloud case rather than deferring it: at tier 3 the
read model is shared (§13.2), so `change` *is* the cross-replica ordered log,
cursors are portable because the sequence and epoch are shared, and `run_intake`
gives cross-process discovery. The only tier-3 addition is notification —
`LISTEN/NOTIFY`, or a 1 Hz `WHERE seq > cursor` poll. **No new component at
either tier.**

---

## 5. The read model

### 5.1 Two stores, ATTACHed

| Store | Contents | Size now | Rebuild input | Criticality |
|---|---|---|---|---|
| **`read.db`** (new) | `run`, `run_stage`, `change`, `run_intake`, `projection_state`, day buckets | tens of MB | **191 MB** (`run.yaml` + `events.jsonl`) | The product |
| **`telemetry.db`** (existing) | spans, span events, usage, agent invocations, curation, scheduler events | **547 MB** | **2,263 MB** (spans) | Analytics |

Both opened on one connection via SQLite `ATTACH`, so cross-store joins are
ordinary SQL — and at tier 3 the same queries run against one database with two
schemas.

What the split buys: cold start gated on 191 MB rather than 2.5 GB; two retention
policies, which they already need; a small enough `read.db` that rebuild is a
whole-store operation (§6.5); and separate blast radius.

### 5.2 Connection and durability

| Today | Change | Why |
|---|---|---|
| `SetMaxOpenConns(1)` (`rollup/db.go:61`) | Writer handle (`MaxOpenConns=1`, `_txlock=immediate`) owned by the projector; **read-only pool** (`mode=ro`, `MaxOpenConns=NumCPU`) | WAL is configured then discarded. Today every reader serializes behind every reader *and* the writer, so the Overview's five concurrent requests have their queries serialized and an analytics aggregate blocks every list |
| `synchronous` defaults to FULL in WAL | `synchronous(NORMAL)` | fsync per commit on derived, rebuildable data |
| `runs` has **no index** beyond its PK (`rollup/schema.go:11`) | Declared indexes per query family, asserted by plan tests (§5.7) | Today the "indexed" list path is a full scan plus sort, and `LatestWorkflowRunRefs` is an unindexed window function over all history (`query.go:278`) |
| Path fixed | `readModel.path` / `telemetry.path` | §13.1 |

### 5.3 Shape

**`run` — one row per run, complete.** Identity (run id, **gaggle**, workflow
name/version/digest, goober digest, trigger, `started_at`); execution state
(**`phase`** stored, `terminal`, `current_stage`, `finished_at`,
`last_activity_at`, `last_seq`, repass/retry/policy-retry/infra-retry counts);
terminal business outcome (verdict, target); and `disposition`
(`produced`/`no-work`/`unknown`), reserved so #1429's definition and #1439's
filter are index-pushable rather than a new scan.

`duration` is **not** stored for a running run — computed at query time, so a
quiet in-flight run ages as the journal-derived path makes it age.

**`run_stage` — one row per (run, stage)**, carrying journal-derived stage facts
*and* named boolean **facets** for the selective populations, each with a partial
index (§5.7). The first pass put population flags here and the second pass
removed them as an extensibility trap; §5.7 explains why the resolution is
neither.

**`projection_state`** — schema version; **epoch**; per-run
`last_projected_seq` and source fingerprint; the repair cursor and
last-completed-cycle time; the **projection floor** (§6.3); rebuild state and
last error. This replaces `IndexedRunIDs()` (`query.go:329`), which materializes
every run id into a Go map on the request path, and it is what lets a read
*establish* completeness rather than assume it.

### 5.4 Run phase becomes a stored fact

Today phase is reconstructed by replaying a run's whole event log
(`journal.Reader.Phase`, `internal/journal/reader.go:77`) — including in the path
that counts live runs across all history. Measured: **17.2 s to answer "2."**

Stored, that becomes one indexed aggregate over `phase = 'running'`.

The staleness objection is answered by: **source-relative** lag stated in every
response (§7.2 — not projection-relative, per §4.1); the removal of the
status-pushdown bug class, since the projector writes phase on every transition
and records `last_projected_seq`, making a lagging run *detectably* dirty rather
than silently miscategorised (today `status` is written only on finish
(`ingest.go:108`), backfill only touches *absent* runs (`runs.go:905`), and the
finish-time update is best-effort with a swallowed error (`runs.go:921`)); and
`reconstructPhase` surviving as a differential oracle (§14.7).

### 5.5 Authorization is a query predicate, never a post-filter

Once a list is one indexed query returning `limit+1` rows, scoping must happen
inside that query. **Post-filtering after `LIMIT` silently omits rows** — the
diagnosis's §5.6 failure. **Filtering before `LIMIT` without an index**
reintroduces the scan.

So `gaggle` leads the scoped ordering indexes, **and** a global recency index
exists for the unrestricted case:

```sql
CREATE INDEX idx_run_g_recency ON run(gaggle, started_at DESC, run_id);
CREATE INDEX idx_run_g_wf      ON run(gaggle, workflow, started_at DESC, run_id);
CREATE INDEX idx_run_recency   ON run(started_at DESC, run_id);  -- unrestricted fast path
```

Three scope cases, all bounded:

| Principal scope | Plan | Bound |
|---|---|---|
| Provably **unrestricted** (all gaggles) | `idx_run_recency`, no gaggle predicate | `limit+1` |
| **k gaggles, k ≤ `maxMergeFanout`** (default 16) | k keyset seeks on `idx_run_g_*`, merged | `k × (limit+1)` |
| **k > `maxMergeFanout`** and not all gaggles | **Rejected** with a typed "narrow the scope" error | — |

The third row is deliberate. The second pass asserted k is "small and known,"
which is false today (`AllowAll` means every principal sees everything — served
by row 1) and would be false again for a future organization admin over many
gaggles. An explicit bounded refusal is honest; a silent unbounded merge is not.

Tested rule: **every list query contains the authorization predicate, and its
plan matches its declared family (§5.7).** Holds under `AllowAll` today and when
#644's RBAC lands, without schema change.

### 5.6 Aggregate buckets: recompute, don't accumulate

Day buckets per `(gaggle, workflow, phase, outcome, disposition)` make even an
all-time window a bounded number of rows.

**Buckets are recomputed, not accumulated.** When a run in day *D* changes, mark
*D* dirty; the projector recomputes *D* by aggregating that day's indexed `run`
rows. Bounded by runs in a day, and **idempotent** — which serves determinism
(§14.9) for free. Reversible deltas would require storing and subtracting each
run's prior contribution; recompute avoids that class of bug entirely. Monthly
rollups recompute from dailies on the same rule.

### 5.7 Query families, and why `limit+1` is not a bound

**`limit+1` returned rows does not bound rows examined, and `EXPLAIN QUERY PLAN`
showing an index does not either.** A selective residual predicate — a
stage/outcome/population combination — lets SQLite walk many recency candidates
before it finds 51 matches. That is the current candidate loop
(`runs.go:771–815`) relocated into the query planner, not removed. The second
pass's acceptance criterion missed this, and it is exactly the decision to serve
population filters by cross-store join rather than by projected facets that makes
it bite.

The fix is not to measure the residual predicate. **It is to not have one.**

**Declared query families.** Filters are grouped into families, each declaring a
covering index. The conformance test asserts, for every family, that the plan:

1. is a `SEARCH … USING INDEX` on the declared index — never a `SCAN`;
2. contains **no residual predicate** over the ordering index; and
3. contains **no `USE TEMP B-TREE FOR ORDER BY`**.

(2) is the criterion the second pass lacked. All three are statically assertable
from `EXPLAIN QUERY PLAN` output.

**Selective populations get partial indexes, not a join and not a column per
filter.**

```sql
CREATE INDEX idx_run_g_failed ON run(gaggle, started_at DESC, run_id)
  WHERE phase = 'failed';
CREATE INDEX idx_stage_cost_measured ON run_stage(gaggle, stage, started_at DESC, run_id)
  WHERE has_cost_measurement = 1;
```

Review suggested a compact facets bitmask. A bitmask is the better *storage*
shape but the worse *access* shape — `facets & 4 != 0` is not indexable, so it
becomes exactly the residual predicate this section exists to remove. Partial
indexes on named facets keep the seek. The extensibility objection that removed
these in pass 2 is answered not by avoiding columns but by making their addition
**mechanical and declared**: one entry in the filter registry generates the
column, the partial index, the migration, and the plan assertion. Columns grow;
the cost of growth is bounded and enforced.

**Runtime backstop.** `modernc.org/sqlite` exposes no `sqlite3_stmt_status`,
progress handler, or interrupt (verified: `Limit`, `DBStatus`, and function/cache
registration only), so VM-step counting is unavailable through the driver. It
does expose `RegisterDeterministicScalarFunction`, so the harness registers a
`probe()` UDF in a test-only variant of each family's query and counts how many
rows the planner actually visits. That gives a real rows-visited assertion in
tests without driver support. In production the bound is structural (1–3 above)
plus §14.3's latency ceiling.

---

## 6. The projector

### 6.1 Position, and one serialized commit loop

`internal/readmodel/projector` holds the sole writable handle to projected facts.

**Change rows commit through a single serialized loop.** The projector may read
journals and prepare work concurrently on a bounded worker set, but the commit of
`(run row, run_stage rows, change row, watermark ack)` is serialized. Without
this, two workers allocate `10` and `11`, `11` commits first, a client advances
past `11`, then `10` commits — and `WHERE seq > 11` never returns it. The second
pass described a "bounded worker set" and a "leader-elected single instance"
without reconciling them; this is the reconciliation, and it applies at both
tiers (§13.3 adds the leader fence).

At tier 3 it runs as its own leader-elected process with no model change, because
it is driven by a table and a watermark rather than an in-process call from the
writer. Today the coupling is the opposite:
`cmd/goobers/runnerwiring.go` and `cmd/goobers/daemon.go` call `IngestRun` from
the writer after the run finishes.

### 6.2 Normal path

1. Writer appends and fsyncs the authoritative journal record.
2. Writer upserts the source watermark via `Intake` (§4.3) and, in-process,
   sends a wakeup.
3. Projector reads the run's event tail from `last_projected_seq` to
   `source_seq`.
4. **One serialized transaction:** `run` row, `run_stage` rows, `change` row,
   `last_projected_seq`, dirty-day marks, **and the watermark ack with its `<=`
   guard**. Atomic together — the second pass omitted the ack from this step,
   which is where the lose-or-race dilemma came from.
5. Projector publishes the invalidation carrying `<epoch>:<seq>`.
6. Separately, at lower priority: span/analytics projection into `telemetry.db`,
   and dirty-day bucket recompute.

Steps 1–5 are what list visibility waits on; step 6 never delays it. Today they
share one transaction, so list visibility waits on span files.

### 6.3 Repair: a rate bound, plus a projection floor

Repair does not need to be cheap; it needs to be **rate-limited and always making
progress**.

- A **fixed I/O budget** (configured entries/second), walking continuously and
  cycling, with a durable cursor in `projection_state`. Cost is **constant per
  unit time**, independent of history; what scales with history is *cycle time*
  (`H / rate`) — at 40,665 entries and 2,000/s, ~20 s. `readState` reports
  `lastSweepCompletedAt`.
- **A projection floor stops a retention livelock.** `projection_state` records
  the floor below which runs are intentionally aged out of the projection
  (§11.4), and repair **skips** journals older than it. Without this, journals
  outliving projection rows means repair reprojects an aged-out run, retention
  deletes it, and the next cycle repeats — consuming the repair budget and
  flooding `change`. Aged-out runs are tombstoned, not merely absent, so
  "missing" and "deliberately gone" are distinguishable.
- Unpublished directories are remembered as such keyed by directory mtime, so the
  10,906 with no `run.yaml` cost one stat per cycle. Writing `run.yaml` bumps the
  directory mtime, so promotion is detected.
- **Repair never takes a journal lock.** Prune coordination inverts (§4.3):
  retention signals through `Intake`, the projector emits `run.removed`, and a
  mid-prune `ENOENT` is removal rather than an error. This is what stops the read
  path from writing a `.lock` file again (§2.4).

**Why the second pass's bound was not a bound.** It claimed a run root whose
mtime has not advanced cannot hold anything new and is skipped without a
`ReadDir` — the current code's reasoning (`readservice/runs.go:826–839`). But
**every new run bumps its parent's mtime**, so on a live instance the root is
always dirty, the watermark never short-circuits, and repair reads 40,665 entries
every pass. That reproduced the diagnosed pattern inside a document claiming to
enforce against it.

**Date-sharded run directories** (`runs/<date>/<id>/`) would make cycle time
`O(runs since last cycle)`. Deliberately deferred: a layout change touching
`FindRunDir`, `RunDirs`, retention, prune, and a 40,665-directory migration, and
the rate bound makes it unnecessary at tier 1/2. Worth revisiting at tier 3 where
`LIST` is slow *and metered* (§13.5).

### 6.4 Overload: a stated ceiling and a shed order

**Shed order**, most-shed first: bucket recompute → span/analytics projection →
repair sweep. Run-row projection is never shed.

**Lag ceiling.** Below `lagCeiling` (default 30 s), affected surfaces are `stale`
with a number. Above it, **`unavailable` with reason `projection_lag_exceeded`**
rather than presenting ever-staler data as merely stale. `readState` also reports
`pendingIntake` and `oldestPendingSourceAge` (§4.1), so "catching up" is
distinguishable from "stuck."

### 6.5 Rebuild: a new epoch and a safe generation swap

Rebuild mints a **new epoch** and builds `read-<epoch>.db`.

The second pass said "build a temp file and rename," which is unsafe as written:
the `-wal` and `-shm` files cannot be atomically swapped with the main file by one
rename; on Unix existing pooled connections keep reading the old inode; on
Windows replacing an open database generally fails.

The sequence is therefore explicit:

1. Mint epoch *E*; build `read-<E>.db` in bounded transactions. The current epoch
   stays open and readable throughout.
2. Validate *E* (schema version, row counts, differential spot-check).
3. **Barrier:** stop the commit loop. Intake accumulates durably in the *old*
   store's `run_intake` and is **replayed into *E*** before the swap — the source
   watermark protocol (§4.3) makes this a resume, not a gap.
4. Quiesce and **close the entire reader pool**, update the epoch pointer, reopen
   against *E*. Requests during the swap get 503 + `Retry-After`, not a stale
   inode.
5. Retain the previous epoch's file until the swap is confirmed, then remove it.

A crash before step 4 leaves the old epoch authoritative and discards or resumes
*E*; it can never partially publish. Clients holding a pre-swap cursor get
`epoch_changed` (§4.2) and refetch a snapshot.

`telemetry.db` rebuilds independently; lists never wait on it. Rebuild is an
**availability metric with a budget**, measured against the real input split
(§2.6). `goobers telemetry --rebuild` remains the entry point and gains
`--read-model` / `--analytics` scoping.

### 6.6 The one-time transition

Additive and revertible:

1. Ship `read.db` construction alongside the existing store. Nothing reads it.
2. Build it by rebuild-from-journals on first start (191 MB; §16 measures the
   wall time).
3. Cut reads over behind a config flag, defaulting on, with the old path intact.
4. Only in a later release, drop the unused `runs` table from `telemetry.db`.

Rollback before step 4: flip the flag, or delete `read.db` and revert the binary.
No journal is touched at any step.

---

## 7. The read contract

### 7.1 Cost class and budget, declared in the contract

```go
type CostClass string

const (
    CostBounded   CostClass = "bounded"    // a declared query family (§5.7); zero journal opens
    CostSingleRun CostClass = "single-run" // one run's journal, once per fingerprint
    CostAggregate CostClass = "aggregate"  // pre-aggregated buckets; bounded bucket count
)

type Route struct {
    ID          RouteID
    Method      string
    Path        string
    ActionClass ActionClass
    Capability  string
    Cost        CostClass     // NEW — required
    Budget      time.Duration // NEW — required, non-zero
}
```

A route without a class and a non-zero budget fails a contract test — which is
how the diagnosis's §8.1 ("the classification is enforced rather than
documented") becomes true.

Budgets come from §16's measured **p99.9**, not from taste, and are enforced by
`context.WithTimeout` in the router *and* `http.Server.WriteTimeout`, currently
unset (`internal/httpapi/server.go` sets only `ReadHeaderTimeout`). Three rules:

- **Every server budget is strictly below the client's 10 s abort**, which becomes
  a backstop rather than the only bound.
- **Shed at admission over accept-and-timeout.** Queue wait counts against the
  budget, so a saturated class would otherwise accept work it cannot finish. A
  fast 503 with `Retry-After` is cheaper for both sides.
- **Deadline expiry must actually stop the work.** A cancelled request's
  goroutine must not continue an expensive query; §14 asserts it.

### 7.2 The `readState` envelope

Additive top-level field on every read response:

```json
{
  "runs": [ ... ],
  "nextCursor": "…",
  "readState": {
    "epoch": "01J8…",
    "appliedSeq": 918342,
    "sourceApplied": { "runId": "504ba1f4…", "journalSeq": 47 },
    "observedAt": "2026-07-29T18:12:03Z",
    "lagSeconds": 0.4,
    "pendingIntake": 0,
    "oldestPendingSourceAge": 0,
    "lastSweepCompletedAt": "2026-07-29T18:11:47Z",
    "completeness": "complete",
    "degraded": []
  }
}
```

`lagSeconds` and `pendingIntake` are **source-relative** (§4.1): a projection
sequence alone cannot reveal an append the projector has not discovered, so
freshness that ignores pending intake is not freshness.

**`completeness: "partial"` is narrow, and `budget_exhausted` is not one of its
reasons.** This is a correction: the second pass listed `budget_exhausted` as a
`partial` reason "with what was returned," which is a truncated list returning
200 — the silent-omission failure in a new costume.

| Situation | Response |
|---|---|
| An interrupted, truncated, or over-budget page | **503** + `Retry-After`. Never a 200 |
| A **named, described** missing partition — analytics not yet projected, a run root not yet swept | 200, `partial`, with the partition named **and an expiry expectation** |
| Projection lag below `lagCeiling` | 200, `complete`, with `lagSeconds` |
| Projection lag above `lagCeiling` | **503**, `projection_lag_exceeded` (§6.4) |

`partial` reasons: `read_model_rebuilding`, `analytics_rebuilding`,
`sweep_incomplete` (with the roots not yet cycled). Each carries
`expectedCompleteIn`, because `partial` without an expiry becomes wallpaper.

Three states, never a fourth indefinite one: current; stale by a stated amount;
unavailable with a reason. No path can hang.

### 7.3 Cursors

Keyset only, never offset, encoding the ordering key `(started_at, run_id)`, the
schema version, the **epoch**, and a hash of the normalized filters. Existing
50/200 limits are retained deliberately — they are what the interface renders.

Every page belongs to a declared query family (§5.7); there is no candidate loop.

### 7.4 Read-your-write uses the source position

A mutation commits the **journal** and returns `(runID, journalSeq)`. It cannot
return `change.seq`: the projector allocates that asynchronously, *after* the
mutation has returned, so at response time the value does not exist — and making
the mutation wait for projection would couple write availability to the derived
store. The second pass got this wrong.

A read may carry `If-Source-Applied: <runID>:<journalSeq>`. The server compares
against `projection_state.last_projected_seq` for that run, waits up to a
fraction of the route budget, then answers — stating in `readState.sourceApplied`
whether it reached it. `<epoch>:<seq>` remains the ordering and SSE cursor.

---

## 8. Live updates

### 8.1 Server

Delete `internal/httpapi/eventstream.go`'s change detection: the 100 ms ticker,
the 5 s sweep that stats every historical run, per-run offset/digest state for all
history, the in-memory `history` ring, and the random per-process session id
(`newEventSession`, `eventstream.go:240`). Replace with a tail of `change`.

- **Cursor = `<schemaVersion>:<epoch>:<change.seq>`** — durable,
  process-independent, portable across replicas.
- Three named conditions, not one generic `stale_cursor`: `epoch_changed` (§4.2),
  `feed_truncated` (below the retention floor), `schema_changed`.
- Invalidations publish **after** the transaction that produced the `change` row,
  carrying `<epoch>:<seq>`, so "refetch" and "the data is there" cannot be out of
  order. Today the projection updates on run *finish* while the stream discovers
  change by polling the filesystem — two mechanisms with different latency,
  completeness, and failure modes.
- Heartbeats keep 15 s and become contractual: a client missing two consecutive
  heartbeats treats the stream as dead. Fixes #1711.

### 8.2 Client

Three primitives, the only way a page fetches data:

- **`useBoundedList`** — keyset pagination over a client-side ordered window. A
  scoped invalidation **patches or prepends within the loaded window**; it never
  resets pagination, which resets only on filter/scope change or explicit retry.
  Fixes #1713: today `runsHistory.ts:178` calls `refresh()` on any matching `run`
  invalidation, and `refresh()` resets `cursor: undefined`
  (`runsHistory.ts:102`), discarding everything paged in. Responses are applied
  by `<epoch, appliedSeq>` — **and a differing epoch forces a snapshot rather
  than a discard**, which is what stops §4.2's permanent-staleness trap.
- **`useSingleRun`** — one loader per `(runId, sourceFingerprint)`, shared by
  summary, event ledger, and stage attempts, so `getRun` and `listRunEvents` stop
  reparsing the same bytes (`runDetailData.ts:149`).
- **`useAggregate`** — bucket-backed, window-parameterized.

One coalescing rule — **at most one in-flight and one queued refresh per query
family, and a useful in-flight request is never aborted merely because a newer
event arrived** — and one freshness rule: render `readState`, not connection
state. Today `LiveFreshness` (`liveData.tsx:24`) describes the SSE *connection*,
which is not how current the data is.

`DATA_CACHE_TTL_MS` (`dataCache.ts:3`) drops from 30 s to a short coherence
window: with an ordered sequence and scoped invalidations the cache no longer
needs a TTL to bound staleness, and 30 s is incompatible with read-your-write.

---

## 9. Isolation inside the daemon

One process for V1 with explicit internal contracts. §4.4 and §13.3 make the
later split a deployment change.

1. **Bounded worker pools per cost class.** Bounded high, aggregate 2, single-run
   in between. Overflow returns a fast 503. An analytics aggregate can no longer
   block a list, and the Overview's five queries can run concurrently.
2. **A separate bounded budget for the projector**, plus §6.4's shed order.
3. **The two measured hot paths removed**: the 1.3 s instance-journal append
   (§2.2) and the 4–17 s active-run scan (§2.1).
4. **A read-only connection pool** (§5.2).

Together these are the intervention §2.3's hypothesis predicts will fix
run-detail timeouts — which §16 validates or refutes rather than assumes.

---

## 10. Extensibility

- **Projector units are declared.** Each declares the event types it consumes and
  the columns it owns, and is a pure function `(prevRow, events) → nextRow`.
  Adding a fact = one unit + one migration; discovery, repair, rebuild,
  invalidation, and determinism testing are inherited.
- **Filters are declared with their family and index** (§5.7). One registry entry
  generates the column, the partial index, the migration, and the plan assertion.
  **A new filter cannot ship with a residual predicate.**
- **Views compose primitives.** §8.2's three hooks are the only data primitives,
  and the harness fails a page whose request count grows with entities rendered.

---

## 11. One topology (tiers 1–2)

**11.1 In the daemon.** Projector in-process with a serialized commit loop, both
stores at configured paths, admission control per §9.

**11.2 Standalone portal (`goobers dashboard`, no daemon).** Today constructed
with **no index at all** (`cmd/goobers/dashboard.go:394`), so every list is a full
scan — and it stands up a second filesystem-polling change detector. New: open
`read.db`; if absent or incompatible, **build it and say so**, with
`read_model_rebuilding` and progress. `single-run` works immediately; `bounded`
becomes available on completion — now seconds, because of §5.1. If the volume is
read-only, serve **explicitly degraded** — banner, `single-run` only — never a
silent O(H) scan.

**11.3 CLI in-process.** Same read service, same stores. The journal-scanning
path survives as an explicit `--authoritative` flag, documented as O(total
history), used by operators and by §14.7's differential tests. Never a silent
fallback; today `ListRuns` selects it silently whenever `Telemetry == nil`
(`runs.go:413`).

**11.4 Queryable history, and its relationship to journal retention**

**Position: 90 days of full-fidelity `run` rows; aggregate buckets beyond.**

The two retentions are independent and must be stated together, because their
interaction is what created §6.3's livelock:

| | Journal retention | Projection retention |
|---|---|---|
| Governs | `runs/<id>/` on disk | `run` / `run_stage` rows |
| Default | existing policy | 90 days |
| If journal outlives the row | The run is **tombstoned below the projection floor**; repair skips it; it is answerable in aggregate, not individually listable | — |
| If the row outlives the journal | Impossible — retention signals through `Intake` and the projector emits `run.removed` (§4.3) | — |

Consequences: the `run` table's working size is bounded by rate rather than
instance age; all-time analytics comes from buckets; a six-month-old run is
answerable in aggregate but may not be individually listable. **That last point is
the only product-visible decision in this plan and needs a product call, not an
engineering one.** 90 days is a recommendation.

---

## 12. What gets deleted

| Removed | Where | Because |
|---|---|---|
| Request-path reconciliation, throttle, in-memory watermarks | `readservice/runs.go:840–940`; `reconcileMu`/`lastReconcile`/`reconcileWatermarks` | Durable source watermarks; rate-bounded background repair (§4.3, §6.3) |
| `IndexedRunIDs()` full materialization | `rollup/query.go:329` | Indexed lookups + `projection_state` (§5.3) |
| Status-pushdown heuristics + the false invariant comment | `readservice/runs.go:593–681` | Declared query families over a maintained column (§5.4, §5.7) |
| The candidate/hydrate loop | `readservice/runs.go:771–815` | No residual predicates (§5.7) |
| Backwards latest-terminal walk | `readservice/runs.go:450–495` | One indexed aggregate |
| Unindexed window function over all history | `rollup/query.go:278` | Declared index (§5.2) |
| Journal-walking active-run count on read paths | `localscheduler/reconcile.go` + 4 call sites | Stored, indexed phase (§5.4). Retained only as the no-projection cold-start primitive |
| Filesystem polling, 5 s sweep, per-run stream state, in-memory history, random session cursor | `httpapi/eventstream.go` (~1,000 lines) | `change` tail (§8.1) |
| Whole-run delete-and-reinsert across 18 tables | `rollup/ingest.go:28,76` | Incremental tail projection (§6.2) |
| Journal-scan fallback as a silent default | `readservice/runs.go:413,710` | Explicit `--authoritative` (§11.3) |
| Index-free standalone construction | `cmd/goobers/dashboard.go:394` | One topology (§11.2) |
| `SetMaxOpenConns(1)` | `rollup/db.go:61` | Reader pool (§5.2) |
| Full-journal re-read per instance-log append | `journal/instance.go:81` | Bounded tail read under the same lock (§17 Wave 1.1) |
| Pagination reset on live invalidation | `runsHistory.ts:94–102,178` | In-place patching (§8.2) |

Kept deliberately: the three observer seams from the diagnosis's Appendix B
(`openRunObserver`, `reconcileScanObserver`/`reconcileInspectObserver`,
`journalReadObserver`), re-pointed at the new components.

---

## 13. Cloud trajectory

### 13.1 What carries forward unchanged

`run`/`run_stage`/`change`/`run_intake`/`projection_state` schema; stored phase;
incremental projection; buckets by recompute; the source-watermark protocol; the
epoch; gaggle-leading indexes and the authz predicate rule; declared query
families; cost classes, budgets, `readState`, admission control; cursors and
source-position read-your-write; the three client primitives; and the
`Reader`/`Intake`/`Projector` interfaces — which are what make the process split
a wiring change.

### 13.2 What changes: the store backend and the process layout

At tier 3 the read model is **shared, not per-replica.** Per-replica embedded
SQLite means every pod start, rolling deploy, and scale-out pays a rebuild
dominated by **29,759 file opens over a network or blob mount** — a
readiness-probe problem that makes horizontal scaling impossible. Sharing it
makes replicas stateless and dissolves the "N replicas at N points of freshness"
problem rather than managing it.

**What lands early is the semantic seam, not a maintained second backend.** Wave 2
ships the store interface and a **backend-neutral conformance contract** —
source watermarks, epochs, serialized commits, query-family/no-residual-predicate
invariants, truthful source-relative freshness — expressed so they are not
SQLite-specific. Actually *maintaining* a second backend is deferred to Wave 7.
This is a correction to the second pass, which put a live two-backend suite in
Wave 2: the early cost worth paying is semantic, not operational.

Today's store is SQLite-shaped in ways that do not travel (`julianday()` in
migrations v14/v15, `INSERT OR IGNORE`, `_pragma` DSNs, raw-DDL migration
strings). The seam keeps new queries from adding to that debt; it does not require
running Postgres in CI from Wave 2.

`ATTACH` (§5.1) becomes two schemas in one database.

### 13.3 The projector: serialized commits plus a fenced lease

Leader-elected, single instance, writing the shared store — **and the §6.1
serialized commit loop is a cloud correctness requirement, not deployment
wiring.** Shared-database sequences are allocated before commit, so parallel
committers strand clients past a lower uncommitted seq.

Leadership additionally needs a **fenced lease**: a deposed leader must not commit
after failover or during rolling overlap. Fencing is by lease token checked in the
commit transaction, so a stale leader's commit fails rather than interleaving.

### 13.4 Authority changes, and recovery plans must know

If Temporal history projects into the journal format, the chain is history →
journal → read model, and the journal is itself a projection. "You can always
rebuild from journals" is a tier-1/2 statement.

### 13.5 Repair on blob storage

§6.3's rate-bounded sweep is correct at tier 1/2 but a continuous background
`LIST` over a blob mount is slow *and metered*. Two options, both a repair-strategy
swap behind `Projector`: drive repair from the authoritative writer set
(enumerate workflow executions) — preferred, since the writer set is known — or
adopt date-sharded directories (§6.3).

### 13.6 What gets more valuable in the cluster

Server budgets strictly below the ingress read timeout, so the server stays the
thing that decides. **Queue depth per cost class as an autoscaling signal** — CPU
is not, for an I/O-bound read tier. `503` + `Retry-After` cooperating with client
backoff. Cost classes eventually allowing `aggregate` to route to its own
deployment. Two SSE specifics to document: response buffering must be disabled at
the ingress (`proxy_buffering off` / `X-Accel-Buffering: no`) or events never
flush; and graceful drain must let clients reconnect, which cursor-based resume
handles.

---

## 14. The acceptance bar

The diagnosis's §8, with mechanism and enforcing test. A property with no test is
not claimed.

| # | Property | Mechanism | Enforced by |
|---|---|---|---|
| 1 | Classified cost | `Route.Cost` + `Route.Budget` required (§7.1) | Contract test: every route has a class and non-zero budget; a `bounded` route firing `openRunObserver` fails; **a cancelled request's goroutine must stop doing expensive work**, asserted by observer counts after deadline |
| 2 | Bounded lists **in work, not just in rows** | Declared query families with no residual predicate and no temp b-tree (§5.7) | At 100k runs / 1M+ attempts, for **every family**: plan is `SEARCH … USING INDEX` on the declared index; **no residual predicate**; no `USE TEMP B-TREE`. Plus a `probe()` UDF counting **rows actually visited**, bounded at `limit+1` (× k for merged scopes). Zero journal opens, zero directory reads |
| 3 | Latency, stated as a reliability bar | Indexed keyset on a bounded working set; buckets (§5.2, §5.6, §11.4) | **Cold and warm p99.9** plus a **hard maximum server-side bound** — not warm p95. ≤ 2× growth from 10k → 100k runs. Measured on **slow-disk and low-core developer hardware** as well as the reference host, reported separately with hardware |
| 4 | No fan-out per entity | One latest-outcome aggregate; three client primitives (§8.2, §10) | A 2,000-workflow page issues one aggregate request and zero per-workflow requests; harness fails on request growth with entity count. **Multi-gaggle fan-out has a tested ceiling** (§5.5) and exceeding it is a typed refusal, not an unbounded merge |
| 5 | Bounded behavior under **sustained mixed load** | Scoped invalidations after commit; coalescing; admission control (§7.1, §8, §9) | Not a 60-second burst: **sustained concurrent reads, scheduler writes, projection, SSE traffic, rebuild, restart, and retention together.** Unrelated views perform zero refreshes; per-family in-flight ≤ 1 and queued ≤ 1; zero aborts attributable to a newer event; **bounded queue depth** per class |
| 6 | Legible, source-relative freshness | `readState` with source-relative lag; three states; narrow `partial` (§7.2) | Fault injection yields exactly one of current / stale-with-number / unavailable-with-reason. **An interrupted or over-budget page returns 503, never a 200 with `partial`** — asserted directly. `partial` always names a partition and an expiry. `pendingIntake` is nonzero whenever an unconsumed watermark exists |
| 7 | No silent omission | Completeness from `projection_state`; authz as an indexed predicate (§5.3, §5.5) | Differential test per filter and cursor against the journal-derived reference under an injected clock. Injecting a dirty run / incomplete sweep yields `partial`. A test asserts no list applies an authz filter after `LIMIT` |
| 8 | Crash, restart, and **adverse-state** safety | One serialized transaction including the change row and watermark ack; epoch swap (§6.2, §6.5) | Kill at every transaction boundary during project, rebuild, and **epoch swap**: previous epoch intact or resumed; no partial epoch readable; **intake replayed, not skipped**. Restart reprojects only pending intake plus non-terminal rows — asserted O(active + pending). Plus **disk-full, read-only volume, corrupt store, and daemon↔standalone transitions** |
| 9 | Determinism | Pure `(prevRow, events) → nextRow`; buckets recomputed (§5.6, §10) | Rebuilding from the same journals produces byte-identical canonical rows; recomputing a day's bucket twice is identical |
| 10 | Single-run isolation | `useSingleRun` keyed by `(runId, fingerprint)` (§7.1, §8.2) | Run detail cost independent of H; an unchanged journal parsed **at most once** per fingerprint across summary + events + attempts |
| 11 | **Instance-log sequence uniqueness survives Wave 1.1** | Bounded tail read under the existing cross-process lock (§17 Wave 1.1) | `TestInstanceLogConcurrentAppendsAllocateUniqueMonotonicSequence` (25 independent handles) and the sequential independent-writer test **retained and passing**, plus a bound on bytes read per append |

On the figures: 100k runs / 1M+ attempts are the diagnosis's working numbers. The
live instance is at 29,759 published runs after roughly six months, so 100k is the
right target and §11.4's retention position keeps the working set below it. §16
confirms the curve.

---

## 15. Answers to the open questions

**15.1 The writer set, and is it a choice?** Four writers: daemon runners, the
out-of-process `goobers run`, the stalled-run terminalizer
(`cmd/goobers/stalledruns.go:194`), and retention. `goobers run` **stays
first-class** but stops being a silent writer: it records source watermarks
through `Intake` (§4.3) on every query-visible transition. The current situation
is subtler than "two uncoordinated writers": `goobers run` takes the same
`up.lock` and *delegates* to a live daemon when one holds it
(`cmd/goobers/run.go:99–102`) — except that `up.lock` can go stale, as it is on
the measured instance. Either way the daemon has no in-memory knowledge of runs
written while it was down, which is the real reason reconciliation is currently a
correctness requirement.

**15.2 One service or several?** One process for V1 with explicit contracts (§9);
§4.4 and §13.3 make the split a deployment change.

**15.3 What is the durable unit of change?** **Two, deliberately.** The
authoritative unit is the journal record, identified by `(runID, journalSeq)` —
what a writer can promise. The projection-order unit is a `change` row identified
by `<epoch, seq>`. The first pass answered "a record in a new durable feed file";
the second answered "one number for everything." Both were wrong in different
directions.

**15.4 How much history at full fidelity?** 90 days of run rows, buckets beyond,
with the journal-retention relationship stated (§11.4). A product call.

**15.5 What when queryable state is behind?** Serve with a **source-relative**
number and let the caller opt into waiting (§7.4) — up to `lagCeiling`, above
which return 503 `projection_lag_exceeded` rather than presenting ever-staler data
as merely stale. Never silently substitute a slower authoritative path, and never
return an interrupted page as a successful `partial`.

**15.6 Stored or derived phase?** **Stored** (§5.4). Deriving it measures 17.2 s
for an answer of 2.

**15.7 Where does "did any work happen" belong?** A **stored dimension**
(`disposition`), so it can be excluded before pagination and aggregation.
Semantics stay with #1429; UX and denominators with #1439.

**15.8 The diagnostic surface's cost ceiling?** Measured (§2.3): largest event log
131 KB, p99 40 KB. Pagination is not the answer. The remaining question — whether
every run-detail timeout is head-of-line blocking — is a hypothesis for §16, not
a conclusion.

**15.9 Which existing behaviors are requirements, which are accidents?**

| Behavior | Verdict |
|---|---|
| Client 10 s abort | **Accident as a primary bound; kept as a backstop** strictly above every server budget (§7.1) |
| 30 s client cache TTL | **Accident.** Reduced to a short coherence window (§8.2) |
| 2 s reconcile throttle | **Deleted** with the request-path reconcile (§6.3) |
| 100 ms change poll | **Deleted** with filesystem polling (§8.1) |
| 5 s idle sweep, 30 s idle-after | **Deleted** — polling artifacts |
| 50 / 200 page limits | **Requirement.** What the interface renders |
| 15 s heartbeat | **Requirement, promoted** to a liveness contract (§8.1) |
| Instance-log reread under the journal lock | **Requirement in substance, accident in cost.** It *is* the cross-process sequence allocator (§2.2); the O(history) read is the accident (§17 Wave 1.1) |
| `Promise.allSettled` per-phase Overview | **Transitional.** Correct given five independent queries (#1709); replaced by one bounded multi-phase query retaining per-group failure independence |

---

## 16. Conformance and load harness

The existing harness does not cover the failing surfaces (§2.5). Wave 0, because
nothing after it is measurable — and because §2.3's hypothesis can only be settled
here:

1. **Real definitions.** Replace `minimalDefinitions()` with a parameterizable
   inventory (2,000 workflows for §14.4).
2. **The missing surfaces.** `Instance()`, `Gaggles()`, `Workflows()`,
   `LatestPerWorkflow`, `GetRun` + `RunEvents` + `StageAttempts`.
3. **Sustained mixed load**, not a burst: concurrent reads, scheduler journal
   writes, projection, SSE, rebuild, restart, and retention simultaneously. This
   is the experiment that validates or refutes head-of-line blocking (§2.3).
4. **Adverse states**: slow disk, low-core hardware, disk-full, read-only volume,
   corrupt store, daemon↔standalone transitions.
5. **Pathologies from the live instance**: 27% unpublished directories with stray
   `.lock` files, a 324 MB instance journal, 2.26 GB of spans against 191 MB of
   events.
6. **Work assertions, not just row counts**: the `probe()` UDF for rows visited
   (§5.7); observer seams for zero journal opens and zero request-path directory
   reads; queue-depth ceilings; and no goroutine continuing expensive work past
   its deadline.
7. **The differential oracle** (§14.7), per filter, per cursor, under an injected
   clock.
8. **Rebuild cost by store and by storage class**, including at least one network-
   or blob-backed mount, because §2.6's file-open cost decides §13.2 and no figure
   exists.
9. **Cold and warm p99.9 reported separately, with hardware.**

Run at the current instance's shape as 1×, and at 3× / 10×. **Publish the baseline
before changing anything.**

---

## 17. Plan

Nine waves including Wave 0, each independently valuable, independently
revertible, ending with a measurement.

Risk posture throughout: no wave changes the journal format or the directory
layout; every wave touching the read path ships behind a flag with the prior path
intact; no wave's rollback requires touching a journal.

### Wave 0 — Make it measurable *(no product change)*

§16's harness. Publish the baseline at 1× / 3× / 10×, including the mixed-load
experiment that tests §2.3's hypothesis.

**Risk:** none — test-only.
**Exit:** every §2 number reproducible in-tree; every §14 property has a failing
test; the blob-mount rebuild figure exists; §2.3's mechanism is confirmed or
replaced.

### Wave 1 — Reduce the measured hot paths *(five small independent diffs)*

| # | Change | Effect | Risk / rollback |
|---|---|---|---|
| 1.1 | **Bound the instance-log append without breaking sequence allocation.** Under the *existing* cross-process journal lock, recover the sequence by reading only the **last complete record** backwards from EOF (the bounded chunked-read pattern already in `eventstream.go:612`), instead of `readEvents` over the whole file. Torn-tail detection comes from the same read | Removes 1.3 s and growing per journaled scheduling decision, under a lock, in the serving process | **Medium, not low** — corrected from the second pass. The reread *is* the cross-process sequence allocator: independent handles allocate under the lock, and `TestInstanceLogConcurrentAppendsAllocateUniqueMonotonicSequence` exists because of #530's "two events sharing seq:5". Per-handle in-memory sequence would reintroduce that bug. **Both independent-writer tests are retained, plus a new assertion bounding bytes read per append.** Rejected alternative: a sequence sidecar — a second file and a second recovery path for no benefit over a tail read. Rollback: revert the commit |
| 1.2 | Indexes on `runs`: gaggle-leading scoped indexes plus a global recency index (§5.5) | Removes full-scan + sort per page and the unindexed window function | Very low. Additive `CREATE INDEX` migration. Rollback: drop them |
| 1.3 | Split reader/writer handles; reader pool `MaxOpenConns=NumCPU`, `mode=ro`; `synchronous(NORMAL)` | Readers stop serializing behind each other and the writer | Low–medium. Uses WAL as already configured. Gate on §16.3's mixed-load run |
| 1.4 | Background active-run sampler off the request path, served from memory with its age reported. **No sample yet returns `read_model_rebuilding` with progress — never the synchronous scan.** Deliberately throwaway; Wave 2 replaces it | `/instance`, `/gaggles`, `/workflows` stop paying 4–17 s | Low. Corrected from the second pass, which kept the 17-second scan as the no-sample fallback and so preserved the exact failure on a cold daemon |
| 1.5 | `http.Server.WriteTimeout` + per-route `context.WithTimeout`; cancellation must stop the work | No request can hang | Low. Budgets generous here, tightened in Wave 4 from measured p99.9 |

**Exit:** Overview, Workflows, and the warnings widget load on the live instance.
Whether run-detail timeouts are resolved is the §2.3 experiment's answer, reported
either way.

### Wave 2 — Read model

`read.db` per §5.3; stored indexed phase; complete run rows; `change` with
**epoch**; declared **query families** with no-residual-predicate plan assertions
and the `probe()` rows-visited harness (§5.7); gaggle-leading indexes plus the
unrestricted fast path and the fan-out ceiling (§5.5); the filter registry; the
**store interface and backend-neutral conformance contract** (§13.2 — semantic
seam only, no maintained second backend); one latest-outcome aggregate. Delete the
candidate loop, status-pushdown heuristics, and the backwards terminal walk.
Transition per §6.6.

**Risk:** medium — the substantive change. Contained by: `read.db` is a new file;
cutover is a flag with the old path intact; the differential oracle gates it;
rollback is deleting a file and flipping a flag.
**Exit:** §14.2 holds — every family, no residual predicate, rows visited ≤
`limit+1`, zero journal opens at 100k runs. Wave 1.4's sampler deleted. §14.3's
p99.9 bar met.

### Wave 3 — Projector

`Reader`/`Intake`/`Projector` interfaces; the projector as sole writer with a
**single serialized commit loop**; the **durable source-watermark protocol** with
in-transaction acknowledgement (§4.3); the O(active + pending) restart pass;
rate-bounded lock-free repair with a **projection floor** (§6.3); the **epoch
rebuild and pool swap** (§6.5); §6.4's shed order and lag ceiling. Delete
request-path reconcile and in-process `IngestRun`-on-finish.

**Risk:** medium. Contained by: the compile-time `Reader` boundary makes
"reads never write" structural; repair is rate-limited, so a bug is slow rather
than catastrophic; the old reconcile path is deleted only after the oracle is
green with it disabled; the epoch swap is tested by kill-at-every-boundary
including mid-swap.
**Exit:** §14.7, §14.8, §14.9 hold. No `.lock` file created by a read path.
`readservice` holds no writable handle (compile-time).

### Wave 4 — Read contract *(parallel to 2/3)*

`Route.Cost` + `Route.Budget`; `readState` with **source-relative** freshness;
**503 for interrupted or over-budget pages, `partial` only for named partitions
with an expiry**; admission control with shed-at-admission; cancellation actually
stopping work. Client renders `readState`.

**Risk:** low. Additive to the wire contract; touches router and contract, not the
projection.
**Exit:** §14.1, §14.6 hold. No fourth indefinite state under fault injection, and
no truncated 200.

### Wave 5 — Live updates

Cursor `<schemaVersion>:<epoch>:<seq>` with `epoch_changed` handling; delete
filesystem polling, sweep, per-run stream state, in-memory history, session
identity. `useBoundedList` with in-place patching and epoch-aware snapshot
(#1713); heartbeat watchdog (#1711); coalescing; stage attempts refresh on live
runs (#1714).

**Risk:** medium on the client, low on the server. The client primitives land page
by page rather than in one cutover.
**Exit:** §14.4, §14.5 hold. #1711, #1713, #1714 closed with tests.

### Wave 6 — Aggregates and retention

Day/month buckets by recompute (§5.6); the 90-day window with its journal-retention
relationship and projection floor (§11.4, §6.3); all-time analytics from buckets.

**Risk:** low–medium. Buckets are recomputable, so a bug self-heals. **The
retention window needs product sign-off before it ships.**
**Exit:** all-time analytics bounded; recompute idempotence tested; no
repair/retention livelock under §16.3's sustained run.

### Wave 7 — Cloud readiness

Shared-store backend maintained for real; stateless API replicas; the projector as
a **fenced** leader-elected deployment (§13.3); spans to OTLP only, out of the
rebuild path; repair strategy swap (§13.5); ingress SSE documentation.

**Risk:** medium, **isolated to deployment**, because Wave 2 shipped the semantic
seam and Wave 3 the interface boundaries. Tier-1/2 keeps the embedded backend by
construction.
**Exit:** an API replica starts in seconds holding no durable state; the
conformance contract passes against the shared backend; fencing tested against a
deposed leader.

### Wave 8 — Mutability seam *(no new mutations)*

`If-Source-Applied`; mutations return `(runID, journalSeq)`; `Idempotency-Key`
durably recorded; `ETag`/`If-Match` on definition reads; actor attribution on
journal and `change` rows. The three routes stay 501 until #466/#468.

**Risk:** low. Additive; no mutation enabled.
**Exit:** read-your-write demonstrated end to end; a retried submit after a client
abort provably does not duplicate.

### Sequencing notes

- **Wave 0 now gates a claim, not just a number.** §2.3's mechanism is a
  hypothesis until its mixed-load experiment runs.
- **Wave 1.1 is the highest-risk item in Wave 1**, not the lowest. It is small in
  diff and load-bearing in invariant.
- **Three things are pulled early because they are cheap now and expensive later:**
  gaggle-leading indexes (1.2), the semantic store seam (2), and the epoch (2) —
  retrofitting an epoch into a deployed cursor format means every live client
  refetches.
- **Wave 4 runs parallel to 2/3.**
- **Existing issues:** #1741 → 1.4 then 2. #1782 → 2. #1665 → Wave 0's experiment
  then Wave 1. #1712 → 2 + 5. #1711/#1713/#1714 → 5. #1882 → 1.4 removes its
  cause; its abort/restart bug remains separately real. #1888–#1892 (filed under
  #1883) are approximately Waves 2–3 and should be **re-scoped against this
  document rather than started as written**. #1439/#1429 unblocked by
  `disposition`. #644 unblocked by §5.5. #652 → 7. #1410 closed by Waves 1–3.

---

## 18. Revision history

Recorded because this class of error is the subject of the diagnosis, and a design
document that cannot show its own corrections has not earned §14.

### 18.1 Second pass → third pass (review findings)

| Finding | Why it mattered | Resolution |
|---|---|---|
| **Wave 1.1 would break the multi-process sequence invariant** | The reread under the journal lock *is* the cross-process sequence allocator. Per-handle in-memory sequence lets two handles both recover `N` and append duplicate `N+1` — #530's original "two events sharing seq:5". Verified: `TestInstanceLogConcurrentAppendsAllocateUniqueMonotonicSequence` opens 25 independent handles for exactly this. The second pass called it "low risk… on-disk format unchanged," conflating format with invariant | §17 Wave 1.1 — bounded tail read under the *same* lock; both independent-writer tests retained; risk reclassified to medium; sidecar alternative rejected with reasons; §14.11 added |
| **Rebuild needs an epoch and a safe generation swap** | A rebuilt SQLite file resets `AUTOINCREMENT`, so a client cursor of `918342` against a store whose max is `100` is neither below the floor nor a schema change — it waits forever, and §8.2's discard rule makes it permanent. And rename-swap is unsafe with pooled WAL readers: `-wal`/`-shm` cannot be atomically swapped with the main file, Unix readers keep the old inode, Windows refuses. The second pass had *removed* the epoch as redundant | §4.2 epoch + `epoch_changed`; §6.5 explicit build/validate/barrier/close-pool/reopen with intake replay; §8.2 epoch mismatch forces a snapshot, not a discard |
| **The doorbell could lose out-of-process progress** | Intake on *creation only* meant later appends were invisible until repair or restart, so freshness was bounded by sweep cycle rather than by lag. And the ack was outside the projection transaction: before commit loses discovery on crash, after commit races a newer notification | §4.3 — durable per-run **source watermark**, upserted on every transition, acknowledged inside the projection transaction with a `source_seq <=` guard. Channels are wakeups only. Retention uses the same protocol |
| **`change.seq` cannot be the read-your-write token** | The mutation commits the journal; the projector allocates `change.seq` later, so the value does not exist at response time unless writes wait on the derived store. And a projection sequence cannot reveal a source append the projector has not discovered — freshness must be source-relative | §4.1 source position `(runID, journalSeq)`; §7.4 `If-Source-Applied`; §7.2 `pendingIntake`/`oldestPendingSourceAge`. The second pass's headline "one number serves three jobs" was wrong: it serves two |
| **`LIMIT + 1` rows does not bound work** | An index in the plan does not bound rows *examined*; a selective residual predicate relocates the candidate loop into the planner. Especially pointed given pass 2 removed projected facets in favour of a cross-store join. Three indexes also do not cover every combination, and k per-gaggle queries is unbounded for an unrestricted principal | §5.7 declared **query families** with a **no-residual-predicate** and no-temp-b-tree assertion; partial indexes on named facets (with why a bitmask is the worse access shape); `probe()` UDF for rows visited, since the driver exposes no stmt-status — verified; §5.5 global recency fast path plus a typed refusal above the fan-out ceiling |
| **Repair and the 90-day window livelock** | Journals outliving projection rows means repair reprojects an aged-out run, retention deletes it, and the next cycle repeats — consuming the budget and flooding `change` | §6.3 **projection floor** and tombstones; repair skips intentionally aged-out runs; §11.4 states the journal-vs-projection retention relationship explicitly |
| **Cloud ordering needs serialized commits and a fenced lease** | "Bounded worker set" and "leader-elected single instance" were never reconciled. Parallel committers on a shared sequence commit out of order and strand clients past a lower uncommitted seq | §6.1 single serialized commit loop at both tiers; §13.3 fenced leader lease checked in the commit transaction |
| **Evidence overclaimed a single universal cause** | Small event logs rule out "one huge ledger" but do not prove every run-detail timeout is SQLite head-of-line blocking; production evidence includes refresh/cancellation churn and proxy cancellations while direct endpoints were healthy | §1 and §2.3 downgraded to "leading measured hypothesis"; §16.3 makes the mixed-load experiment the arbiter; §17 Wave 0 exit gates the claim |
| **Acceptance bar too weak for a reliability claim** | Warm p95 and a 60-second burst do not support "zero timeouts"; an interrupted page returned as `partial` is silent omission renamed; Wave 1.4's fallback was the 17-second scan | §14 rebuilt: cold/warm p99.9 plus a hard max bound; sustained mixed load; adverse hardware and disk states; rows/steps, queue depth, and no work past deadline. §7.2 **503, never a truncated 200**. §17 Wave 1.4 fallback corrected |
| Two-backend suite maintained from Wave 2 | The early cost worth paying is semantic, not operational | §13.2 — backend-neutral **conformance contract** in Wave 2; a maintained second backend in Wave 7 |

### 18.2 First pass → second pass

| First pass | Why it was wrong | Now |
|---|---|---|
| A separate durable change-feed file with segments, retention floors, torn-record rules, and a lock protocol | Conflated ordering with discovery; unnecessary in one process and unworkable across nodes | §4.4 — `change` table + source watermarks |
| One physical store | List data is 191 MB and analytics 2,263 MB; cold start was gated on the larger | §5.1 — two stores, ATTACHed |
| Repair bounded by parent-directory mtime watermarks | Every new run bumps the parent's mtime, so the watermark never short-circuits and repair reads 40,665 entries every pass. Reproduced the diagnosed pattern | §6.3 — a rate bound |
| No mention of authorization | Post-filtering after `LIMIT` silently omits rows; retrofitting an authz column into ordering indexes is expensive | §5.5 — gaggle-leading indexes, pulled to Wave 1.2 |
| "Aggregate bucket deltas" | Reversible deltas need each run's prior contribution stored and subtracted | §5.6 — recompute the dirty day |
| One writer, no overload policy | Lag would grow and `readState` would report a faithfully useless growing number | §6.4 — shed order plus lag ceiling |
| Restart cost unaddressed | Implied a full scan to find unprojected runs | §4.3 — O(active + pending) |
| Steady-state rebuild described; the one-time migration not | The transition on 29,759 runs is what actually hurts | §6.6 |
| Four cost classes | `stream` is a lifecycle, not a cost | §7.1 — three |
| `run_stage` with denormalized population flags | A column per filter is an extensibility trap | §5.7 — declared registry generating column, partial index, migration, and assertion |

---

## 19. Non-goals and risks

**Non-goals.** No journal event or directory-layout change — date sharding
explicitly deferred (§6.3). No stage invocation/result envelope change. No
breaking change to `/api/v1` parameters, response fields, ordering, or cursor
semantics; `readState`, cost classes, and aggregate routes are additive, and the
epoch is added to a cursor format before any client depends on the current one.
No new service or external dependency at tier 1–2. No new mutations. No inference
of "no work" from duration or missing telemetry. No request-time full-history
fallback presented as a successful read.

**Risks and containment.**

| Risk | Containment |
|---|---|
| Wave 1.1 reintroduces duplicate instance-log sequences | Both independent-writer tests retained, including the 25-handle concurrency test; a bytes-read-per-append bound added; risk classified medium and reviewed as an invariant change, not a refactor (§14.11) |
| Stored phase drifts from the journal | `reconstructPhase` survives as a differential oracle over the whole corpus (§14.7); lag is source-relative and stated |
| The epoch swap drops intake or serves a stale inode | Intake is durable and replayed under an explicit barrier; the whole reader pool is closed before the pointer moves; kill-at-every-boundary includes mid-swap (§6.5, §14.8) |
| A query family regresses into a residual predicate as filters are added | The filter registry generates the plan assertion, so a new filter cannot ship without one; the `probe()` harness counts rows visited (§5.7) |
| Repair's rate budget is too low and a restored run stays invisible | `lastSweepCompletedAt` is in every response; the budget is configurable; the watermark protocol covers every non-exceptional case |
| Two stores diverge | `read.db` is journal-derived and wholly rebuildable; rebuild must produce byte-identical rows (§14.9) |
| `partial` becomes permanently lit | It is narrow by construction (§7.2 — never budget exhaustion), always names a partition, and always carries an expiry; a test asserts a healthy instance reads `complete` |
| Nine waves stalls halfway | Wave 1 is independent of the rest; every wave ends green and shippable with a number |
| 90-day fidelity window is wrong for someone | A recommendation with its consequence and its journal-retention relationship stated (§11.4), configurable, gated on product sign-off before Wave 6 |
| Tier-3 rebuild cost over a blob mount is worse than assumed | Measured in Wave 0 (§16.8) before §13.2 depends on it |
