# Portal read architecture — a rethink

> **Status:** Design proposal, fourth pass. Responds to
> `Goobers-Reviews/2026-07-29_portal-architecture-findings.md` (the diagnosis) and
> supersedes [`unified-index-backed-run-reads.md`](unified-index-backed-run-reads.md)
> (#1883), keeping its read-projection conclusion and replacing the parts it left
> open: ordering, the writer set, request budgeting, in-process isolation,
> topology, authorization, and the hosted shape.
>
> **Revision history is in §18** — nine errors in the first pass, seven
> correctness holes in the second, ten state-boundary findings in the third. The
> fourth pass concentrates on three boundaries the third pass left unsafe: the
> **intake store** (durable discovery cannot live inside the store that gets
> replaced), the **rebuild barrier** (a new epoch could publish silently stale),
> and **query bounds** (SQLite's plan output cannot prove the absence of a
> residual predicate, so the bound must come from an enumerated set of supported
> filter combinations instead). It also corrects a factual error: this document
> previously claimed `modernc.org/sqlite` exposes no interrupt. **It does** — and
> that is the production mechanism behind "a deadline must stop the work."

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
   supported filter combination** with a covering index, and any unlisted
   combination is a **typed refusal** — because `limit+1` returned rows does not
   bound rows *examined*, and SQLite's plan output cannot prove a residual
   predicate is absent (§5.7).

2. **Two explicit identities, not one overloaded number.**
   - **Source position** `(runID, journalSeq)` — what a writer commits and can
     return immediately. This is the read-your-write token and the basis of
     freshness, because only a source position can reveal an append the projector
     has not yet discovered.
   - **Projection position** `<schemaVersion>:<epoch>:<seq>` — the SSE cursor and
     ordering key, from a `change` row written in the same transaction as the
     fact it describes. The **epoch** is an **opaque identity minted fresh on
     every build** (§4.2), because a new SQLite file resets `AUTOINCREMENT` and a
     client cursor would otherwise outrank the entire rebuilt store forever.

3. **One projector, one serialized commit loop, and reads never write.** The
   projector owns every write to projected facts, and change rows are committed
   by a **single serialized loop** — parallel workers on a shared sequence can
   commit out of order and strand a client past a lower uncommitted seq (§13.3).
   Discovery is a **durable per-run source watermark in a stable store that is
   never rebuilt** (§4.3) — it cannot live inside the store a rebuild replaces —
   acknowledged under a guard *after* the projection commits, which is safe
   because projection is idempotent and is the only option available, since
   SQLite WAL cannot commit atomically across attached files. Channels are
   wakeups only, never the correctness mechanism. Repair is a **rate bound** — a fixed I/O budget per second that
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

// Intake is the doorbell, backed by a STABLE store that a rebuild never
// replaces (§4.3). It records durable per-run intent and nothing else; it
// cannot touch a projected fact.
type Intake interface {
    // Observed records a source watermark: the highest journal seq the writer
    // has committed for this run.
    Observed(runID string, journalSeq uint64) error
    // Removing records retention's durable intent to delete a run's journal,
    // BEFORE the unlink. Observed alone cannot express removal, and a removal
    // inferred from a missing journal is indistinguishable from a restore in
    // progress. See §4.3.
    Removing(runID string) error
}

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

**But freshness is bounded by pending intake *or* the last completed repair
sweep, whichever is weaker — not perfectly source-relative.** The journal append
and the intake upsert are in different files and cannot be made atomic. A crash,
`SQLITE_BUSY`, disk-full, or read-only transition after the journal fsync but
before the intake write leaves `pendingIntake = 0` while the projection is
behind: the exact false-"current" state this section exists to prevent. The
contract admits that residual window rather than claiming it away.

**Writer intake-failure policy.** Run progress must not depend on the derived
store, so an intake write that fails is logged and counted but **does not fail
the run**. The consequence is explicit: that run's discovery falls back to the
repair sweep, `readState` reports freshness as
`max(pendingIntake age, now − lastSweepCompletedAt)`, and a nonzero
`intakeWriteFailures` counter is itself a degraded condition (§7.2).

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

**The cursor is `<schemaVersion>:<epoch>:<seq>`.** The `epoch` is an **opaque
identity (ULID) minted fresh for every build**, independent of any value read
from the old or the new store.

The epoch is load-bearing, not bookkeeping. **A rebuilt `read.db` is a new SQLite
file, so `AUTOINCREMENT` restarts at 1.** Without an epoch, a client holding
cursor `918342` reconnects to a store whose maximum is `100`: the cursor is
neither below the retention floor nor from a different schema version, so no
named condition fires, the client waits forever, and §8.2's rule discarding
responses with a lower position makes that permanent.

**It must not be a counter in `projection_state`, because `projection_state`
lives inside `read.db` — the store a rebuild replaces.** Standalone recovery
rebuilds when the store is absent (§11.2) and rollback permits deleting it
(§6.6), so a recovered counter would restart and recreate the very collision the
epoch exists to prevent. Epochs need **equality semantics, not ordering**: any
inequality is the named condition **`epoch_changed`**, which instructs a snapshot
refetch. The third pass called the epoch "a monotonic value in
`projection_state`" while its own `readState` example already showed a ULID; the
ULID was right and the prose was wrong.

**Change-row retention is a defined floor, not an implication.**
`projection_state.min_change_seq` records the oldest retained sequence.

- **Pruning rule:** the projector deletes `change` rows below
  `max(seq at now − changeRetentionWindow, seq at changeRetentionRows from the head)`
  and advances `min_change_seq` in the same transaction as the delete. Defaults:
  a **10-minute** supported client-disconnect window or **50,000 rows**,
  whichever is larger. The window is the contract — a client offline longer than
  it must resnapshot.
- **Exact condition:** `cursor.seq < min_change_seq` → `feed_truncated` plus a
  snapshot instruction. A persisted comparison, not "roughly old."
- **The floor is pinned during a rebuild.** `min_change_seq` may not advance past
  an in-progress rebuild's `rebuildFromSeq` (§6.5), or the barrier loses exactly
  the rows it needs to catch the new epoch up. This is a hard constraint on the
  pruner, and it is the only interaction between change retention and rebuild.

### 4.3 Discovery — a durable watermark in a stable store

**The intake store is `<instance>/intake.db`, and a rebuild never replaces it.**
The third pass put `run_intake` inside `read.db`, which is wrong three ways: it
contradicts §5.2's projector-owned sole writer handle, since out-of-process
`goobers run`, the terminalizer, and retention all write intake; it races the
epoch swap, because an external process can open the old epoch before the barrier
and keep writing watermarks to the old inode after replay, losing them silently;
and on Windows the old file cannot be removed while an external process holds it.

```sql
-- intake.db — stable, small, multi-writer, never rebuilt.
-- WAL, busy_timeout(5000), synchronous(NORMAL).
CREATE TABLE run_intake (
  run_id     TEXT PRIMARY KEY,
  source_seq INTEGER NOT NULL,   -- highest journal seq the writer has committed
  removing   INTEGER NOT NULL DEFAULT 0,   -- retention intent (§ below)
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

**Ownership and failure behavior.** `intake.db` is created by whichever process
first needs it; no process owns it exclusively. `busy_timeout` absorbs ordinary
contention; a write that still fails is retried a bounded number of times, then
logged and counted per §4.1's failure policy — never fatal to the run.

**Acknowledgement is after the projection commit, under a guard.** It cannot be
in the same transaction: `intake.db` is a separate file, and **SQLite WAL does not
provide atomic commit across attached databases** — multi-database transactions
are atomic per-database only when the main database is not in WAL mode. So:

1. Commit the projection — `run` row, `run_stage` rows, `change` row,
   `last_projected_seq` — in **one `read.db` transaction** (§6.2). This part is
   genuinely atomic and must stay so.
2. Then, in `intake.db`:

```sql
DELETE FROM run_intake
 WHERE run_id = ? AND source_seq <= ? AND removing = 0;
```

The guard is the protocol. A newer append leaves a higher `source_seq`, so the
delete no-ops, the marker survives, and the projector revisits. A crash between
(1) and (2) leaves the marker, so the same source sequence is reprocessed —
harmless, because projection is idempotent on `last_projected_seq`. The third
pass claimed atomicity across the two; that was not achievable, and it is not
needed.

**Channels are wakeups only.** The in-process runner signals the projector over a
channel so steady-state latency is a send rather than a poll, but a dropped
wakeup costs latency, never correctness.

**Removal needs its own durable intent, and an ordering.** `Observed(runID,
journalSeq)` cannot express removal. If retention merely signalled and the
projector consumed the marker before the unlink, the projector would project an
ordinary row, acknowledge, and retention would then unlink with **no surviving
removal signal** — leaving a projected row whose journal is gone, which
contradicts §11.4. So removal is explicit and ordered:

| Step | Actor | Action |
|---|---|---|
| 1. Intent | Retention | `Intake.Removing(runID)` sets `removing = 1`. The `removing = 0` guard above means this marker can no longer be acknowledged as ordinary progress |
| 2. Unlink | Retention | Delete the journal directory |
| 3. Projection | Projector | Sees `removing = 1`, emits `run.removed`, deletes the run's projected rows and its `last_projected_seq`, all in one `read.db` transaction |
| 4. Confirm | Projector | Deletes the intake row, guarded on `removing = 1` |

A crash at any step leaves a marker that resolves on the next pass. A crash
between 1 and 2 leaves a run marked for removal whose journal still exists —
resolved by retention retrying, and visible meanwhile because a `removing` marker
is reported in `readState`.

**Repair reconciles both directions.** It is not only "on disk but not
projected." It must also handle **projected but no longer on disk above the
projection floor** — a journal removed without intent (an operator `rm`, a failed
restore) — by emitting `run.removed`. Without this, §11.4's "impossible" case
becomes merely "unusual."

**Restart.** Two bounded passes, no scan: drain `run_intake` (including
`removing` markers), then reproject rows the read model records as non-terminal,
since only those can have advanced. **O(active + pending)**, typically tens of
rows.

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
| `runs` has **no index** beyond its PK (`rollup/schema.go:11`) | One covering index per supported filter combination, each with a rows-visited benchmark (§5.7) | Today the "indexed" list path is a full scan plus sort, and `LatestWorkflowRunRefs` is an unindexed window function over all history (`query.go:278`) |
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

### 5.7 Enumerated query combinations, and why `limit+1` is not a bound

**`limit+1` returned rows does not bound rows examined**, and neither does an
index in the plan. A selective residual predicate — a stage/outcome/population
combination — lets SQLite walk many recency candidates before it finds 51
matches. That is the current candidate loop (`runs.go:771–815`) relocated into
the query planner, not removed.

**And `EXPLAIN QUERY PLAN` cannot prove a residual predicate is absent.** SQLite
reports `SEARCH run USING INDEX idx_ab (a=?)` while silently evaluating `c=?` as
a residual filter; the plan output simply does not enumerate residual terms. The
third pass asserted this check was statically possible. It is not, so the bound
cannot come from a plan property.

**The bound comes from a closed set instead.** Enumerate the filter
**combinations** the API and UI actually expose; give each supported combination a
covering or partial index; **return a typed refusal for any unlisted
combination.** A finite, declared set of supported queries is mechanically
enforceable in a way "no residual predicate" is not — and a refusal is honest,
where an unbounded walk is not.

```go
// internal/readmodel/queryset — the closed set. One entry per supported
// combination, not per filter. Adding a combination is a deliberate act that
// ships an index and a benchmark with it.
type Combination struct {
    Dims  []Dim         // e.g. {Gaggle, Workflow, Phase, Since}
    Index string        // the covering or partial index that serves it
    Bench string        // the harness case that bounds its rows-visited
}
```

Consequences, stated plainly:

- **Per-filter partial indexes are not sufficient**, which is the third pass's
  other error here. `phase='failed' AND has_cost_measurement=1` seeks one and
  filters the other; a population facet plus workflow and time bounds does the
  same. Combinations, not filters, are the unit that needs an index.
- **Unpinned stage-population facets are hoisted to run level.** "Any stage has
  cost" over `run_stage` requires a join, a `DISTINCT`, and usually a temp
  structure. So the projector maintains **run-level rollup columns** for the
  unpinned form (`any_stage_has_cost`, `any_stage_retry_waste`, …), and list
  queries never join or deduplicate `run_stage`. Stage-*pinned* queries still use
  `run_stage`, where the stage is an equality term and the join is a seek.
- **The refusal is a real product surface.** An unlisted combination returns a
  typed `unsupported_filter_combination` naming the supported neighbours, not a
  slow success. The set is generated into the OpenAPI/contract surface so the UI
  can only construct supported combinations.

**Measuring actual work.** `modernc.org/sqlite` exposes no `sqlite3_stmt_status`
and no progress-handler wrapper (verified in v1.54.0), so VM-step counting is
unavailable. It does expose scalar-function registration, so the harness counts
rows the planner actually visits with a probe:

- **Non-deterministic, and argument-taking**: `probe(run_id) IS NOT NULL`
  registered via `RegisterScalarFunction`, **not**
  `RegisterDeterministicScalarFunction`. The third pass specified a
  zero-argument deterministic function, which SQLite may factor out of the loop
  and evaluate once — measuring nothing.
- **Plan-equivalence assertion**: the instrumented query's `EXPLAIN QUERY PLAN`
  must be byte-identical to the production query's, or the measurement describes
  a different query.

In production the bound is the closed set plus §14.3's absolute latency ceiling
and §7.1's enforced cancellation.

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
4. **One serialized `read.db` transaction:** `run` row, `run_stage` rows,
   run-level population rollups, `change` row, `last_projected_seq`, dirty-day
   marks. Atomic together, and they must stay in one database for that reason.
5. **Then** acknowledge the watermark in `intake.db` under its `source_seq <= ?
   AND removing = 0` guard (§4.3). Not in the same transaction — SQLite WAL gives
   no cross-file atomicity — and not needed there, because a crash between 4 and
   5 simply reprocesses an idempotent projection.
6. Projector publishes the invalidation carrying `<epoch>:<seq>`.
7. Separately, at lower priority: span/analytics projection into `telemetry.db`,
   and dirty-day bucket recompute.

Steps 1–6 are what list visibility waits on; step 7 never delays it. Today they
share one transaction, so list visibility waits on span files.

### 6.3 Repair: a rate bound, plus a projection floor

Repair does not need to be cheap; it needs to be **rate-limited and always making
progress**.

- A **fixed I/O budget** (configured entries/second), walking continuously and
  cycling, with a durable cursor in `projection_state`. Cost is **constant per
  unit time**, independent of history; what scales with history is *cycle time*
  (`H / rate`) — at 40,665 entries and 2,000/s, ~20 s. `readState` reports
  `lastSweepCompletedAt`.
- **Repair reconciles both directions.** "On disk but not projected" is only
  half of it. A **projected row whose journal is gone above the projection
  floor** — an operator `rm`, an abandoned restore, an unlink whose removal
  intent was lost — must also be resolved, by emitting `run.removed`. Without
  this, §11.4's "impossible" case is merely "unusual."
- **A projection floor stops a retention livelock.** `projection_state` records
  the floor below which runs are intentionally aged out of the projection
  (§11.4), and repair **skips** journals older than it.
- **An explicit resume overrides the floor.** `runner.ResumeFromTerminal`
  (`internal/runner/resume.go:152`) durably reopens an escalated or failed run,
  and that journal may be older than the 90-day window. A resumed run is
  therefore **re-admitted regardless of the floor**: the floor governs aging out
  *inactive* runs, and an intake watermark is authority to re-admit. Repair's
  floor-skip applies only to runs with no intake marker. The alternative —
  refusing to project a resumed old run — would make a human action invisible in
  the portal that prompted it, so it is rejected. Without this, journals
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

**Freshness policy is caller-selected, and serving labelled stale data is the
default.** The third pass returned an unconditional 503 above 30 s of lag. For an
operator portal that is the wrong default: during exactly the incident where lag
grows, honestly-labelled stale data is usually more useful than a blank page, and
a portal that goes dark when the system is struggling is a portal that is absent
when it is most needed.

| Caller | Behavior |
|---|---|
| Default (no freshness constraint) | **Serve, always**, with `lagSeconds`, `pendingIntake`, and `lastSweepCompletedAt`. The UI renders "stale by N" prominently. No lag ceiling applies |
| `maxLag=<duration>` or `If-Source-Applied` | Bounded freshness requested explicitly. If it cannot be met within the route budget, **503** `projection_lag_exceeded` |
| Operator-configured `strictFreshness: true` | Fail closed globally — the third pass's behavior, available for deployments that prefer it |

`readState` reports `pendingIntake`, `oldestPendingSourceAge`,
`lastSweepCompletedAt`, and `intakeWriteFailures` (§4.1), so "catching up" is
distinguishable from "stuck" without having to withhold the data. **Which of these
is the default is a product decision, recorded here alongside the 90-day window
(§11.4) rather than settled by engineering.** The recommendation is serve-stale.

### 6.5 Rebuild: a new epoch and a safe generation swap

Rebuild mints a **new epoch** and builds `read-<epoch>.db`.

The second pass said "build a temp file and rename," which is unsafe as written:
the `-wal` and `-shm` files cannot be atomically swapped with the main file by one
rename; on Unix existing pooled connections keep reading the old inode; on
Windows replacing an open database generally fails.

The sequence is therefore explicit:

1. Mint a fresh epoch ULID *E* (§4.2) and record `rebuildFromSeq` = the old
   epoch's current `change.seq`. Build `read-<E>.db` in bounded transactions; the
   current epoch stays open and readable throughout. Because intake lives in
   `intake.db` and is never rebuilt (§4.3), external writers keep recording
   watermarks correctly across the whole rebuild.
2. Validate *E* (schema version, row counts, differential spot-check).
3. **Barrier:** stop the commit loop, then catch *E* up from **two** sources:
   - every `run_id` appearing in the old epoch's `change` where
     `seq > rebuildFromSeq` (the old epoch's `change.seq` recorded in step 1); and
   - every pending `run_intake` marker, including `removing` markers.

   **Pending intake alone is not sufficient**, which is the third pass's gap:
   *E* reads run R at source seq 10; R advances to 11 while *E* builds; the old
   epoch — still live — projects 11 and *acknowledges R's marker*; the barrier
   then sees no pending marker for R and publishes *E* stale at 10, with
   `readState` unable to see it. Replaying from `rebuildFromSeq` closes that,
   which is why `min_change_seq` is pinned during a rebuild (§4.2).

   Then **assert that no run's `last_projected_seq` regresses** between the old
   epoch and *E*. A regression means the catch-up was incomplete and the swap
   must abort rather than publish.
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
    CostBounded   CostClass = "bounded"    // a supported combination (§5.7); zero journal opens
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
- **Deadline expiry must actually stop the work, and the mechanism exists.**
  Every request query uses `QueryContext`/`ExecContext`. `modernc.org/sqlite`
  wires context cancellation to `sqlite3_interrupt` (`interruptOnDone` in
  `sqlite.go:78`, used by `stmt.go:105`/`295` and `tx.go:71`), so a cancelled
  request aborts the in-flight statement rather than running to completion. An
  earlier pass of this document claimed the driver exposed no interrupt; that was
  wrong — it checked the exported API surface rather than driver behavior.
  **`SQLITE_INTERRUPT` maps to the 503 path, never to a partial 200.** §14.1
  asserts both the mapping and that no goroutine keeps working past its
  deadline.

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
    "intakeWriteFailures": 0,
    "lastSweepCompletedAt": "2026-07-29T18:11:47Z",
    "minChangeSeq": 868342,
    "completeness": "complete",
    "degraded": []
  }
}
```

`lagSeconds` and `pendingIntake` are **source-relative** (§4.1): a projection
sequence alone cannot reveal an append the projector has not discovered, so
freshness that ignores pending intake is not freshness. And because the journal
append and the intake write cannot be atomic, the honest bound is
`max(pendingIntake age, now − lastSweepCompletedAt)` — §4.1 states why, and
`intakeWriteFailures` makes the residual window observable rather than assumed
away.

**`completeness: "partial"` is narrow, and `budget_exhausted` is not one of its
reasons.** This is a correction: the second pass listed `budget_exhausted` as a
`partial` reason "with what was returned," which is a truncated list returning
200 — the silent-omission failure in a new costume.

| Situation | Response |
|---|---|
| An interrupted, truncated, or over-budget page — including `SQLITE_INTERRUPT` from a cancelled `QueryContext` (§7.1) | **503** + `Retry-After`. Never a 200 |
| A **named, described** missing partition — analytics not yet projected, a run root not yet swept | 200, `partial`, with the partition named **and an expiry expectation** |
| Projection lag, no freshness constraint requested | 200, `complete`, with `lagSeconds` — served, labelled (§6.4) |
| Projection lag exceeding a caller-requested `maxLag` / `If-Source-Applied` | **503**, `projection_lag_exceeded` (§6.4) |

`partial` reasons: `read_model_rebuilding`, `analytics_rebuilding`,
`sweep_incomplete` (with the roots not yet cycled). Each carries
`expectedCompleteIn`, because `partial` without an expiry becomes wallpaper.

Three states, never a fourth indefinite one: current; stale by a stated amount;
unavailable with a reason. No path can hang.

### 7.3 Cursors

Keyset only, never offset, encoding the ordering key `(started_at, run_id)`, the
schema version, the **epoch**, and a hash of the normalized filters. Existing
50/200 limits are retained deliberately — they are what the interface renders.

Every page belongs to the enumerated set of supported filter combinations (§5.7);
an unlisted combination is refused, not walked. There is no candidate loop.

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
- Three named conditions, not one generic `stale_cursor`: `epoch_changed`
  (cursor epoch ≠ current epoch — equality, §4.2), `feed_truncated`
  (`cursor.seq < projection_state.min_change_seq`, a persisted comparison against
  a defined retention floor, §4.2), and `schema_changed`.
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
| Status-pushdown heuristics + the false invariant comment | `readservice/runs.go:593–681` | Enumerated supported combinations over a maintained column (§5.4, §5.7) |
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
| 2 | Bounded lists **in work, not just in rows** | An enumerated closed set of supported filter combinations, each with a covering index; typed refusal otherwise (§5.7) | At 100k runs / 1M+ attempts, for **every combination in the set**: a **non-deterministic `probe(run_id)`** counts rows actually visited, bounded at `limit+1` (× k for merged scopes), with the instrumented query's plan asserted byte-identical to production's. Every combination the API can express is either in the set or returns `unsupported_filter_combination` — a test enumerates the API surface and fails on a gap. Zero journal opens, zero directory reads |
| 3 | Latency, stated as a reliability bar | Indexed keyset on a bounded working set; buckets (§5.2, §5.6, §11.4) | **Absolute targets (§14.12), not "whatever we measure."** ≤ 2× growth from 10k → 100k runs. Measured on slow-disk and low-core hardware as well as the reference host, reported separately with hardware |
| 4 | No fan-out per entity | One latest-outcome aggregate; three client primitives (§8.2, §10) | A 2,000-workflow page issues one aggregate request and zero per-workflow requests; harness fails on request growth with entity count. **Multi-gaggle fan-out has a tested ceiling** (§5.5) and exceeding it is a typed refusal, not an unbounded merge |
| 5 | Bounded behavior under **sustained mixed load** | Scoped invalidations after commit; coalescing; admission control (§7.1, §8, §9) | Not a 60-second burst: **sustained concurrent reads, scheduler writes, projection, SSE traffic, rebuild, restart, and retention together.** Unrelated views perform zero refreshes; per-family in-flight ≤ 1 and queued ≤ 1; zero aborts attributable to a newer event; **bounded queue depth** per class |
| 6 | Legible freshness, bounded honestly | `readState` bounded by pending intake **or** last sweep; three states; narrow `partial` (§4.1, §7.2) | Fault injection yields exactly one of current / stale-with-number / unavailable-with-reason. **An interrupted or over-budget page returns 503, never a 200 with `partial`** — including `SQLITE_INTERRUPT`. `partial` always names a partition and an expiry. `pendingIntake` is nonzero whenever an unconsumed watermark exists, **and a fixture that kills the writer between journal fsync and intake write asserts freshness falls back to `lastSweepCompletedAt` rather than reporting current** |
| 7 | No silent omission | Completeness from `projection_state`; authz as an indexed predicate (§5.3, §5.5) | Differential test per filter and cursor against the journal-derived reference under an injected clock. Injecting a dirty run / incomplete sweep yields `partial`. A test asserts no list applies an authz filter after `LIMIT` |
| 8 | Crash, restart, and **adverse-state** safety | One `read.db` transaction plus a guarded post-commit ack; epoch swap with `rebuildFromSeq` catch-up (§6.2, §6.5) | Kill at every boundary during project, rebuild, and **epoch swap**: previous epoch intact or resumed; no partial epoch readable. **A run that advances during a rebuild and is acknowledged by the old epoch is still caught up in the new one** (§6.5's exact scenario, as a fixture), and **no run's `last_projected_seq` regresses across the swap**. An external process writing intake across a swap loses nothing. Restart is O(active + pending). Plus **disk-full, read-only volume, corrupt store, and daemon↔standalone transitions** |
| 9 | Determinism | Pure `(prevRow, events) → nextRow`; buckets recomputed (§5.6, §10) | Rebuilding from the same journals produces byte-identical canonical rows; recomputing a day's bucket twice is identical |
| 10 | Single-run isolation | `useSingleRun` keyed by `(runId, fingerprint)` (§7.1, §8.2) | Run detail cost independent of H; an unchanged journal parsed **at most once** per fingerprint across summary + events + attempts |
| 11 | **Instance-log sequence uniqueness survives Wave 1.1** | Backward scan to the last *parseable, non-zero-`Seq`* event under the existing cross-process lock, with a byte budget and a full-recovery fallback (§17 Wave 1.1) | `TestInstanceLogConcurrentAppendsAllocateUniqueMonotonicSequence` (25 independent handles) and the sequential independent-writer test **retained and passing**, plus a bytes-read-per-append bound, plus **the existing NUL-cascade crash fixtures (#116) added to the sequence-allocation coverage** — a newline-terminated fill-only tail must not recover `seq=0` |
| 12 | **Absolute, falsifiable targets** | §14.12 | Numbers that can fail, revisable **once** in Wave 0 with explicit sign-off |

### 14.12 Absolute targets

"Budgets come from measured p99.9" can always pass by adopting whatever number
came out, so the bar carries numbers capable of failing. These are the initial
targets on the reference benchmark host at 100k runs / 1M+ stage attempts. **Wave
0 may revise them exactly once, with explicit sign-off recorded in this
document.**

| Dimension | Target |
|---|---|
| Bounded list, 50 rows — warm p99.9 | **≤ 250 ms** |
| Bounded list, 50 rows — cold p99.9 | **≤ 1.5 s** |
| Bounded list — hard server maximum (budget) | **1 s**, enforced by `context` + `WriteTimeout` |
| Single-run detail — warm p99.9 / hard max | **≤ 300 ms** / **2 s** |
| Aggregate, any window including all-time — warm p99.9 / hard max | **≤ 500 ms** / **3 s** |
| Admission queue depth per cost class | **≤ 2 × pool size**; beyond it, shed |
| Sustained event rate absorbed without shedding run-row projection | **≥ 50 events/s for ≥ 30 min** |
| Projection lag under that sustained rate — p99 | **≤ 2 s** |
| Repair sweep full-cycle time at 100k directories | **≤ 120 s** |
| `read.db` rebuild, 100k runs, local disk | **≤ 90 s** to list-serving readiness |
| `read.db` size at 100k runs | **≤ 250 MB** |
| WAL size, steady state, either store | **≤ 64 MB** (checkpointed) |
| Rows visited per bounded page (probe) | **≤ limit + 1** per gaggle query |

On the scale figures: 100k runs / 1M+ attempts are the diagnosis's working
numbers. The live instance is at 29,759 published runs after roughly six months,
so 100k is the right target and §11.4's retention position keeps the working set
below it. §16 confirms the curve.

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

**15.5 What when queryable state is behind?** **Serve, labelled** — that is the
default (§6.4), because a portal that goes dark during the incident that caused
the lag is absent when it is most needed. Bounded freshness is opt-in per request
(`maxLag`, `If-Source-Applied`) or per deployment (`strictFreshness`). The number
served is bounded by pending intake *or* the last completed sweep, whichever is
weaker (§4.1). Never silently substitute a slower authoritative path, and never
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
6. **Work assertions, not just row counts**: the non-deterministic
   `probe(run_id)` UDF for rows visited with a plan-equivalence check (§5.7);
   an enumeration of every filter combination the API can express, asserting each
   is either in the supported set or refused; observer seams for zero journal
   opens and zero request-path directory reads; queue-depth ceilings; and no
   goroutine continuing expensive work past its deadline, with `SQLITE_INTERRUPT`
   mapped to 503.
7. **State-boundary fixtures**, one per §18.1 finding: a NUL-cascade tail against
   instance-log sequence allocation; a rebuild in which a run advances and is
   acknowledged by the old epoch; an external process writing intake across an
   epoch swap; retention crashing between removal intent and unlink; a journal
   removed with no intent; a writer killed between journal fsync and intake
   write; and a cursor below `min_change_seq`.
8. **The differential oracle** (§14.7), per combination, per cursor, under an
   injected clock.
9. **Rebuild cost by store and by storage class**, including at least one network-
   or blob-backed mount, because §2.6's file-open cost decides §13.2 and no figure
   exists.
10. **Cold and warm p99.9 reported separately, with hardware**, against §14.12's
    absolute targets — which Wave 0 may revise **once**, with sign-off.

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
| 1.1 | **Bound the instance-log append without breaking sequence allocation.** Under the *existing* cross-process journal lock, scan backward from EOF (the bounded chunked pattern in `eventstream.go:612`) **until a line that, after NUL stripping, parses as an event with non-zero `Seq`** — not merely to the last newline. Impose a byte budget; **if the budget is exhausted, fall back to the full `readEvents` recovery path rather than allocating from zero.** Torn-tail detection comes from the same read | Removes 1.3 s and growing per journaled scheduling decision, under a lock, in the serving process | **Medium, not low.** The reread *is* the cross-process sequence allocator; `TestInstanceLogConcurrentAppendsAllocateUniqueMonotonicSequence` exists because of #530's "two events sharing seq:5", and per-handle in-memory sequence would reintroduce it. **And the last complete line is not necessarily the last event:** `readEventRecords` strips NUL crash-fill and `continue`s on a line that collapses to empty (`reader.go:449–454`), so the #116 cascade can leave a newline-terminated fill-only tail — reading to the last newline would recover `seq=0` and reallocate from 1, duplicating every sequence. Hence scan-to-valid-event with a budget and a full-recovery fallback. **Both independent-writer tests retained, plus a bytes-read-per-append bound, plus the existing NUL-cascade fixtures added to sequence-allocation coverage.** Rejected alternative: a sequence sidecar — a second file and a second recovery path. Rollback: revert the commit |
| 1.2 | Indexes on `runs`: gaggle-leading scoped indexes plus a global recency index (§5.5) | Removes full-scan + sort per page and the unindexed window function | Very low. Additive `CREATE INDEX` migration. Rollback: drop them |
| 1.3 | Split reader/writer handles; reader pool `MaxOpenConns=NumCPU`, `mode=ro`; `synchronous(NORMAL)` | Readers stop serializing behind each other and the writer | Low–medium. Uses WAL as already configured. Gate on §16.3's mixed-load run |
| 1.4 | Background active-run sampler off the request path, served from memory with its age reported. **No sample yet returns `read_model_rebuilding` with progress — never the synchronous scan.** Deliberately throwaway; Wave 2 replaces it | `/instance`, `/gaggles`, `/workflows` stop paying 4–17 s | Low. Corrected from the second pass, which kept the 17-second scan as the no-sample fallback and so preserved the exact failure on a cold daemon |
| 1.5 | `http.Server.WriteTimeout` + per-route `context.WithTimeout`; cancellation must stop the work | No request can hang | Low. Budgets generous here, tightened in Wave 4 from measured p99.9 |

**Exit:** Overview, Workflows, and the warnings widget load on the live instance.
Whether run-detail timeouts are resolved is the §2.3 experiment's answer, reported
either way.

### Wave 2 — Read model

`read.db` per §5.3; stored indexed phase; complete run rows; `change` with an
**opaque epoch** and a **defined `min_change_seq` floor** (§4.2); the **enumerated
closed set of supported filter combinations** with typed refusal, run-level
population rollups, and the non-deterministic `probe(run_id)` rows-visited
harness (§5.7); gaggle-leading indexes plus the
unrestricted fast path and the fan-out ceiling (§5.5); the filter registry; the
**store interface and backend-neutral conformance contract** (§13.2 — semantic
seam only, no maintained second backend); one latest-outcome aggregate. Delete the
candidate loop, status-pushdown heuristics, and the backwards terminal walk.
Transition per §6.6.

**Risk:** medium — the substantive change. Contained by: `read.db` is a new file;
cutover is a flag with the old path intact; the differential oracle gates it;
rollback is deleting a file and flipping a flag.
**Exit:** §14.2 holds — every supported combination has a covering index and a
probe-measured rows-visited bound of `limit+1`, every unlisted combination is
refused, zero journal opens at 100k runs. Wave 1.4's sampler deleted. §14.12's
absolute targets met.

### Wave 3 — Projector

`Reader`/`Intake`/`Projector` interfaces including `Removing`; **`intake.db` as a
stable never-rebuilt store** with the guarded post-commit acknowledgement (§4.3);
the projector as sole writer of projected facts with a **single serialized commit
loop**; the ordered removal protocol (intent → unlink → project → confirm) and
**bidirectional** repair; the O(active + pending) restart pass; rate-bounded
lock-free repair with a **projection floor** and the resume override (§6.3); the
**epoch rebuild with `rebuildFromSeq` catch-up and pool swap** (§6.5); §6.4's shed
order and caller-selected freshness policy. Delete request-path reconcile and
in-process `IngestRun`-on-finish.

**Risk:** medium. Contained by: the compile-time `Reader` boundary makes
"reads never write" structural; repair is rate-limited, so a bug is slow rather
than catastrophic; the old reconcile path is deleted only after the oracle is
green with it disabled; the epoch swap is tested by kill-at-every-boundary
including mid-swap.
**Exit:** §14.7, §14.8, §14.9 hold — including §6.5's acknowledged-during-rebuild
fixture and the no-`last_projected_seq`-regression assertion. No `.lock` file
created by a read path. `readservice` holds no writable handle to projected facts
(compile-time).

### Wave 4 — Read contract *(parallel to 2/3)*

`Route.Cost` + `Route.Budget`; `readState` with **source-relative** freshness;
**503 for interrupted or over-budget pages, `partial` only for named partitions
with an expiry**; admission control with shed-at-admission; **all request queries
on `QueryContext`/`ExecContext` so cancellation reaches `sqlite3_interrupt`, with
`SQLITE_INTERRUPT` mapped to 503** (§7.1); caller-selected freshness policy
(§6.4). Client renders `readState`.

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

**Risk:** low–medium. Buckets are recomputable, so a bug self-heals. **Two
product decisions need sign-off before this ships: the retention window (§11.4)
and the default freshness policy (§6.4) — serve-labelled-stale versus fail
closed.**
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
- **Four things are pulled early because they are cheap now and expensive later:**
  gaggle-leading indexes (1.2), the semantic store seam (2), the epoch (2) —
  retrofitting one into a deployed cursor format means every live client
  refetches — and the **enumerated combination set** (2), because a filter that
  ships without an index is a filter the UI already depends on.
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

### 18.1 Third pass → fourth pass (second review)

Ten state-boundary findings. All accepted; three contradicted claims this document
had asserted, and one of those was a factual error about the driver.

| Finding | Why it mattered | Resolution |
|---|---|---|
| **Wave 1.1 must scan back to a valid event, not the last complete line** | `readEventRecords` strips NUL crash-fill and `continue`s on a line that collapses to empty (`reader.go:449–454`), so the #116 cascade can leave a newline-terminated fill-only tail *after* the last valid event. Reading to the last newline then recovers `seq=0` and reallocates from 1 — duplicating every sequence in the journal. The third pass's "the last complete record carries the maximum" is not true for every crash shape this journal supports | §17 Wave 1.1 — scan backward to a line that parses with non-zero `Seq`, byte budget, **full-recovery fallback rather than allocating from zero**; NUL-cascade fixtures added to sequence coverage (§14.11) |
| **The epoch cannot live in the store being replaced** | `projection_state` is inside `read.db`. Standalone recovery rebuilds when it is absent and rollback permits deleting it, so a recovered counter restarts and recreates the exact collision the epoch prevents. The third pass's own `readState` example already showed a ULID while its prose said "monotonic value in `projection_state`" | §4.2 — an **opaque ULID minted per build**, independent of either store. **Equality semantics, not ordering**; any inequality is `epoch_changed` |
| **The rebuild barrier missed changes the old epoch had already acknowledged** | E reads run R at source seq 10; R advances to 11 while E builds; the **old epoch** projects 11 and *removes R's marker*; the barrier sees no pending marker and publishes E stale at 10, invisibly to `readState` | §6.5 — record `rebuildFromSeq` at rebuild start; under the barrier replay **every run in `change` after it, plus pending intake**; assert **no `last_projected_seq` regresses**, aborting the swap otherwise. §4.2 pins `min_change_seq` during a rebuild so those rows survive |
| **Durable intake cannot live inside the replaceable store** | Out-of-process writers writing `run_intake` inside `read.db` contradicts the sole-writer handle, races the swap (an external process can hold the old inode and write watermarks that are then lost), and on Windows blocks removal of the old file. Also: **SQLite WAL gives no atomic commit across attached databases**, so the third pass's "ack in the same transaction" was not achievable | §4.3 — **`intake.db`, stable and never rebuilt**, with WAL/`busy_timeout`/retry and stated ownership. Projected facts and `change` stay in **one `read.db` transaction**; the ack is a **guarded post-commit** operation, safe because projection is idempotent |
| **Retention removal is not representable by `Observed`** | If the projector consumed the marker before the unlink, it projected an ordinary row, acknowledged, and retention then unlinked with no surviving removal signal — a projected row outliving its journal, contradicting §11.4's "impossible" | §3.1 adds `Removing`; §4.3 defines **intent → unlink → project → confirm** with a `removing = 0` guard on the ordinary ack; §6.3 makes repair **bidirectional**, resolving projected rows whose journals are gone above the floor |
| **"No residual predicate" is not assertable from SQLite's plan** | `EXPLAIN QUERY PLAN` reports `SEARCH … USING INDEX idx (a=?)` while silently filtering `c=?`; it does not enumerate residual terms. One partial index per facet also does not cover *combinations*, and an unpinned "any stage has cost" query needs a join plus dedup | §5.7 rebuilt around an **enumerated closed set of supported filter combinations** with a typed `unsupported_filter_combination` refusal, plus **run-level rollups** for unpinned stage populations so lists never join `run_stage`. The bound comes from a finite declared set, not a plan property |
| **`probe()` as specified measured nothing** | A zero-argument *deterministic* function may be factored out of the loop and evaluated once | §5.7 — **non-deterministic `probe(run_id)`** via `RegisterScalarFunction`, plus a byte-identical plan-equivalence assertion against the production query |
| **Freshness cannot be perfectly source-relative** | Journal append and intake upsert are in different files and cannot be atomic. A crash, `SQLITE_BUSY`, disk-full, or read-only transition between them leaves `pendingIntake = 0` while the projection is behind — the false "current" §4.1 exists to prevent. And `ResumeFromTerminal` (`resume.go:152`) can reopen a journal older than the 90-day floor | §4.1 — the bound is **`max(pendingIntake age, now − lastSweepCompletedAt)`**, with a stated writer intake-failure policy and an `intakeWriteFailures` counter. §6.3 — **a resumed run overrides the floor**, since an intake marker is authority to re-admit and refusing would hide a human action from the portal that prompted it |
| **The change retention floor was undefined** | The third pass kept `feed_truncated` as a named condition while dropping the retention rule, leaving `change` growth and SSE resume unbounded | §4.2 — persisted `min_change_seq`, a pruning rule (10-minute window or 50,000 rows, whichever is larger), the exact condition `cursor.seq < min_change_seq`, and the rebuild pin |
| **The interrupt claim was factually wrong** | This document said `modernc.org/sqlite` exposes no interrupt. **It does** — `interruptOnDone` (`sqlite.go:78`) wires context cancellation to `sqlite3_interrupt`, used from `stmt.go:105`/`295` and `tx.go:71`. I had checked the exported API surface rather than driver behavior. This matters because it *supplies* the enforcement mechanism the document claimed to want but had none for | §7.1 — all request queries on `QueryContext`/`ExecContext`; **`SQLITE_INTERRUPT` maps to 503, never a partial 200**. (The separate claim that no `stmt_status` or progress handler is exposed was re-verified and stands) |
| **The acceptance bar stopped being falsifiable** | "Budgets come from measured p99.9" always passes by adopting whatever came out | §14.12 — **absolute targets** for warm/cold p99.9, hard maxima, queue depth, sustained event rate, lag, sweep cycle, rebuild time, store size, and WAL size. Wave 0 may revise them **once**, with sign-off |
| **Unconditional 503 above 30 s lag is wrong for an operator portal** | Honestly-labelled stale data usually beats a blank page during exactly the incident that caused the lag | §6.4 — **serve-labelled-stale is the default**; bounded freshness is opt-in per request (`maxLag`, `If-Source-Applied`) or per deployment (`strictFreshness`). Recorded as a product decision alongside the 90-day window |

### 18.2 Second pass → third pass (first review)

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

### 18.3 First pass → second pass

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
| Wave 1.1 reintroduces duplicate instance-log sequences | Scan-to-valid-event with a full-recovery fallback rather than allocating from zero; both independent-writer tests retained including the 25-handle concurrency test; **NUL-cascade (#116) fixtures added to sequence coverage**; a bytes-read-per-append bound; risk classified medium (§14.11) |
| Stored phase drifts from the journal | `reconstructPhase` survives as a differential oracle over the whole corpus (§14.7); lag is source-relative and stated |
| The epoch swap drops intake, misses an acknowledged change, or serves a stale inode | Intake lives outside the replaced store and is never rebuilt (§4.3); the barrier catches up from `rebuildFromSeq` **and** pending intake, then asserts no `last_projected_seq` regression before publishing; the whole reader pool closes before the pointer moves; kill-at-every-boundary includes mid-swap (§6.5, §14.8) |
| A new filter combination ships without an index and walks candidates | The set is closed and generated into the contract surface, so an unlisted combination is a typed refusal the UI cannot construct; a test enumerates every combination the API can express and fails on a gap; `probe(run_id)` bounds rows visited per combination (§5.7, §14.2) |
| `intake.db` becomes a contention or corruption point of its own | It is tiny, single-table, WAL, `busy_timeout`-armed, and fully rebuildable by a repair sweep — losing it costs one sweep cycle of discovery latency, never correctness (§4.1, §4.3) |
| Repair's rate budget is too low and a restored run stays invisible | `lastSweepCompletedAt` is in every response; the budget is configurable; the watermark protocol covers every non-exceptional case |
| Two stores diverge | `read.db` is journal-derived and wholly rebuildable; rebuild must produce byte-identical rows (§14.9) |
| `partial` becomes permanently lit | It is narrow by construction (§7.2 — never budget exhaustion), always names a partition, and always carries an expiry; a test asserts a healthy instance reads `complete` |
| Nine waves stalls halfway | Wave 1 is independent of the rest; every wave ends green and shippable with a number |
| 90-day fidelity window is wrong for someone | A recommendation with its consequence and its journal-retention relationship stated (§11.4), configurable, gated on product sign-off before Wave 6 |
| Tier-3 rebuild cost over a blob mount is worse than assumed | Measured in Wave 0 (§16.8) before §13.2 depends on it |
