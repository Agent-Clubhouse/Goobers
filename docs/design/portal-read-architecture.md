# Portal read architecture — a rethink

> **Status:** Design proposal, second pass. Responds to
> `Goobers-Reviews/2026-07-29_portal-architecture-findings.md` (the diagnosis) and
> supersedes [`unified-index-backed-run-reads.md`](unified-index-backed-run-reads.md)
> (#1883), keeping its read-projection conclusion and replacing the parts it left
> open: ordering, the writer set, request budgeting, in-process isolation,
> topology, authorization, and the hosted shape.
>
> **Not committed. Nothing filed.**
>
> **Changes from the first pass**, after self-review (§18 records why):
> the separate durable change-feed file is **cut** — ordering comes from the read
> model's own commit sequence; the store is **split in two** so cold start is
> gated on 191 MB rather than 2.5 GB; the repair scan is re-specified as a
> **rate bound, not a scan bound**, because the first pass's mtime watermark
> provably never short-circuits on a busy instance; and **authorization** enters
> the schema, which the first pass omitted entirely.

---

## 0. How to read this

§1 is the decision. §2 is measured evidence from the live instance — it changes
two of the diagnosis's conclusions and adds a defect the diagnosis did not have.
§3–§12 are the architecture. §13 is the cloud trajectory: what carries forward
unchanged and what is genuinely tier-3-only. §14 maps the diagnosis's acceptance
bar to mechanism *and* enforcing test. §15 answers its open questions. §16 is the
harness. §17 is the plan, with a risk and rollback posture per wave. §18 records
what the first pass got wrong, because that is itself evidence about this class
of problem.

The shape of the plan, up front: **Wave 1 is five small independent diffs that
relieve every reported symptom in days.** Waves 2–3 make it structural. Waves
4–6 make it extensible. Wave 7 is the only wave that changes deployment
topology, and it changes the store backend and the process layout — not the
model, not the contract, not the client.

---

## 1. The decision

Four changes, in dependency order.

1. **One read model, complete enough to answer without the journals — in its own
   small store.** Every list and aggregate the interface offers is answered from
   indexed rows with **zero journal opens**. Run phase becomes a **stored,
   indexed fact** rather than something reconstructed by replaying an event log.
   The run read model lives in a new, small `read.db` (tens of MB), separate from
   the existing 547 MB `telemetry.db`, so cold start is gated on 191 MB of run
   event logs rather than 2.5 GB including spans. Gaggle is the leading column of
   every ordering index, so authorization is a query predicate rather than a
   post-filter.

2. **One ordered sequence, and it is the read model's own commit sequence.** A
   `change` table written in the same transaction as the row it describes.
   Its `seq` is one number serving three jobs: the SSE cursor, the
   read-your-write token, and the freshness number in every response. No second
   durable log, no file format, no fsync-ordering question, no retention floor.
   §4 explains why the first pass's separate change-feed file was both
   unnecessary here and unworkable in the cloud.

3. **One projector, and reads never write.** A single component owns every write
   to projected facts. Discovery is a doorbell (an in-process queue, plus a
   narrow insert-only intake table for out-of-process writers), not a scan.
   Repair remains as a backstop, re-specified as a **fixed I/O budget per second
   that always makes progress** — because a bound that only holds when nothing is
   happening is not a bound.

4. **One read contract that states its own cost and its own freshness.** Every
   route declares a **cost class** and a **server-side budget** in
   `apicontract`. Every response carries a `readState` envelope. A request that
   cannot be answered within budget returns a bounded, honest, partial answer —
   or a fast 503 — never a hang and never a silent subset. Bounded worker pools
   per cost class stop an aggregate query or an execution burst from starving a
   list.

Two consequences worth stating up front:

- **"Zero timeouts" is a structural claim.** No request can end in an indefinite
  wait or a client-side abort, because every request has a server-enforced
  deadline, a declared bounded cost, and a response shape that can express
  "partial answer, N seconds stale." It does not mean every question is O(1) — it
  means every question either has a bounded query plan or is answered from a
  pre-aggregated bucket.
- **This is a net deletion.** §12 lists roughly 1,500 lines of accumulated
  mitigation — throttles, caches, skip conditions, pushdown heuristics, a second
  change detector, three topologies — that this removes rather than reorganizes.

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

### 2.1 Confirmed, and worse than stated: the active-run count is the primary symptom

17.2 s cold to count two live runs, on an idle host, against a client that aborts
at 10 s. `/v1/instance`, `/v1/gaggles`, and `/v1/gaggles/{g}/workflows` each
invoke it (`internal/readservice/inventory.go:332,380,512`), and
`ListRuns(LatestPerWorkflow)` invokes it again (`runs.go:366`). There is no cache
of any kind (`inventory.go:601`).

This alone explains the configuration-warnings widget never populating: its only
data source is `getInstance()` (`portal/src/configurationWarnings.ts`), which is
the 17-second call. It is not a widget bug.

### 2.2 New defect, not in the diagnosis: every instance-journal append re-reads the whole journal

`journal.InstanceLog.Append` (`internal/journal/instance.go:81`) takes the
journal lock, then calls `readEvents` over the *entire*
`scheduler/events.jsonl` to recompute the highest sequence — **1.30 s per append
at the current 324 MB**, growing without bound. The scheduler calls it from 13
sites (`internal/localscheduler/scheduler.go`: `trigger.fired`, `tick.skipped`,
`poll.shed`, error events) plus the claim ledger
(`internal/localscheduler/claim.go:554`).

This is a **write**-path defect that starves the **read** path, and it is the
best available explanation for the diagnosis's "critical observation" — that the
same page is fast one hour and unusable the next. The unrelated concurrent
activity is the scheduler, burning 1.3 s of CPU and I/O under a lock per
journaled scheduling decision, in the same process as every HTTP handler. The
scale harness's own comment already noticed the shape (`test/scale/main.go`:
"`journal.OpenInstanceLog.Append` re-reads the whole log per append (O(n²))")
— but only as an obstacle to *generating* fixtures, never as a production
finding. There is no open issue for it.

### 2.3 Refuted: run detail is not slow because of large event ledgers

The diagnosis (§9.8) and #1665 leave the mechanism unconfirmed and ask for
measurement first. Measured: the **largest** run event log in 40,665 runs is
**131 KB**; p99 is 40 KB. Parsing 131 KB of JSONL three times is single-digit
milliseconds. Redundant parsing is real (`GetRun` and `RunEvents` each call
`openRun`; stage selection can call it a third time) and worth fixing, but it
cannot produce a 10-second timeout.

**Run detail times out because it queues behind a starved process.** The
mechanism is head-of-line blocking: a single SQLite connection
(`SetMaxOpenConns(1)`, `rollup/db.go:61`) shared by every reader and writer, plus
1.3 s locked journal appends, plus 4–17 s active-count scans from concurrent
inventory requests, plus a 100 ms filesystem-polling ticker. The answer is
therefore **neither pagination nor deduplication** — it is connection
concurrency, admission control, and removing the two starving operations. This
closes #1665's open question.

### 2.4 Physical evidence that reads perform maintenance

Every one of the 40,665 run directories contains a `.lock` file — including all
10,906 that have no `run.yaml` and can never be ingested. Those locks were
created by `IngestRun` → `journal.WithPruneProtection` → `acquireJournalLock`
(`internal/journal/prune.go:51`), called from `reconcileIndex` **on the HTTP list
path** (`readservice/runs.go:921`), before failing to read an identity that does
not exist. The read path wrote 10,906 lock files into the journal tree.

### 2.5 The scale harness does not measure the failing surfaces

`test/scale/measure.go` times `ListRuns`, the Overview fan-out, and the full
status scan. It builds the read service with `minimalDefinitions()` — **no
gaggles and no workflows** — so it never calls `Instance()`, `Gaggles()`,
`Workflows()`, or `LatestPerWorkflow`. It never measures the 17-second active
count, never measures the Workflows page, never measures run detail, and never
applies concurrent load. §16 fixes it first, because without it we cannot tell a
fix from a coincidence.

### 2.6 What these numbers imply for store design

Two ratios drive §5's split:

- **List data is 191 MB; analytics data is 2,263 MB.** A 12× difference in
  rebuild cost between the data the product *is* and the data that decorates it.
- **The cost driver on rebuild is file opens, not bytes: 29,759 of them.** On
  local disk that is seconds. On any network- or blob-backed mount it is
  dominated by per-open latency, and I have no defensible figure for it —
  which is why §17 Wave 0 measures it rather than assuming.

---

## 3. Authority and layers

Four layers, each with one authority and one direction of dependency.

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
│           read.db  — run, run_stage, change, projection_state, buckets│
│           telemetry.db — spans, usage, analytics (ATTACHed)           │
│           the projector: the only writer of projected facts           │
└────────────────────────────────▲─────────────────────────────────────┘
                                 │ doorbell (intake) + read (journals)
┌────────────────────────────────┴─────────────────────────────────────┐
│ RECORD    internal/journal — authoritative, per run                  │
└──────────────────────────────────────────────────────────────────────┘
```

### 3.1 Three capabilities, three interfaces, enforced by types

The first pass said "reads never write" and left it as a rule. Make it a type
boundary:

```go
// internal/readmodel

// Reader is what SERVE receives. No write methods exist on it.
type Reader interface { /* queries only */ }

// Intake is the doorbell. Insert-only, one method, no reads.
// Held by journal writers so an out-of-process run is discovered
// without a scan. It cannot touch a projected fact.
type Intake interface { MarkDirty(runID string) error }

// Projector owns the only writable handle to projected facts.
type Projector interface { /* project, repair, rebuild */ }
```

`readservice.Local` holds a `Reader`. It becomes a compile error for a read path
to write, backfill, or repair — which is stronger than the test the first pass
proposed, and free.

### 3.2 Invariants

- **RECORD is authoritative.** Both stores can be deleted at any time and rebuilt
  from journals alone. This is a tier-1/2 statement: at tier 3, if Temporal
  history projects *into* the journal format, the journal is itself a
  projection, and recovery plans must not assume otherwise (§13.4).
- **The projector is the only writer of projected facts** (§3.1).
- **SERVE performs no I/O outside the read model**, except `single-run`-class
  routes which open exactly one journal, exactly once per source fingerprint.
- **VIEW issues no query whose count grows with the number of entities
  rendered.**

---

## 4. Ordering: the read model's commit sequence

The first pass introduced a separate durable change-feed file. **That is cut.**
It is worth explaining why at length, because the reasoning also resolves the
cloud case.

### 4.1 What ordering is actually for

An ordered record of change buys exactly two things:

1. **Ordering** — a cursor for live updates, a token for read-your-write, and a
   number for "how far behind is the display."
2. **Discovery** — the projector learning that a run exists without scanning
   40,665 directories.

The first pass conflated these into one durable segmented log with retention
floors and torn-record semantics. They are different problems with different
difficulty.

### 4.2 Ordering needs no new file

If the projector is the only writer of projected facts, then **the read model's
own commit sequence is already a durable, monotonic, ordered log** — and it is
written in the same transaction as the row it describes, which a separate file
can never be.

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

One row per projected transition, in the same transaction as the `run` row
update. `seq` then serves all three jobs with one number:

| Job | Value |
|---|---|
| SSE cursor | `change.seq` |
| Read-your-write token (`appliedSeq`) | `change.seq` |
| Freshness in `readState` | `change.seq` + its `at` |

This is *strictly better* than the first pass, not merely simpler:
`appliedSeq` and the SSE cursor become the **same number**, so "has the
projection caught up to my write" is an integer comparison rather than a
correlation across two systems. The first pass had a feed sequence and a
projection sequence and had to relate them.

Retention: keep 10,000 rows or 24 hours, whichever is larger, pruned by the
projector. A client cursor below the floor gets an explicit snapshot
instruction — the same named condition the first pass had, minus the file.

### 4.3 Discovery is a doorbell, not a log

Three mechanisms, in cost order, each covering the case above it:

1. **In-process queue.** The daemon's runners hand `runID` to the projector over
   a channel on every query-visible transition. Covers essentially 100% of runs
   on a healthy instance. Cost: a channel send.
2. **`dirty_run` intake table** — insert-only, written via `Intake` (§3.1) by
   out-of-process writers (`goobers run` standing alone, the stalled-run
   terminalizer) on run *creation*, and read/drained only by the projector.
   SQLite WAL supports multi-process writers; this is one small insert, once per
   run. Covers cross-process discovery **without a feed file and without making
   repair load-bearing.**
3. **Startup pass over non-terminal rows.** After a crash or restart, the only
   runs whose journals can have advanced are those the read model records as
   non-terminal. That is ~10 rows, not 40,665. `SELECT run_id FROM run WHERE
   terminal = 0` → reproject each. **O(active), not O(H)** — this is the
   observation that makes restart cheap, and the first pass missed it.

Repair (§6.3) then returns to being what it should be: a backstop for file
restores, manual copies, and migrations. It is no longer the completeness
mechanism.

### 4.4 Why this is also the right cloud answer

The first pass concluded that a file-based feed breaks on shared storage
(multi-node atomic append, and a lock on a volume that must not be a coordination
mechanism) and would need replacing with a table at tier 3. That framing was
backwards. The file feed sat in the worst possible position: **unnecessary where
it worked** (one process, where the commit sequence suffices) and **unworkable
where it would have been necessary** (separate processes, no shared store).

Cutting it does not defer the cloud problem — it solves it. At tier 3 the read
model is a shared store (§13.2), so:

- the `change` table *is* the cross-replica ordered log, naturally;
- cursors are portable across replicas because the sequence is shared;
- discovery works across processes because writers insert into the same
  `dirty_run` table.

The only tier-3 addition is notification — `LISTEN/NOTIFY`, or a 1 Hz
`WHERE seq > cursor` poll, which is boring and adequate. **No new component, no
new format, at either tier.**

---

## 5. The read model

### 5.1 Two stores, ATTACHed

The first pass asserted one physical store, inheriting the prior design's
argument that a second "duplicates ingestion, reconciliation, retention, and
rebuild failure modes." That argument is weaker than it sounds: those are
*shared code paths*, not duplicated ones. §2.6's ratios point the other way.

| Store | Contents | Size at current scale | Rebuild input | Criticality |
|---|---|---|---|---|
| **`read.db`** (new) | `run`, `run_stage`, `change`, `dirty_run`, `projection_state`, day buckets | tens of MB | **191 MB** (`run.yaml` + `events.jsonl`) | The product. Lists, Overview, Workflows, run headers |
| **`telemetry.db`** (existing) | spans, span events, usage, agent invocations, curation, scheduler events, error signatures | **547 MB** | **2,263 MB** (spans) | Analytics. Insight, cost, error signatures |

Both are opened on one connection via SQLite `ATTACH`, so cross-store joins are
ordinary SQL. That matters for the cloud port: at tier 3 the same queries run
against one database with two schemas, near-textually identical (§13.2).

What the split buys, concretely:

- **Cold start is gated on 191 MB, not 2.5 GB.** Lists become available in
  seconds; analytics fidelity arrives afterwards with `completeness: "partial"`
  declared until it does.
- **Two retention policies**, which they already need: 90 days of run rows
  (§11.4) versus whatever the analytics window is.
- **Shadow-generation rebuild collapses into "build a temp file and rename."**
  Same atomicity guarantee, a fraction of the machinery — because `read.db` is
  small enough to rebuild wholesale. The first pass specified a shadow generation
  inside a shared 547 MB file, which needed real bookkeeping.
- **Separate blast radius.** Corrupting the analytics store does not take down
  the run list.

### 5.2 Connection and durability

| Today | Change | Why |
|---|---|---|
| `SetMaxOpenConns(1)` (`rollup/db.go:61`) | **Two handles**: a writer (`MaxOpenConns=1`, `_txlock=immediate`) owned by the projector, and a **read-only pool** (`mode=ro`, `MaxOpenConns=NumCPU`) | WAL is configured then discarded. Today every reader serializes behind every reader *and* the writer, so the Overview's five concurrent requests have their queries serialized and one analytics aggregate blocks every list. Direct fix for §2.3 |
| `synchronous` defaults to FULL in WAL | `_pragma=synchronous(NORMAL)` | fsync per commit on data that is derived and rebuildable by design |
| `runs` has **no index** beyond its PK (`rollup/schema.go:11`) | Explicit indexes, asserted by `EXPLAIN QUERY PLAN` tests | Every other table has them. Today the "indexed" list path is a full scan plus sort, and `LatestWorkflowRunRefs` is an unindexed window function over all history (`query.go:278`) |
| Path fixed | `readModel.path` / `telemetry.path`, defaulting to today's locations | §13.1: an embedded single-file DB is not safe on a shared volume |

### 5.3 Shape

**`run` — one row per run, complete.** Every field `RunSummary` needs, so a list
row is a row read:

- *Identity:* run id, **gaggle**, workflow name/version/digest, goober digest,
  trigger kind/ref, `started_at`.
- *Execution state:* **`phase`** (stored, §5.4), `terminal`, `current_stage`,
  `finished_at`, `last_activity_at`, `last_seq`, repass / retry / policy-retry /
  infra-retry counts.
- *Terminal business outcome:* verdict, target — what run detail already derives
  (`RunOutcome`, `runs.go:174`).
- *Semantic disposition:* `produced` / `no-work` / `unknown`. Column and index
  reserved now so #1429's eventual definition and #1439's filter are
  index-pushable rather than a new scan.

`duration` is **not** stored for a running run; it is computed at query time from
`started_at` and now, so a quiet in-flight run ages exactly as the
journal-derived path makes it age.

**`run_stage` — one row per (run, stage).** Journal-derived facts only: stage
presence, whether a completed measurement exists, latest and terminal attempt
outcome facts for the existing stage-scoped vocabulary.

Note what is *not* here. The first pass put telemetry population flags (token,
premium, cost, retry-waste) on `run_stage` as denormalized booleans. That is the
extensibility trap reappearing in the schema: every new population filter becomes
a column plus a migration plus a backfill. Those facts already live in
`stage_usage`, which has a primary key. **Serve them by an indexed join across
the ATTACHed stores; denormalize only where a query plan proves it necessary.**
Population filters remain telemetry-gated and keep their existing
`ErrTelemetryUnavailable` behavior.

**`projection_state` — what makes completeness establishable.** Schema version;
per-run `last_projected_seq` and source fingerprint (size + mtime + inode of
`events.jsonl`); the repair sweep's cursor and last-completed-cycle timestamp;
rebuild state and last error.

This replaces `IndexedRunIDs()` (`query.go:329`), which materializes every run id
in history into a Go map on the request path. More importantly it is what lets a
read *establish* completeness rather than assume it: an absent row is no longer
treated as proof that a run does not exist.

### 5.4 Run phase becomes a stored fact

The highest-leverage change in the document.

Today phase is reconstructed by replaying a run's entire event log
(`journal.Reader.Phase` → `Events()` → `reconstructPhase`,
`internal/journal/reader.go:77`) — *including* in the hot path that counts live
runs across all history. Measured: **17.2 s to answer "2."**

Stored, the active-run count becomes

```sql
SELECT gaggle, workflow, COUNT(*) FROM run WHERE phase = 'running' GROUP BY 1, 2
```

— one indexed query, single-digit milliseconds, invariant to history.

The objection is staleness. Three answers:

1. **Lag is bounded and stated.** Every response carries `appliedSeq` and
   `lagSeconds` (§7.2). "The display is behind" becomes one observable condition
   with a number.
2. **The status-pushdown bug class is structurally removed.** Today `status` is
   written *only* on finish (`ingest.go:108`), backfill only touches runs
   *absent* from the index (`runs.go:905`), and the finish-time update is
   best-effort with a swallowed error (`runs.go:921`). A row can keep an empty
   status permanently, and a terminal-phase filter then excludes that run
   forever. The projector writes phase on **every** transition and records
   `last_projected_seq`; a run whose journal has advanced past it is *detectably*
   dirty, not silently miscategorised.
3. **The journal path survives as the oracle.** `reconstructPhase` stays, used by
   the differential tests (§14.7) and the explicit `--authoritative` CLI mode
   (§11.3). We assert equality between stored and reconstructed phase across the
   fixture corpus rather than asserting it in a comment — the diagnosis found
   three comments on `main` asserting invariants that do not hold.

### 5.5 Authorization is a query predicate, never a post-filter

The first pass never mentioned authorization. This is the gap that bites hardest
later, because retrofitting an authz dimension into ordering indexes after twenty
queries exist is expensive.

Once a list is "one indexed query returning `limit+1` rows," scoping has to
happen *inside* that query:

- **Post-filtering after `LIMIT` silently omits rows** — exactly the diagnosis's
  §5.6 silent-omission failure the whole design exists to prevent.
- **Filtering before `LIMIT` without an index** reintroduces the scan.

So: **`gaggle` is the leading column of every ordering index.**

```sql
CREATE INDEX idx_run_gaggle_recency  ON run(gaggle, started_at DESC, run_id);
CREATE INDEX idx_run_gaggle_wf       ON run(gaggle, workflow, started_at DESC, run_id);
CREATE INDEX idx_run_gaggle_phase    ON run(gaggle, phase, started_at DESC, run_id);
```

A principal's authorized scope is a set of gaggles, known and small. A
multi-gaggle list is served as **k keyset queries merged in memory**, bounded by
`k × (limit+1)` rows where k is the number of gaggles the principal can see —
not by a scan over an unindexed ordering. A single-gaggle list, which is the
common case, is one seek.

Rule, tested: **every list query's `WHERE` clause contains the authorization
predicate, and the plan uses an index whose leading column is `gaggle`.** This
holds today (`AllowAll`) and continues to hold when #644's RBAC lands, without
schema change.

### 5.6 Aggregate buckets: recompute, don't delta

Day buckets per `(gaggle, workflow, phase, outcome, disposition)` are what make
even an all-time Insight query a bounded number of rows rather than a scan of all
matching history. Without them "zero timeouts" does not hold on the analytics
surface.

The first pass called them "aggregate bucket deltas" and left the hard part
unspecified. Reversible deltas require storing each run's prior contribution and
subtracting it on reprojection — fiddly and easy to get wrong.

**Instead: buckets are recomputed, not accumulated.** When a run in day *D*
changes, mark *D* dirty; the projector recomputes day *D*'s buckets by
aggregating that day's indexed `run` rows. Recomputing one day is bounded by runs
in a day (tens to hundreds), and it is **idempotent** — which serves the
determinism property (§14.9) for free rather than requiring a separate argument.
Monthly rollups recompute from dailies on the same rule.

---

## 6. The projector

### 6.1 Position

One long-lived component, `internal/readmodel/projector`, holding the sole
writable handle to projected facts. It runs in the daemon as a bounded worker set
separate from the HTTP serving pool (§9), in the standalone portal identically
(§11.2), and — at tier 3 — as its own leader-elected process with no model
change, because it is driven by a table rather than by an in-process call from
the writer (§13.3).

Today the coupling is the opposite: `cmd/goobers/runnerwiring.go` and
`cmd/goobers/daemon.go` call `IngestRun` from the writer, in-process, after the
run finishes.

### 6.2 Normal path

1. Writer appends and fsyncs the authoritative journal record.
2. Writer rings the doorbell — channel send in-process; `Intake.MarkDirty` on run
   creation from out-of-process (§4.3).
3. Projector reads that run's event tail from `last_projected_seq`.
4. **One transaction:** `run` row, `run_stage` rows, `change` row,
   `last_projected_seq`, dirty-day marks. Committed atomically.
5. Projector publishes the invalidation carrying the committed `change.seq`.
6. Separately and at lower priority: span/analytics projection into
   `telemetry.db`, and dirty-day bucket recompute.

Duplicate delivery is harmless: `last_projected_seq` makes application
idempotent. Steps 1–5 are what list visibility waits on; step 6 never delays it.
Today they share one transaction, so a run's list visibility waits on its span
files.

### 6.3 Repair: a rate bound, not a scan bound

**This is the first pass's clearest error, and it is worth being explicit about
because it is the diagnosed pattern reappearing.** The first pass claimed repair
was bounded because a run root whose mtime has not advanced cannot hold anything
new and is skipped without a `ReadDir`. That is the current code's reasoning
(`readservice/runs.go:826–839`), and it is wrong in the only case that matters:
**every new run bumps its parent's mtime**, so on a live instance the root is
always dirty, the watermark never short-circuits, and repair does a
40,665-entry `ReadDir` every pass. A bound that holds only when nothing is
happening is not a bound. The first pass reproduced the exact failure mode the
diagnosis was written about, inside a document claiming to enforce against it.

The correct framing: **repair does not need to be cheap, it needs to be
rate-limited and always making progress.**

- The sweep has a **fixed I/O budget** (a configured entries-per-second, default
  sized so it is invisible next to real work). It walks continuously, cycling,
  with a durable cursor in `projection_state`.
- Cost is therefore **constant per unit time**, independent of history. What
  scales with history is *cycle time* — `H / rate`. At 40,665 entries and a
  2,000/s budget, a full cycle is ~20 seconds. That is a completeness *latency*,
  and `readState` reports it: `lastSweepCompletedAt`.
- Unpublished directories are remembered as such, keyed by directory mtime, so
  the 10,906 with no `run.yaml` cost one stat per cycle rather than an ingest
  attempt. Writing `run.yaml` bumps the directory mtime, so promotion is
  detected.
- **Repair never takes a journal lock.** Prune coordination inverts: retention
  marks its reservation and emits `run.removed`; the projector drops the rows; a
  mid-prune `ENOENT` is treated as removal, not an error. This is what stops the
  read path from ever writing a `.lock` file again (§2.4).
- Cancellation or restart resumes from the durable cursor.

Because §4.3's three discovery mechanisms cover every writer, repair is a
backstop for restores, manual copies, and migrations — not the completeness
mechanism. Its cycle time being tens of seconds is therefore fine.

**Date-sharded run directories** (`runs/<date>/<id>/`) would turn cycle time into
`O(runs since the last cycle)` by making prior shards genuinely immutable. It is
deliberately **not** in this plan: it is a layout change touching `FindRunDir`,
`RunDirs`, retention, prune, and a 40,665-directory migration, and the rate bound
makes it unnecessary at tier 1/2. It becomes worth revisiting at tier 3, where
directory `LIST` over a blob mount is slow *and metered* (§13.5).

### 6.4 Overload: a stated ceiling, and a shed order

The first pass had one writer and no policy for sustained overload, so lag would
grow and `readState` would faithfully report a number that grows forever —
honest and useless.

**Shed order**, most-shed first: bucket recompute → span/analytics projection →
repair sweep. Run-row projection is never shed.

**Lag ceiling.** Below `lagCeiling` (default 30 s), affected surfaces are
`stale` with a number. Above it, they are **`unavailable` with reason
`projection_lag_exceeded`** rather than presenting ever-staler data as merely
stale. `readState` also reports `oldestDirtyAge`, so "catching up" is
distinguishable from "stuck" — which is the same distinction §7.2 makes for
`partial`.

### 6.5 Rebuild

`read.db` rebuilds wholesale into a temp file and renames — atomic, resumable by
restart, and simple because the store is small (§5.1). `telemetry.db` rebuilds
independently and lists never wait on it. Journal updates committed during a
rebuild are replayed by per-run sequence before the rename.

Rebuild is an **availability metric with a budget**, measured against the real
input split: 191 MB / 29,759 files for `read.db`; 2,263 MB / 70,425 files for
`telemetry.db`. `goobers telemetry --rebuild` remains the operator entry point
and gains `--read-model` / `--analytics` scoping.

### 6.6 The one-time transition

The first pass described steady-state rebuild but not the migration on a live
instance with 29,759 runs and a 547 MB store. That is what will actually hurt, so
it is specified as **additive and revertible**:

1. Ship `read.db` construction alongside the existing store. Nothing reads it.
2. Build it by rebuild-from-journals on first start (191 MB — Wave 0 measures the
   wall time; expected tens of seconds on local disk).
3. Cut reads over behind a config flag, defaulting on, with the old path intact.
4. Only in a later release, drop the now-unused `runs` table from
   `telemetry.db`.

Rollback at any point before step 4 is: flip the flag, or delete `read.db` and
revert the binary. No journal is touched at any step.

---

## 7. The read contract

### 7.1 Cost class and budget, declared in the contract

```go
type CostClass string

const (
    // ≤ limit+1 projection rows after indexed filtering. Zero journal opens.
    CostBounded   CostClass = "bounded"
    // Exactly one run's journal, parsed at most once per source fingerprint.
    CostSingleRun CostClass = "single-run"
    // Pre-aggregated buckets; bounded bucket count for any window, all-time included.
    CostAggregate CostClass = "aggregate"
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

Three classes, not the first pass's four: `stream` was not a cost class but a
different lifecycle, and SSE is budgeted per write (`defaultEventWriteTimeout`)
rather than per request.

A route without a class and a non-zero budget fails a contract test. This is how
the diagnosis's §8.1 — "no path is unclassified, and the classification is
enforced rather than documented" — becomes true.

Budgets are set from Wave 0's measured p99, not by taste, and enforced by
`context.WithTimeout` in the router *and* by `http.Server.WriteTimeout`, which is
currently unset (`internal/httpapi/server.go` sets only `ReadHeaderTimeout`).
Two rules:

- **Every server budget is strictly below the client's 10 s abort**, which
  becomes a backstop rather than the only bound.
- **Prefer shedding at admission over accepting and timing out.** Queue wait
  counts against the budget, so a saturated class would otherwise accept work it
  cannot finish, burn it, and return nothing. A fast 503 with `Retry-After` is
  cheaper for both sides.

### 7.2 The `readState` envelope

Added as a new top-level field on every read response — additive, so no existing
field, ordering, or cursor semantic changes:

```json
{
  "runs": [ ... ],
  "nextCursor": "…",
  "readState": {
    "appliedSeq": 918342,
    "observedAt": "2026-07-29T18:12:03Z",
    "lagSeconds": 0.4,
    "oldestDirtyAge": 0,
    "lastSweepCompletedAt": "2026-07-29T18:11:47Z",
    "completeness": "complete",
    "degraded": []
  }
}
```

`appliedSeq` is `change.seq` (§4.2) — one identifier, not the first pass's three
(`generation` + schema version + seq). Schema version already lives in the
cursor, where it belongs.

`completeness` is `"complete"` or `"partial"`. `"partial"` **always** carries at
least one `degraded` entry naming the reason, what is missing, **and an expiry
expectation** — the first pass's weakness was that `partial` could stay
permanently lit and stop being read. `{"reason": "analytics_rebuilding",
"detail": "spans 41% projected", "expectedCompleteIn": "PT4M"}` is
actionable; `partial` alone is wallpaper.

Reasons: `read_model_rebuilding`, `analytics_rebuilding`, `sweep_incomplete`
(with the roots not yet cycled), `projection_lag_exceeded`, `budget_exhausted`
(with what was returned).

Three states, never a fourth indefinite one:

| State | Response | Interface |
|---|---|---|
| Current | 200, `complete`, `lagSeconds` ≈ 0 | Live |
| Stale by a stated amount | 200, `lagSeconds: N` and/or `partial` + reason + expiry | "Stale by N s — <reason>" |
| Unavailable | 503 + `Retry-After` + `degraded` | "Unavailable — <reason>", retry offered |

There is no fourth case, because no path can hang: every handler has a deadline,
and deadline exhaustion produces the third row with reason `budget_exhausted`.

**A read never silently omits.** If completeness cannot be established — a dirty
run in scope, a sweep cycle not yet complete — the response is `partial` with the
reason. Verified by differential testing against the journals (§14.7).

### 7.3 Cursors

Keyset only, never offset, encoding the ordering key `(started_at, run_id)`, the
schema version, and a hash of the normalized filters, so a cursor cannot be
replayed against a different query or schema. Existing 50/200 limits are retained
**deliberately** — they are what the interface renders, not incident residue.

A query reads at most `limit + 1` result rows after indexed filtering (times k
gaggles for a multi-gaggle scope, §5.5). There is no candidate loop. Today
`listRunsIndexed` fetches 100-row candidate pages and hydrates each until it
accumulates `limit` matches (`runs.go:771–815`) — unbounded in journal opens when
the residual filter is selective. That loop is deleted.

### 7.4 Read-your-write

`?minAppliedSeq=N` (or `If-Read-State: <schemaVersion>:<seq>`) on any read. The
server waits, up to a fraction of the route budget, for the projection to reach
N, then answers, stating in `readState` whether it did. A mutation returns the
`change.seq` it committed at.

Because §4.2 makes the projection sequence and the cursor the same number, this
is an integer comparison against a value the client already holds — which makes
"is my action visible yet" definitively answerable rather than probabilistic.

---

## 8. Live updates

### 8.1 Server

Delete `internal/httpapi/eventstream.go`'s change detection: the 100 ms ticker,
the 5-second sweep that stats every historical run it has ever seen, per-run
offset/digest state retained for all history, the in-memory `history` ring, and
the random per-process session id (`newEventSession`, `eventstream.go:240`).
Replace with a tail of the `change` table.

- **Cursor = `<schemaVersion>:<change.seq>`.** Durable, process-independent,
  portable across replicas.
- A cursor below the retention floor gets `feed_truncated` and a snapshot
  instruction; a cursor from a different schema version gets `schema_changed`.
  Both are named conditions, not a generic `stale_cursor`.
- Invalidations are published **after** the transaction that produced the
  `change` row, carrying its `seq`, so "refetch" and "the data is there" cannot
  be out of order. Today the projection updates on run *finish* while the stream
  discovers change by polling the filesystem: two mechanisms with different
  latency, completeness, and failure modes, which is why in-flight runs are
  visible to one and not the other.
- Heartbeats keep 15 s and become contractual: a client missing two consecutive
  heartbeats treats the stream as dead. Fixes #1711.

### 8.2 Client

Three primitives, and they become the *only* way a page fetches data:

- **`useBoundedList`** — keyset pagination over a client-side ordered window. A
  scoped invalidation **patches or prepends within the loaded window**; it never
  resets pagination. Pagination resets only on a filter/scope change or explicit
  retry. Fixes #1713 directly: today `runsHistory.ts:178` calls `refresh()` on
  any matching `run` invalidation, and `refresh()` resets `cursor: undefined`
  (`runsHistory.ts:102`), discarding everything the operator paged in. Responses
  are applied by `appliedSeq`, so a response older than one already applied is
  discarded — which is what makes non-monotonic reads across replicas safe
  (§13.2).
- **`useSingleRun`** — one loader per `(runId, sourceFingerprint)`, shared by the
  summary, event ledger, and stage attempts, so `getRun` and `listRunEvents` stop
  independently reparsing the same bytes (`runDetailData.ts:149`).
- **`useAggregate`** — bucket-backed, window-parameterized.

All three share one coalescing rule — **at most one in-flight and one queued
refresh per query family, and a useful in-flight request is never aborted merely
because a newer event arrived** — and one freshness rule: they render
`readState`, not connection state. Today `LiveFreshness` (`liveData.tsx:24`)
describes the *SSE connection*, which is not how current the data is, and is why
an operator cannot tell "slow" from "broken."

The 30-second `DATA_CACHE_TTL_MS` (`dataCache.ts:3`) drops to a short coherence
window: with an ordered sequence and scoped invalidations the cache no longer
needs a TTL to bound staleness, and a 30 s TTL is flatly incompatible with
read-your-write.

---

## 9. Isolation inside the daemon

The diagnosis asks (§9.2) whether the daemon is one service or several colocated
ones. **Keep one process for V1 with explicit internal contracts.** Splitting
processes now buys isolation obtainable more cheaply, and §4.4 means the split is
later a deployment change rather than a redesign.

1. **Bounded worker pools per cost class.** Bounded gets high concurrency,
   aggregate gets 2, single-run a middle value. Overflow returns a fast 503 with
   `Retry-After`. An analytics aggregate can no longer block a list, and the
   Overview's five queries can finally run concurrently.
2. **A separate bounded budget for the projector**, plus §6.4's shed order, so a
   projection burst cannot starve serving and vice versa.
3. **The two starving operations removed**: the 1.3 s instance-journal append
   (§2.2) and the 4–17 s active-run scan (§2.1). Wave 1, ahead of any
   architecture, because they are the measured cause of the variance.
4. **A read-only connection pool** (§5.2).

Together these are the answer to the run-detail timeout, which is not a
run-detail problem (§2.3).

---

## 10. Extensibility

The brief asks for an architecture that extends by model and view without
reintroducing timeouts. Three enforcement mechanisms, because a convention that
is not tested is a comment, and the diagnosis found three comments asserting
invariants that do not hold.

- **Projector units are declared.** Each declares the event types it consumes and
  the columns it owns, and is a pure function `(prevRow, events) → nextRow`.
  Adding a projected fact = one unit + one migration; write-through, discovery,
  repair, rebuild, invalidation, and determinism testing are inherited. Nothing
  new gets its own ad-hoc backfill scan.
- **Filters are declared with their index.** A `Filter{Name, Columns, Index}`
  registry, plus a test asserting (a) the contract's filter set equals the
  declared set and (b) each filter's query plan uses its declared index, with a
  leading `gaggle` column (§5.5). **A new filter cannot ship unindexed** — which
  is the specific way the current index fails: "adding a filter the index cannot
  express reintroduces an unbounded scan."
- **Views compose primitives.** §8.2's three hooks are the only data primitives,
  and the portal harness fails a page whose request count grows with the number
  of entities rendered. A new page cannot invent a new fan-out.

---

## 11. One topology (tiers 1–2)

The diagnosis finds three configurations of the same read library with different
capabilities, and notes the worst-performing one is what a new user meets first.

**11.1 In the daemon.** Projector in-process, both stores at configured paths,
admission control per §9.

**11.2 Standalone portal (`goobers dashboard`, no daemon).** Today this is
constructed with **no index at all** (`cmd/goobers/dashboard.go:394` passes no
`Telemetry`), so every list is a full scan of history — and it stands up a
*second* independent filesystem-polling change detector. New behavior: open
`read.db`; if absent or incompatible, **build it and say so**, with `readState`
reporting `read_model_rebuilding` and progress. `single-run` routes work
immediately; `bounded` routes become available on completion — which is now
seconds, because the split (§5.1) means 191 MB, not 2.5 GB. If the volume is
read-only and no model can be built, serve **explicitly degraded** — banner,
`single-run` only — never a silent O(H) scan. There is no configuration in which
the read service has no projection.

**11.3 CLI in-process.** Same read service, same stores. The journal-scanning
path survives as an explicit `--authoritative` flag, documented as O(total
history), used by operators verifying a suspicion and by §14.7's differential
tests. It is never a silent fallback. Today `ListRuns` selects it silently
whenever `Telemetry == nil` (`runs.go:413`).

**11.4 Queryable history.** The diagnosis (§9.4) notes nobody has stated how much
history should be queryable at full fidelity, which is why unbounded "all time"
queries exist. **Position: 90 days of full-fidelity `run` rows; aggregate buckets
beyond.** Run journals follow existing retention independently. Consequences: the
`run` table's working size is bounded by rate rather than instance age; all-time
analytics is answered from buckets in bounded time; a six-month-old run is
answerable in aggregate but may not be individually listable. That last point is
a product decision this design surfaces rather than hides. 90 days is a
recommendation to confirm, not a discovery.

---

## 12. What gets deleted

A design that only adds is a fifth layer of mitigation. This removes:

| Removed | Where | Because |
|---|---|---|
| Request-path reconciliation, throttle, in-memory watermarks | `readservice/runs.go:840–940`; `reconcileMu`/`lastReconcile`/`reconcileWatermarks` | Discovery is a doorbell; repair is a rate-bounded background sweep (§4.3, §6.3) |
| `IndexedRunIDs()` full materialization | `rollup/query.go:329` | Indexed lookups + `projection_state` (§5.3) |
| Status-pushdown heuristics + the false invariant comment | `readservice/runs.go:593–681` | Every filter pushed into SQL against a correctly-maintained column (§5.4) |
| The candidate/hydrate loop | `readservice/runs.go:771–815` | `limit+1` rows, zero journal opens (§7.3) |
| Backwards latest-terminal walk | `readservice/runs.go:450–495` | One indexed aggregate |
| Unindexed window function over all history | `rollup/query.go:278` | Indexed (§5.2) |
| Journal-walking active-run count on read paths | `localscheduler/reconcile.go` + 4 call sites | Stored, indexed phase (§5.4). Retained only as the no-projection cold-start primitive |
| Filesystem polling, 5 s sweep, per-run stream state, in-memory history, random session cursor | `httpapi/eventstream.go` (~1,000 lines) | `change` table tail (§8.1) |
| Whole-run delete-and-reinsert across 18 tables | `rollup/ingest.go:28,76` | Incremental tail projection (§6.2) |
| Journal-scan fallback as a silent default | `readservice/runs.go:413,710` | Explicit `--authoritative` only (§11.3) |
| Index-free standalone construction | `cmd/goobers/dashboard.go:394` | One topology (§11.2) |
| `SetMaxOpenConns(1)` | `rollup/db.go:61` | Reader pool (§5.2) |
| Full-journal re-read per instance-log append | `journal/instance.go:81` | In-memory seq (§2.2) |
| Pagination reset on live invalidation | `runsHistory.ts:94–102,178` | In-place patching (§8.2) |

Also deleted relative to the **first pass** of this design: the change-feed file
format, its segmentation, its retention floor, its torn-record rule, its lock
protocol; the shadow-generation rebuild bookkeeping; the `generation` identifier;
the fourth cost class; and `run_stage`'s denormalized population flags.

Kept deliberately: the three observer seams the diagnosis's Appendix B identifies
(`openRunObserver`, `reconcileScanObserver`/`reconcileInspectObserver`,
`journalReadObserver`), re-pointed at the new components. They are how §14's
properties become assertions.

---

## 13. Cloud trajectory

The requirement is that nothing built for tiers 1–2 is thrown away, and that the
tier-3 move changes deployment rather than design. It holds, with one honest
exception (§13.5).

### 13.1 What carries forward unchanged

| Artifact | Tier 3 |
|---|---|
| `run` / `run_stage` / `change` / `projection_state` schema | Same tables, different backend (§13.2) |
| Stored phase, incremental projection, day buckets by recompute | Unchanged |
| `gaggle`-leading indexes and the authz predicate rule (§5.5) | Unchanged — and *required*, since tier 3 is where multi-tenant RBAC lands |
| Cost classes, budgets, `readState`, admission control (§7, §9) | Unchanged, and more valuable (§13.6) |
| Cursor = `<schemaVersion>:<seq>`, read-your-write (§4.2, §7.4) | Unchanged — portable across replicas by construction |
| The three client primitives (§8.2) | Unchanged |
| `Reader` / `Intake` / `Projector` interfaces (§3.1) | Unchanged — this is what makes the process split a wiring change |

### 13.2 What changes: the store backend and the process layout

At tier 3 the read model is **shared, not per-replica.** Per-replica embedded
SQLite on node-local storage means every pod start, rolling deploy, and scale-out
pays a full rebuild whose cost is dominated by **29,759 file opens over a network
or blob mount** — a readiness-probe problem, not a latency problem, and it makes
horizontal scaling impossible. Sharing the store means API replicas are
stateless, start in seconds, and the "N replicas at N points of freshness"
problem *disappears* rather than being managed.

Consequences:

- **The SQL must be portable, and that is a Wave 2 constraint, not a Wave 7
  retrofit.** Writing twenty more queries against SQLite and *then* adding a
  store seam is the expensive order. Today's store is SQLite-shaped in ways that
  do not travel: `julianday()` in migrations v14/v15, `INSERT OR IGNORE`,
  `_pragma` DSN configuration, raw-DDL migration strings. Wave 2 introduces the
  seam and a conformance suite that runs against both backends.
- `ATTACH` (§5.1) becomes two schemas in one database — near-textually identical
  queries.
- `readState` stays but carries less weight: it reports the projector's lag
  rather than inter-replica divergence.

### 13.3 The projector becomes its own deployment

Unchanged code, leader-elected, single instance, writing the shared store. This
is possible *because* it is driven by a table and a doorbell rather than by an
in-process call from the writer — which is why §6.1 removes that coupling in
Wave 3 rather than Wave 7.

### 13.4 Authority changes, and recovery plans must know

If Temporal history projects *into* the journal format, the chain becomes
history → journal → read model, and the journal is itself a projection. "You can
always rebuild from journals" is a tier-1/2 statement. Nobody should plan a
tier-3 recovery path on it.

### 13.5 The one honest exception: repair on blob storage

§6.3's rate-bounded sweep is correct at tier 1/2 and adequate at tier 3, but
directory `LIST` over a blob mount is both slow and **metered** — a continuous
background sweep has a bill attached. Two tier-3 options:

- Drive repair from the authoritative writer set (enumerate workflow executions)
  rather than from directory listing. Preferred: the writer set is known.
- Adopt **date-sharded run directories** (§6.3), making prior shards immutable
  and skippable. Deferred deliberately at tier 1/2; genuinely worth it here.

Either way this is a repair-strategy swap behind the `Projector` interface, not a
model change.

### 13.6 What gets more valuable in the cluster

- Ingress has its own read timeout; a request that outlives it becomes a gateway
  timeout the app never observes. Declared budgets strictly below it are the only
  way the server stays the thing that decides.
- **Queue depth per cost class is a meaningful autoscaling signal.** CPU is not,
  for an I/O-bound read tier.
- `503` + `Retry-After` cooperates with client backoff rather than holding a
  connection.
- Cost classes eventually allow routing `aggregate` to its own deployment — a
  config change.
- Two SSE specifics to document: response buffering must be disabled at the
  ingress (`proxy_buffering off` / `X-Accel-Buffering: no`) or events never
  flush — a silent, common failure; and graceful drain must let clients
  reconnect, which cursor-based resume already handles.

---

## 14. The acceptance bar, and what enforces each item

The diagnosis's §8, with mechanism and enforcing test. A property with no test is
not claimed.

| # | Property | Mechanism | Enforced by |
|---|---|---|---|
| 1 | Classified cost | `Route.Cost` + `Route.Budget` required (§7.1) | Contract test: every route has a class and a non-zero budget; a `bounded` route firing `openRunObserver` fails |
| 2 | Bounded lists in fact | Complete rows; every filter in SQL; `limit+1` (§5.3, §7.3) | At 100k runs / 1M+ attempts: every filter reads ≤ `limit+1` rows (× k gaggles), opens **zero** journals, reads **zero** run directories. `EXPLAIN QUERY PLAN` asserts the intended index |
| 3 | Flat latency | Indexed keyset on a bounded working set; buckets for aggregates (§5.2, §5.6, §11.4) | Warm p95 ≤ 250 ms for a 50-row page; ≤ 2× growth from 10k → 100k runs. Cold and warm budgeted separately, hardware reported |
| 4 | No fan-out per entity | One latest-outcome aggregate; three client primitives (§8.2, §10) | A 2,000-workflow page issues one aggregate request and zero per-workflow requests; the harness fails on request growth with entity count |
| 5 | Bounded burst | Scoped invalidations after commit; one in-flight + one queued per family; no abort-on-newer (§8) | 100 events/s for 60 s with 500 ms injected latency: unrelated views perform **zero** refreshes; per-family in-flight ≤ 1, queued ≤ 1; zero aborts attributable to a newer event |
| 6 | Legible freshness | `readState` on every response; three states; `partial` carries an expiry expectation (§7.2) | Fault injection produces exactly one of current / stale-with-number / unavailable-with-reason — a test asserts no fourth outcome, budget exhaustion included, and that `partial` always carries a reason and expiry |
| 7 | No silent omission | Completeness from `projection_state`; authz as an indexed predicate, never a post-filter (§5.3, §5.5, §7.2) | Differential test: per filter and cursor fixture, the projected page equals the journal-derived reference page under the same injected clock. Injecting a dirty run / incomplete sweep must produce `partial`. A test asserts no list applies an authz filter after `LIMIT` |
| 8 | Crash and restart safety | One transaction per run including the `change` row and cursors; temp-file-and-rename rebuild (§6.2, §6.5) | Kill at every transaction boundary during project and rebuild: previous state intact or resumed; no partial store readable. Restart reprojects only non-terminal rows (§4.3) and a test asserts that count is O(active) |
| 9 | Determinism | Pure `(prevRow, events) → nextRow` units; buckets recomputed, not accumulated (§5.6, §10) | Rebuilding from the same journals produces byte-identical canonical rows; recomputing a day's bucket twice is identical |
| 10 | Single-run isolation | `useSingleRun` keyed by `(runId, fingerprint)`; `single-run` class (§7.1, §8.2) | Run detail cost independent of H; an unchanged journal parsed **at most once** per fingerprint across summary + events + attempts |

On the figures: 100k runs / 1M+ attempts / 100 events per second are the
diagnosis's working numbers. The live instance is at 29,759 published runs after
roughly six months, so 100k is the right target and §11.4's retention position is
what keeps the working set below it indefinitely. Wave 0 confirms the curve.

---

## 15. Answers to the open questions

**15.1 The writer set, and is it a choice?** Four writers: daemon runners, the
out-of-process `goobers run`, the stalled-run terminalizer
(`cmd/goobers/stalledruns.go:194`), and retention. `goobers run` **stays
first-class** — it is the offline story — but stops being a silent writer: it
rings the doorbell via `Intake` (§4.3). The current situation is subtler than
"two uncoordinated writers": `goobers run` takes the same `up.lock` and
*delegates* to a live daemon when one holds it (`cmd/goobers/run.go:99–102`), so
they are serialized — except that `up.lock` can go stale, as it is on the
measured instance right now. Either way the daemon has no in-memory knowledge of
runs written while it was down, which is the real reason reconciliation is
currently a correctness requirement. The doorbell removes that without removing
the writer.

**15.2 One service or several?** One process for V1 with explicit internal
contracts (§9). §4.4 and §13.3 make the split a deployment change.

**15.3 What is the durable unit of change?** A row in the `change` table, written
in the same transaction as the fact it describes (§4.2). The first pass answered
"a record in a new durable feed file"; that was over-built.

**15.4 How much history at full fidelity?** 90 days of run rows; buckets beyond
(§11.4). Recommended, not discovered.

**15.5 What when queryable state is behind?** Serve, with a number, and let the
caller opt into waiting (§7.4) — up to `lagCeiling`, above which say
`unavailable` rather than presenting ever-staler data as merely stale (§6.4).
Never silently substitute a slower authoritative path.

**15.6 Stored or derived phase?** **Stored** (§5.4). Deriving it measures 17.2 s
for an answer of 2.

**15.7 Where does "did any work happen" belong?** A **stored dimension**
(`disposition`) on the run row, so it can be excluded before pagination and
before aggregation — required for both scaling and consistent denominators.
Semantics stay with #1429; UX and denominator disclosure stay with #1439. This
design reserves the column and index so neither introduces a scan.

**15.8 The diagnostic surface's cost ceiling?** **Measured** (§2.3): largest
event log in 40,665 runs is 131 KB, p99 40 KB. Run detail is not intrinsically
expensive and needs neither pagination nor a new envelope. Deduplicate the parse
because it is free; fix the starvation because that is the cause.

**15.9 Which existing behaviors are requirements, which are accidents?**

| Behavior | Verdict |
|---|---|
| Client 10 s abort | **Accident as a primary bound; kept as a backstop** strictly above every server budget (§7.1) |
| 30 s client cache TTL | **Accident.** Reduced to a short coherence window (§8.2) |
| 2 s reconcile throttle | **Deleted** with the request-path reconcile (§6.3) |
| 100 ms change poll | **Deleted** with filesystem polling (§8.1) |
| 5 s idle sweep, 30 s idle-after | **Deleted** — polling artifacts |
| 50 / 200 page limits | **Requirement.** What the interface renders. Kept |
| 15 s heartbeat | **Requirement, promoted** to a liveness contract (§8.1) |
| `Promise.allSettled` per-phase Overview | **Transitional.** Correct given five independent queries (#1709); replaced by one bounded multi-phase query retaining per-group failure independence |

---

## 16. Conformance harness

§14's tests need a harness, and the existing one does not cover the failing
surfaces (§2.5). This is Wave 0 because nothing after it is measurable:

1. **Real definitions.** Replace `minimalDefinitions()` with a parameterizable
   inventory (target 2,000 workflows for §14.4).
2. **The missing surfaces.** `Instance()`, `Gaggles()`, `Workflows()`,
   `LatestPerWorkflow`, `GetRun` + `RunEvents` + `StageAttempts`.
3. **Concurrency.** Every symptom in the diagnosis is a contention symptom; the
   current harness is single-threaded.
4. **Pathologies from the live instance**, now that their shape is known: 27%
   unpublished directories with stray `.lock` files, a 324 MB instance journal,
   2.26 GB of spans against 191 MB of events.
5. **Observer assertions** wired to §14: zero journal opens on `bounded`; zero
   directory reads on any request path; change detection bounded by active work.
6. **The differential oracle** (§14.7), per filter, per cursor, under an injected
   clock.
7. **Store portability**: the read-model conformance suite runs against both
   backends from the moment the seam exists (§13.2).
8. **Rebuild cost by store and by storage class**, reported separately —
   including at least one network- or blob-backed mount, because §2.6's
   file-open cost is the number that decides §13.2 and nobody has it.
9. **Cold and warm reported separately, with hardware.**

Then run at the current instance's shape as 1×, and at 3× / 10×, and **publish
the baseline before changing anything.**

---

## 17. Plan

Nine waves including Wave 0. Each is independently valuable, independently
revertible, and ends with a measurement. **Only Wave 1 is urgent** — it is five
small diffs addressing the measured cause of every reported symptom.

Risk posture, applied throughout: no wave changes the journal format or the
directory layout; every wave that touches the read path ships behind a flag with
the prior path intact; and no wave's rollback requires touching a journal.

### Wave 0 — Make it measurable *(no product change)*

Extend the harness per §16. Publish the baseline at 1× / 3× / 10×.

**Risk:** none — test-only. **Rollback:** n/a.
**Exit:** every §2 number reproducible in-tree; every §14 property has a failing
test; the blob-mount rebuild figure exists.

### Wave 1 — Relief *(five small independent diffs; days)*

| # | Change | Expected effect | Risk / rollback |
|---|---|---|---|
| 1.1 | `InstanceLog.Append` keeps its sequence in memory; recovers once at open from EOF; torn-tail repair once at open | Removes 1.3 s (and growing) per scheduling journal write, under a lock, in the serving process. **The variance fix** | Low. Touches one function; the on-disk format is unchanged. Revert the commit |
| 1.2 | Indexes on `runs`: `(gaggle, started_at, run_id)`, `(gaggle, workflow, started_at, run_id)`, `(gaggle, status, started_at)` — gaggle-leading from the start (§5.5) | Removes full-scan + sort per page and the unindexed window function | Very low. Additive migration; `CREATE INDEX` only. Rollback: drop the indexes |
| 1.3 | Split reader/writer handles; reader pool `MaxOpenConns=NumCPU`, `mode=ro`; `synchronous(NORMAL)` | Readers stop serializing behind each other and the writer; Overview's five queries run concurrently | Low–medium. WAL is already configured; this uses it. Test under the §16.3 concurrency load. Revert the commit |
| 1.4 | Background active-run sampler: one scan every N seconds off the request path, served from memory with its age reported | `/instance`, `/gaggles`, `/workflows` stop paying 4–17 s. **Deliberately throwaway** — Wave 2 replaces it with an indexed query — but ~50 lines for the single biggest reported symptom | Low. Purely additive; the synchronous path remains as a fallback if the sampler has no sample yet |
| 1.5 | `http.Server.WriteTimeout` + per-route `context.WithTimeout` from provisional budgets | No request can hang; the server, not the client, bounds a request | Low. Budgets set generously here and tightened in Wave 4 from measured p99 |

**Exit:** Overview, Workflows, and the warnings widget load on the live instance.
Run-detail timeouts gone — §2.3 predicts they will be, since they are a
starvation symptom, and this validates or refutes that.

### Wave 2 — Read model

`read.db` with the §5.3 shape; **stored, indexed phase**; complete run rows;
`change` table; gaggle-leading indexes with query-plan tests; the declared-filter
registry (§10); the **store seam and portable SQL with a two-backend conformance
suite** (§13.2); one latest-outcome aggregate. Delete the candidate/hydrate loop,
the status-pushdown heuristics, and the backwards terminal walk. Transition per
§6.6.

**Risk:** medium — this is the substantive change. Contained by: `read.db` is a
new file (nothing existing is modified); the cutover is a flag with the old path
intact; §14.7's differential oracle gates it; rollback is deleting a file and
flipping a flag. The old `runs` table in `telemetry.db` is left alone until a
later release.
**Exit:** §14.2 holds — every filter, `limit+1` rows, **zero journal opens** at
100k runs. Wave 1.4's sampler deleted. §14.3's flat-latency budget met. The
conformance suite is green on both backends.

### Wave 3 — Projector

`Reader`/`Intake`/`Projector` interfaces (§3.1); the projector as sole writer;
the in-process doorbell plus `dirty_run` intake; the O(active) restart pass;
rate-bounded lock-free repair (§6.3); temp-file-and-rename rebuild; §6.4's shed
order and lag ceiling. Delete request-path reconcile and in-process
`IngestRun`-on-finish.

**Risk:** medium. Contained by: the compile-time `Reader` boundary makes the
"reads never write" claim structural rather than reviewed; repair is
rate-limited, so a bug is slow rather than catastrophic; the previous reconcile
path is deleted only after the differential oracle is green with it disabled.
**Exit:** §14.7, §14.8, §14.9 hold. No `.lock` file is ever created by a read
path. `readservice` holds no writable handle (compile-time).

### Wave 4 — Read contract *(can run parallel to 2/3)*

`Route.Cost` + `Route.Budget` required; `readState` on every response with
reason + expiry on `partial`; admission control per cost class; shed-at-admission
over accept-and-timeout; fast 503 with `Retry-After`. Client renders `readState`
and distinguishes data freshness from connection state.

**Risk:** low. Additive to the wire contract; touches the router and the
contract, not the projection. Rollback: remove the middleware.
**Exit:** §14.1, §14.6 hold. No fourth indefinite state under fault injection.

### Wave 5 — Live updates

Cursor = `<schemaVersion>:<change.seq>`; delete filesystem polling, the sweep,
per-run stream state, in-memory history, session identity. `useBoundedList` with
in-place patching (#1713); heartbeat watchdog (#1711); coalescing; stage attempts
refresh on live runs (#1714).

**Risk:** medium on the client, low on the server. Contained by: the server side
is a smaller, simpler component than what it replaces; the client primitives are
introduced page by page rather than in one cutover.
**Exit:** §14.4, §14.5 hold. #1711, #1713, #1714 closed with tests.

### Wave 6 — Aggregates and retention

Day and month buckets by recompute (§5.6); the 90-day full-fidelity window
(§11.4); all-time analytics answered from buckets.

**Risk:** low–medium. Buckets are derived and recomputable, so a bug is
self-healing on the next recompute. The retention window is the one
product-visible decision and needs sign-off before it ships.
**Exit:** all-time analytics bounded. Recompute idempotence tested (§14.9).

### Wave 7 — Cloud readiness

Shared-store mode; stateless API replicas; the projector as a leader-elected
deployment; spans to OTLP only, out of the rebuild path; repair strategy swap
(§13.5); ingress SSE documentation (§13.6).

**Risk:** medium, but **isolated to deployment**. Because Wave 2 shipped the
store seam and Wave 3 shipped the interface boundaries, this wave changes wiring
and configuration, not the model, the contract, or the client. Tier-1/2
deployments are unaffected by construction — they keep the embedded backend.
**Exit:** an API replica starts in seconds and holds no durable state; the
conformance suite passes against the shared backend.

### Wave 8 — Mutability seam *(no new mutations)*

`minAppliedSeq` / `If-Read-State`; mutations return `change.seq`;
`Idempotency-Key` required and durably recorded; `ETag`/`If-Match` on definition
reads; actor attribution on journal and `change` rows. The three routes stay 501
until #466/#468 land their primitives.

**Risk:** low. Additive; no mutation is enabled.
**Exit:** read-your-write demonstrated end to end against a synthetic mutation. A
retried submit after a client abort provably does not duplicate.

### Sequencing notes

- **Wave 1 is the only urgent wave.** Everything after buys structure, bounded
  growth, and extensibility — not immediate relief.
- **Two things are pulled early on purpose**, because both are cheap now and
  expensive later: gaggle-leading indexes in **Wave 1.2** (before any query is
  written against a non-authz-shaped index) and the store seam in **Wave 2**
  (before twenty more SQLite-specific queries exist).
- **Wave 4 runs parallel to 2/3.**
- **Existing issues fold in:** #1741 → Wave 1.4 then 2. #1782 → Wave 2. #1665 →
  Wave 1, with §2.3's measurement as its answer. #1712 → Wave 2 (aggregate) +
  Wave 5 (invalidation scope). #1711/#1713/#1714 → Wave 5. #1882 → Wave 1.4
  removes its cause; its abort/restart bug remains separately real. #1888–#1892
  (filed under #1883) are approximately Waves 2–3 and should be **re-scoped
  against this document rather than started as written** — they assume the
  current single-connection, in-process-ingest, filesystem-polling shape.
  #1439/#1429 unblocked by the reserved `disposition` column. #644 unblocked by
  §5.5. #652 → Wave 7. #1410 closed by Waves 1–3.

---

## 18. What the first pass got wrong

Recorded because this class of error is the subject of the diagnosis, and a
design document that cannot show its own corrections has not earned the claim in
§14.

| First pass | Why it was wrong | Now |
|---|---|---|
| A separate durable change-feed file with segments, retention floors, torn-record rules, and a lock protocol | Conflated ordering with discovery. Ordering is free from the projection's own commit sequence, in the same transaction. Discovery is a doorbell. And the file form was unnecessary where it worked (one process) and unworkable where it would be needed (separate processes, no shared store) | §4 — `change` table + doorbell. A file format, a retention policy, and a lock protocol deleted |
| One physical store, inheriting "a second duplicates ingestion, reconciliation, retention, and rebuild failure modes" | Those are shared code paths, not duplicated ones. Meanwhile list data is 191 MB and analytics is 2,263 MB, and cold start was gated on the larger | §5.1 — two stores, ATTACHed. Cold start gated on 191 MB; shadow-generation bookkeeping collapses to temp-file-and-rename |
| Repair bounded by parent-directory mtime watermarks | **Every new run bumps the parent's mtime**, so on a live instance the watermark never short-circuits and repair reads 40,665 entries every pass. A bound that holds only when nothing is happening is not a bound. This reproduced the diagnosed pattern inside a document claiming to enforce against it | §6.3 — a rate bound. Constant cost per unit time; cycle time reported as `lastSweepCompletedAt` |
| No mention of authorization | Once a list is one indexed query, authz must be a predicate inside it. Post-filtering after `LIMIT` silently omits rows — the diagnosis's §5.6 silent-omission failure. Retrofitting an authz column into ordering indexes after twenty queries is expensive | §5.5 — `gaggle` leads every ordering index, pulled forward to Wave 1.2 |
| "Aggregate bucket deltas" | Reversible deltas need each run's prior contribution stored and subtracted. Unspecified and easy to get wrong | §5.6 — recompute the dirty day. Bounded and idempotent, which serves determinism for free |
| One writer, no overload policy | Lag would grow and `readState` would report a faithfully useless growing number | §6.4 — shed order plus a lag ceiling above which the surface is `unavailable`, not stale |
| Restart cost unaddressed | Implied a full scan to find unprojected runs | §4.3 — only non-terminal rows can have advanced. O(active), ~10 rows |
| Steady-state rebuild described; the one-time migration not | The transition on 29,759 runs and a 547 MB store is what actually hurts | §6.6 — additive, flagged, revertible without touching a journal |
| Four cost classes; three identifiers (`generation`, schema version, seq) | `stream` is a lifecycle, not a cost. Two identifiers suffice | §7.1, §7.2 — three classes, one sequence |
| `run_stage` carrying denormalized telemetry population flags | Every new population filter becomes a column + migration + backfill — the extensibility trap in the schema | §5.3 — indexed join across ATTACHed stores; denormalize only on query-plan evidence |

---

## 19. Non-goals and risks

**Non-goals.** No journal event or directory-layout change — date sharding is
explicitly deferred (§6.3). No stage invocation/result envelope change. No
breaking change to `/api/v1` parameters, response fields, ordering, or cursor
semantics; `readState`, cost classes, and aggregate routes are additive. No new
service or external dependency at tier 1–2; the one-binary, files-as-durable-state
posture is preserved. No new mutations. No inference of "no work" from duration or
missing telemetry. No request-time full-history fallback presented as a
successful read.

**Risks, and containment.**

| Risk | Containment |
|---|---|
| Stored phase drifts from the journal | `reconstructPhase` survives as a differential oracle over the whole corpus (§14.7); lag is stated in every response, not assumed away |
| Two stores diverge | `read.db` is journal-derived and wholly rebuildable; a rebuild must produce byte-identical rows (§14.9). Cross-store queries are joins, not copies |
| Repair's rate budget is set too low and a restored run stays invisible for hours | `lastSweepCompletedAt` is in every response, so the window is visible rather than assumed; the budget is configurable; the doorbell covers every non-exceptional case |
| Nine waves is a long program that stalls halfway | Wave 1 delivers the measured relief in days and is independent of the rest. Every wave ends green and shippable with a number |
| `partial` becomes permanently lit and stops being read | It always carries a reason **and an expiry expectation** (§7.2), so "catching up" is distinguishable from "stuck"; a test asserts the steady state on a healthy instance is `complete` |
| Store seam adds abstraction cost for a backend nobody uses yet | The seam is thin (queries + migrations), and the conformance suite runs both backends from Wave 2, so the second backend is never theoretical |
| 90-day fidelity window is wrong for someone | A recommendation with its consequence stated (§11.4), configurable, and the buckets keep the long tail answerable in aggregate |
| Tier-3 rebuild cost over a blob mount is worse than assumed | It is measured in Wave 0 (§16.8), before §13.2 depends on it, precisely because no defensible figure exists today |
