# Portal read architecture — a rethink

> **Status:** approved — accepted design, sixth pass; implementation in progress under
> epic #1912. Responds to
> `Goobers-Reviews/2026-07-29_portal-architecture-findings.md` (the diagnosis) and
> supersedes [`unified-index-backed-run-reads.md`](unified-index-backed-run-reads.md)
> (#1883), keeping its read-projection conclusion and replacing the parts it left
> open: ordering, the writer set, request budgeting, in-process isolation,
> topology, authorization, and the hosted shape.
>
> **Revision history is in §18** — nine errors in the first pass, seven
> correctness holes in the second, ten state-boundary findings in the third. The
> fourth pass concentrated on three boundaries the third pass left unsafe: the
> **intake store** (durable discovery cannot live inside the store that gets
> replaced), the **rebuild barrier** (a new epoch could publish silently stale),
> and **query bounds** (SQLite's plan output cannot prove the absence of a
> residual predicate, so the bound must come from an enumerated set of supported
> filter combinations instead). The fifth pass added §11A and §18.1a.
>
> **The sixth pass (§18.0) is an implementation premise audit**, not a review: 156
> factual claims this document makes about the code were checked against the code
> before Wave 0 began. 105 held. **Twelve of the rest change the plan**, and two of
> those are mechanisms §14 relies on that cannot work as written — a *per-route*
> budget cannot be enforced by `http.Server.WriteTimeout`, and `SQLITE_INTERRUPT`
> is never observable to a caller because the driver rewrites it to `ctx.Err()`.
> A third resizes Wave 4: the repository contains **zero** `QueryContext` call
> sites and `internal/telemetry/rollup` has no `context.Context` in any
> non-test signature, so "cancellation reaches the statement" is a plumbing
> project, not a router change. §18.0 records all of them.

---

## 0. How to read this

§1 is the decision. §2 is measured evidence from the live instance — it changes
two of the diagnosis's conclusions and adds a defect the diagnosis did not have.
§3–§12 are the architecture, and **§11A is what changes on screen** — including
the two places this design does not achieve strict UX parity. §13 is the cloud
trajectory. §14 is the acceptance bar. §15 answers the diagnosis's open questions. §16 is the harness. §17 is the
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
at 10 s. There is no cache (`inventory.go:601`).

**There are six production call sites, not four** (corrected in the sixth pass;
the original text named four and #1741's acceptance criteria name three):

| Call site | Route or command |
|---|---|
| `readservice/inventory.go:332` | `/v1/instance` |
| `readservice/inventory.go:380` | `/v1/gaggles` |
| `readservice/inventory.go:512` | `/v1/gaggles/{g}/workflows` |
| `readservice/inventory.go:590` | `/v1/gaggles/{g}/workflows/{w}` — **workflow detail**, added by #1894 after #1741 was filed |
| `readservice/runs.go:529` | `workflowRunActivity`, reached from `runs.go:371` on every `ListRuns(LatestPerWorkflow)` — the path the Overview hits on **every load** |
| `cmd/goobers/status.go:902` | `goobers status`, calling `ActiveRunCountsByWorkflowDirs` directly |

Fixing only the three routes #1741 names leaves workflow detail and the Overview's
own list path on the 17.2 s walk. **#1741's acceptance must be widened to all six**,
and the `goobers status` site needs an explicit disposition (§11.3) rather than
being inherited by accident.

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

**Resolution: confirmed as a mechanism, not as the sole cause (#1913).** The
arbiter experiment is `TestReaderPoolAllowsConcurrentReads`
(`internal/telemetry/rollup/readerpool_test.go`), and it settles the mechanism
directly rather than by inference. Against one shared connection, eight
concurrent aggregate queries took **48.62 ms versus 48.37 ms serial — a 0.99×
"speedup"**, which is not slow concurrency but *perfect serialization*: the eight
readers gained nothing whatsoever from running together. With a read-only pool,
the same eight took **13.48 ms (3.54×)** on the reference host.

So `SetMaxOpenConns(1)` did exactly what §2.3 inferred it did, and the inference
is now a measurement. What the experiment does **not** establish is that
head-of-line blocking explains every 10-second timeout. The production evidence
of proxy cancellations against healthy direct endpoints is untouched by it, and
that failure sits outside the read model. The §18.0 downgrade therefore stands as
written: this is a confirmed contributing mechanism whose removal is worth the
change, not a single universal cause.

One methodological note, because it cost three CI failures to learn. The test
asserts the STRUCTURAL property — a read-only pool exists and opens more than one
connection — and merely *logs* the ratio. An earlier version asserted a speedup
above 1.3× and flaked at 1.25× on a three-core runner under `-race`. A wall-clock
ratio cannot be defended on shared hardware; loosening the threshold moves the
flake rather than removing it. The measurement that justifies the change stands
on its own on the reference host, which is where a measurement belongs.

### 2.3a Reproduced in-tree at 3x (#1913)

§17's Wave 0 exit requires "every measured number in design §2 is reproducible
in-tree". `go run ./test/scale -scale 3 -seed 1 -measure` generates 89,277 runs
and 470,295 scheduler events and reports both latency and **work counters**.

The work counters are the load-bearing half. They assert journal opens, active-
scan directory reads, and active-scan opens — quantities that are identical on
every machine — rather than wall time, which is not. That choice is not
fastidiousness: a wall-clock assertion has now self-refuted four separate times
in this wave, most memorably when a small corpus measured *slower* than a large
one on the same runner.

| Path | Journal opens |
|---|---:|
| `status_full_scan` | **121,995** |
| `overview_fanout` | 255 |
| `listruns_latest_per_workflow` | 137 |
| `listruns_page` / `_gaggle_filtered` / `_deep_page` | 51 each |
| `run_detail_summary_events_attempts` | 3 |
| `instance`, `gaggles`, `workflows`, `workflow_detail` | **0** |

The zeros are the paths already cut over to the read model. The 51 is the
pre-cutover journal path — one open per returned row plus the lookahead, which is
§2.1's "lists open and parse a journal per returned row" measured rather than
asserted. The 3 is `GetRun` + `RunEvents` + `StageAttempts` each parsing the same
file, which is what §8.2's `useSingleRun` collapses on the client and the read
model removes on the server.

`status_full_scan` is retained deliberately as the control. It is the unbounded
shape, and a harness that only measured bounded paths could not show that they
are bounded *relative to* anything.

**The blob-mount rebuild figure (§16.9, §13.2).** The same pass reports:

| | 3x (89,277 runs) |
|---|---|
| `rollup_rebuild`, local disk | **3m22.8s** (89,277 journal opens) |
| `rebuild_on_blob`, simulated at 2 ms/open | **6m21.4s** |

Labelled SIMULATED in the harness output itself, not only here, because §17's
exit permits a simulated figure and permits it *only* when labelled — an
unlabelled projection would be indistinguishable from a measurement on real
remote storage, and §13.2's tier decision rests on which of the two it is.

The shape is what matters: rebuild cost is dominated by per-open latency, so a
mount with 2 ms opens nearly doubles a three-minute rebuild, and one with 20 ms
opens would make it half an hour. That is the §2.6 argument — "29,759 file opens
over a network or blob mount" — with a number attached.

### 2.4 Physical evidence that reads perform maintenance

Every one of the 40,665 run directories contains a `.lock` file — including all
10,906 that have no `run.yaml` and can never be ingested. Those locks were
created by `IngestRun` → `journal.WithPruneProtection` → `acquireJournalLock`
(`internal/journal/prune.go:51`), called from `reconcileIndex` **on the HTTP list
path** (`readservice/runs.go:921`), before failing to read an identity that does
not exist.

**Sixth-pass correction — this evidence is residue, not current behavior, and the
distinction changes what Wave 3 has left to do.** #1708 landed
`if !journal.Recorded(runDir) { continue }` at `readservice/runs.go:918` before
these numbers were taken, so today's list path cannot lock an *unpublished*
directory: the 10,906 stray locks are historical. Read this section as
archaeology proving the *mechanism* existed, not as a live defect.

What **is** still live is the narrower half, and it is the half that matters:
`reconcileIndex` still calls `IngestRun` — and so still takes a journal lock on
the HTTP list path — for every directory that *is* recorded but not yet indexed
(`runs.go:921`). The invariant "no read path creates a `.lock` file" is therefore
still false today and still a real Wave 3 exit criterion; it is simply no longer
true that it fires on directories that can never be ingested.

### 2.5 The scale harness does not measure the failing surfaces

`test/scale/measure.go` builds the read service with `minimalDefinitions()` — **no
gaggles and no workflows** — and never calls `Instance()`, `Gaggles()`,
`Workflows()`, or `LatestPerWorkflow`. It never measures run detail and never
applies concurrent load. §16 fixes it first, because without it we cannot tell a
fix from a coincidence — and because §2.3's hypothesis can only be settled there.

**Sixth-pass correction to the mechanism.** The empty inventory is *not* why the
scan goes unmeasured, and an implementer who "fixes" it by populating definitions
will still measure nothing. `Local.Instance` calls `s.activeRunCounts()`
unconditionally (`inventory.go:332`), and `activeRunCounts` (`:601`) walks
`Layout.RunDirs()` — it is keyed on run **directories**, not on the configured
inventory, so an empty inventory does not spare it. The harness misses the scan
for the simple reason that **it never calls those methods at all**. Populating the
inventory is still required (§16.1 needs 2,000 workflows for §14.4's fan-out
assertion), but it is a separate fix from calling the surfaces.

**Three further Wave 0 corrections, each of which would otherwise produce a
misleading baseline:**

- **The coded 1× is not the live instance.** `-runs=1 -scheduler-events=400000`
  (`generate.go:32`) measures to a **76 MiB** scheduler journal against the live
  **324 MB**, and 13,600 runs against 40,665 directories. Publishing "1×" from
  today's constants understates the live instance roughly 4× on journal bytes.
  §16 must re-anchor the baseline to §2's measured shape and date-stamp it,
  because it will drift again.
- **Two §16.5 pathologies are currently inverted, not merely absent.**
  `OrphanDirs` is fixed at 5 regardless of scale (`generate.go:97`) and orphan
  dirs are written *without* a `.lock`, while every dir created through
  `journal.Create` has one — the exact inverse of the live instance's 10,906
  lock-bearing unpublished directories. And generated spans carry ~45-byte
  payloads, so the span:event byte ratio is inverted relative to the live 12×.
- **Generation is fsync-bound and the corpus sizes here are not reachable without
  addressing it:** measured 6.9 runs/s with fsync on versus 124 runs/s with
  `GOOBERS_DISABLE_FSYNC=1`. A 100k-run corpus is ~4 hours versus ~13 minutes.
  Fixture generation must therefore write journals directly rather than through
  `InstanceLog.Append` (whose §2.2 defect is itself O(n²) during generation), and
  the harness must state which corpora were generated with fsync disabled, because
  that is exactly the durability behavior some fixtures exist to test.

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

  **Sixth-pass correction: there is exactly one existing exception, and it must be
  handled rather than assumed away.** `first_success_milestones`
  (`rollup/schema.go:417`) is **not journal-derivable**. Migration v14 seeds it
  from already-projected rows (`schema.go:423`), and `RebuildAll` preserves it by
  reading the **old database** before deleting it — `existingTimeToFirstPR(dbPath)`
  at `rollup/rebuild.go:31`, replayed forward at `:44`. So "delete the store and
  rebuild from journals" silently loses onboarding's time-to-first-PR metric today.

  This bears directly on §6.5 and §14.9. The rebuild-inherits-policy-state rule
  the fifth pass added for the projection floor and tombstones (§6.5 step 2) is
  the **same rule** this needs, and the resolution is to name it generally:
  **a rebuild must carry forward every derived fact that is not reconstructible
  from journals, and that set must be enumerated in code rather than discovered
  during an incident.** Wave 2 owns the enumeration; §14.9's byte-identical
  assertion is scoped to journal-derived rows and must state the carried-forward
  set explicitly rather than ignoring it.
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
  -- A higher source_seq is proof that a pending removal intent is stale:
  -- retention crashed between intent and unlink (§ below) and the run has
  -- since advanced or been resumed. Without this, the projector takes the
  -- removal branch and deletes a live run's rows.
  removing   = CASE WHEN excluded.source_seq > source_seq THEN 0 ELSE removing END,
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

**And if that run advances instead, the intent is cancelled, not honoured.** The
upsert above clears `removing` whenever an incoming watermark advances
`source_seq`, so a run that is written to or resumed while a stale removal intent
is pending is never deleted. This is the one ordering in the protocol where the
writer overrides retention, and it is the correct direction: a live run outranks a
crashed intent to delete it.

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
  slow success. The set is generated into the OpenAPI/contract surface.

- **Sixth-pass correction: "so the UI can only construct supported combinations"
  is false, and this materially raises the bar.** The portal parses its filters
  from **arbitrary hash query parameters** (`portal/src/routing.ts:56`–`67`),
  validating only enum membership — not which *combinations* are legal. Every
  Insight drill-through target is a bookmarkable `#/runs?…` href
  (`InsightPage.tsx:434`, `:467`–`499`, `:700`), so users share and re-enter these
  URLs, and any filter tuple × any phase is reachable **today** without the UI
  offering it. One combination is already reachable *only* by URL and produced by
  no UI element at all: `population="premium-measured"` (`types.ts:24`,
  `routing.ts:163`).

  Two consequences:

  1. **The closed set must be defined over URL-reachable combinations, not
     link-reachable ones.** §14.2's enumeration test must walk the *router's*
     parseable space, which is the cross-product of the validated enums — not the
     set of hrefs the UI happens to emit. This is a strictly larger set and it is
     what makes §11A's capability-reduction a condition rather than a hazard.
  2. **The refusal must degrade gracefully in the client**, because a stale
     bookmark is a normal event rather than a misuse. A refused combination
     renders as an explained empty state offering the nearest supported
     neighbour — never a raw error. §11A's parity condition is not met by the
     server-side refusal alone.

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
`(run row, run_stage rows, population rollups, change row, last_projected_seq)`
— one `read.db` transaction — is serialized. The intake acknowledgement is **not**
part of it: it is a guarded post-commit write to a separate store (§4.3, §6.2),
because SQLite WAL offers no cross-file atomicity and idempotent projection makes
it unnecessary. Without
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
- **An explicit resume overrides the floor.** **Sixth-pass note: this rule is
  correct and currently unreachable.** `runner.ResumeFromTerminal` has **zero
  production callers** — only `internal/runner/resume.go` itself and three test
  files; the CLI surfaces that would reach it are explicit stubs pending
  #465–#469. So the floor-override is implemented as a **property of the intake
  protocol** (an intake watermark is authority to re-admit, full stop) rather than
  as a special case wired to a specific caller, which is both cheaper and what
  makes it correct when the CLI does land. It is not separately testable end-to-end
  until then, and §16's fixture exercises it through `Intake` directly.
  `runner.ResumeFromTerminal`
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
2. **Copy the projection floor and tombstones into *E* first, before projecting
   any journal, and apply the floor while building.** They are derived *policy*
   state living in the old `read.db`, not journal facts, so a rebuild from
   journals alone does not reproduce them. Skipping this makes a post-retention
   rebuild briefly re-admit every expired journal — a removal and change-feed
   burst, and a rebuild whose size is proportional to total history rather than to
   the retained window, which defeats §14.12's rebuild-time and store-size
   targets outright.
3. Validate *E* (schema version, row counts, differential spot-check).
4. **Barrier:** stop the commit loop, then catch *E* up from **two** sources:
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
5. Quiesce and **close the entire reader pool**, update the epoch pointer, reopen
   against *E*. Requests during the swap get 503 + `Retry-After`, not a stale
   inode.
6. Retain the previous epoch's file until the swap is confirmed, then remove it.
7. **Release the change-retention pin** (§4.2) — see below.

A crash before step 5 leaves the old epoch authoritative and discards or resumes
*E*; it can never partially publish. Clients holding a pre-swap cursor get
`epoch_changed` (§4.2) and refetch a snapshot.

**The change-retention pin is released on every terminal outcome, not only
success.** §4.2 pins `min_change_seq` at `rebuildFromSeq` so the barrier's
catch-up rows survive; the release rule must therefore cover **success, abort,
discard, and startup recovery of a stale or expired build**, or an aborted rebuild
prevents `change` pruning indefinitely and the table grows without bound. The pin
is recorded with the build it belongs to, so startup drops a pin whose build is no
longer in progress. Kill-at-every-boundary coverage (§14.8) includes the pin's
release.

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
4. Only in a later release, retire the superseded `runs` columns from
   `telemetry.db` — see the correction below.

Rollback before step 4: flip the flag, or delete `read.db` and revert the binary.
No journal is touched at any step.

**Sixth-pass correction: `runs` is not unused, and §5.1's characterization of
`telemetry.db` as analytics is wrong.** The store holds **22 tables, 18 of them
per-run** (`ingest.go:76`) — `runs`, `run_goober_digests`, `stage_attempts`,
`stage_usage`, `stage_model_usage`, `gate_verdicts`, `provider_mutations`,
`run_errors`, `harness_transcripts` and more — and there are **32 non-test
references to the `runs` table** across the tree. "Drop the unused `runs` table"
was written as bookkeeping and is in fact a migration touching most of the query
surface.

This does not change the decision to split the stores, and it does not change the
§5.1 sizing argument, which rests on **rebuild input** (191 MB of events versus
2,263 MB of spans) and is unaffected. What it changes is step 4's scope and the
transition's shape: the per-run tables that back *analytics* joins (stage usage,
model usage, verdicts) stay in `telemetry.db` and keep their `runs` join target,
so `runs` in `telemetry.db` becomes a **narrow join key** — run id, gaggle,
workflow, `started_at` — rather than the full row it is today. The complete row
moves to `read.db`. Step 4 retires the *superseded columns*, not the table, and
which columns those are is Wave 2's enumeration, not an assumption.

**Two adjacent live defects Wave 2 must not inherit,** both found in the same
audit and neither previously filed:

- **`goobers telemetry compact` can permanently break scheduler ingest.**
  `CompactInstanceEvents` (`journal/compact.go`) rewrites `scheduler/events.jsonl`
  shorter while preserving the surviving records' `seq`, but `IngestSchedulerLog`
  advances a byte-offset cursor. After a compaction the cursor points past
  surviving records. A projector keyed on instance-journal position inherits this
  unless the cursor is keyed on `seq` rather than offset.
- **A run terminalized by the stalled-run sweep never has its rollup rows
  refreshed.** `sweepStalledRuns` (`cmd/goobers/stalledruns.go:85`) holds no
  rollup handle and writes no index update, so the store keeps the pre-terminal
  row until something else re-ingests it. This is exactly the class of hole
  §4.3's intake protocol closes — the terminalizer is one of the writers, and
  under this design it must upsert a watermark — but it is a **present** silent
  staleness bug, and the differential oracle will find it as a "drift" that is
  really a missing write.

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

Budgets come from §16's measured **p99.9**, not from taste.

**Sixth-pass correction — how they are enforced.** Earlier passes said
`context.WithTimeout` in the router *and* `http.Server.WriteTimeout`. The second
half is wrong twice over, and shipping it would break a working feature:

- **`WriteTimeout` cannot express a per-route budget.** It is a single
  server-global field on `http.Server` (`internal/httpapi/server.go:85` is the only
  `http.Server` in the API path) and `net/http` applies it once per connection at
  request start. A `Route.Budget` per route has no way to reach it.
- **Setting it at all would cut the SSE stream.** `/api/v1/events` is a
  deliberately unbounded response (`eventstream.go:936`). A server-global write
  deadline terminates it at the deadline. The stream survives today only because it
  resets a short write deadline before every message via `ResponseController`.

**The per-request primitive is `http.NewResponseController(w).SetWriteDeadline`,**
which the event stream already uses (`eventstream.go:982`) — so the mechanism is
in-tree and proven, just not yet generalized. Per-route enforcement is therefore:
`context.WithTimeout` for the work, `SetWriteDeadline` for the socket, both from
`Route.Budget`, and **no global `WriteTimeout`**.

Two consequences the contract test must encode rather than inherit:

- **`Route.Budget` cannot be required non-zero for every route.** An SSE stream has
  no meaningful total budget. The contract needs an explicit `CostStream`-style
  disposition (or a documented sentinel) so "unbounded on purpose" is declared and
  reviewable, instead of forcing a fictitious number that the enforcement layer
  then has to special-case anyway.
- **Byte-serving routes need a stated policy.** Artifact and transcript downloads
  (`router.go:405`, `:429`) are dominated by client link speed, and a body cannot
  be turned into a 503 after `WriteHeader(200)`. They are budgeted on *server work
  before first byte*, not on total wall time, and that must be written down.

Three rules:

- **Every server budget is strictly below the client's 10 s abort**, which becomes
  a backstop rather than the only bound.
- **Shed at admission over accept-and-timeout.** Queue wait counts against the
  budget, so a saturated class would otherwise accept work it cannot finish. A
  fast 503 with `Retry-After` is cheaper for both sides.
- **Deadline expiry must actually stop the work, and the mechanism exists.**
  `modernc.org/sqlite` wires context cancellation to `sqlite3_interrupt`
  (`interruptOnDone` in `sqlite.go:78`, used by `stmt.go:105`/`295` and
  `tx.go:71`), so a cancelled request aborts the in-flight statement rather than
  running to completion. An earlier pass claimed the driver exposed no interrupt;
  that was wrong — it checked the exported API surface rather than driver behavior.

  **Sixth-pass corrections. Three, and the first two invert stated mechanisms.**

  1. **"Every request query uses `QueryContext`/`ExecContext`" is not a
     description of a small change — it is a project.** The repository contains
     **zero** `QueryContext`, `ExecContext`, `QueryRowContext`, or
     `PrepareContext` call sites, and `internal/telemetry/rollup` contains **no
     `context.Context` in any non-test function signature at all** (e.g.
     `query.go` `func (db *DB) Runs() (…)`). Roughly 36 exported methods across
     ~12 files need a context parameter before a deadline can reach a statement.
     §17's "Wave 4 risk: low — touches router and contract, not the projection"
     is false as written; the plumbing is mechanical but wide, and it is a
     **prerequisite of Wave 1.5**, not a part of Wave 4. §17 is corrected
     accordingly.
  2. **`SQLITE_INTERRUPT` is never observable to the caller, so nothing can map
     it.** The driver arms `interruptOnDone` only when `ctx.Done() != nil`, and
     the deferred block immediately after **rewrites the result**:
     `if ctx != nil && atomic.LoadInt32(&done) != 0 { r, err = nil, ctx.Err() }`
     (`stmt.go:98`–`116`, `:285`–`300`). What reaches the caller is
     `context.DeadlineExceeded` or `context.Canceled` — never a SQLite error
     code. **The 503 mapping therefore keys on `errors.Is(err,
     context.DeadlineExceeded)`**, with `context.Canceled` distinguished by
     *whose* cancellation it was: client disconnect is the existing 499 path
     (`router.go:486`), server budget expiry is 503. The property §14.6 asserts is
     unchanged and still correct — an interrupted page returns 503, never a
     truncated 200 — only the discriminant changes. Note this is a live trap:
     `clientCancelled` (`router.go:486`) keys on `context.Canceled`, so a
     router-installed `WithTimeout` yields `DeadlineExceeded` and falls through to
     **500 `read_error`** today unless the mapping is added deliberately.
  3. **An interrupted connection is destroyed, not reused.** `conn.IsValid`
     returns false once `sqlite3_is_interrupted` is set for a file-backed database
     (`conn.go:950`–`967`), so `database/sql` discards it. Every budget expiry
     therefore costs a connection open on the reader pool (§5.2), which means
     **admission control is what protects the pool** — a route that sheds at
     admission (below) costs nothing, while one that accepts and times out costs a
     reconnect. This strengthens the shed-at-admission rule from a preference to a
     resource argument.

  §14.1 asserts the mapping and that no goroutine keeps working past its deadline.

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
| An interrupted, truncated, or over-budget page — including a statement aborted by budget expiry, which surfaces as `context.DeadlineExceeded`, **not** as `SQLITE_INTERRUPT` (§7.1) | **503** + `Retry-After`. Never a 200 |
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

## 11A. What changes on screen

The brief was parity: same featureset, better reliability. That mostly holds — the
page inventory, the data on each page, and every diagnostic surface are unchanged.
But it does not hold *entirely*, and the deviations should be visible to a reader
rather than inferred from §7.2.

**Unchanged.** Overview, Workflows, Runs, Run detail, Insight, Errors. What each
page shows. Graph, stages, attempts, evidence, escalation cause, replay. Filter
*meanings*. The 50/200 page sizes and keyset paging. Insight's metrics — same
numbers, answered from buckets. No new mutations; the three routes stay 501.

**Behavior fixes users will feel.** Paging is no longer discarded by an arriving
event (#1713 — today `runsHistory.ts:178` calls `refresh()`, which resets the
cursor). A dead stream stops reporting "connected" (#1711). Stage attempts refresh
on a live run (#1714).

**One genuinely new surface: freshness means something different.** The indicator
exists today (`LiveFreshness` → `DaemonQueryState`) but reports the *SSE
connection* — connected / reconnecting / stale / offline / polling-fallback —
which is not how current the data is, and is precisely why an operator cannot
distinguish "slow" from "broken."

| Today | After |
|---|---|
| "connected" (about the socket) | "current" / "stale by 4 s" (about the data) |
| A spinner that dies at 10 s with no reason | "Unavailable — <reason>" with retry |
| Standalone loads slowly and silently | "Building read model, 61%" |
| — | "Partial — analytics 41% projected", with an expiry |

Plus a degraded-mode banner on a read-only volume (run detail works, lists do
not), and — during Wave 1.4 only — active-run counts labelled "as of N s ago".
This is design work someone has to do, not just a data contract.

**Two capability reductions, both requiring a decision.**

1. **Unsupported filter combinations become a typed refusal (§5.7), and parity
   here is a condition, not a guarantee.** The Runs page constructs
   `gaggle × workflow × stage × outcome × population × since × until × phase`
   (`runsHistory.ts:36–39`), and Insight drill-through builds combinations
   *programmatically* from a metric (`InsightPage.tsx:1051–1073`). The set is
   enumerable because that catalog is finite — not because users cannot ask
   arbitrary questions. **If the enumeration misses a combination drill-through
   can produce, that is a regression against behavior that works today.** §14.2's
   API-surface enumeration test is what makes this a condition rather than a
   hazard, and it must pass before Wave 2 lands.
2. **The 90-day window (§11.4).** A six-month-old run stays answerable in
   aggregate but may not be individually listable. Straightforwardly less than
   today, and gating Wave 6.

Also new but currently invisible: a multi-gaggle scope above `maxMergeFanout`
returns "narrow your scope" (§5.5). Unreachable today, since `AllowAll` takes the
unrestricted fast path; reachable once #644's RBAC lands.

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
| Journal-walking active-run count on read paths | `localscheduler/reconcile.go` + **6** call sites (§2.1, corrected) | Stored, indexed phase (§5.4). **The cold-start primitive that is genuinely retained is the unexported `activeRuns` (`reconcile.go:38`), reached from `Scheduler.ReconcileAll` (`scheduler.go:315`)** — daemon-startup reconciliation legitimately needs a journal-authoritative count when no projection exists. The exported single-directory `ActiveRunCounts` (`reconcile.go:22`) has **no production caller** and is test-only; it goes with the read-path usage |
| Filesystem polling, 5 s sweep, per-run stream state, in-memory history, random session cursor | `httpapi/eventstream.go` (~1,000 lines) | `change` tail (§8.1) |
| Whole-run delete-and-reinsert across 18 tables | `rollup/ingest.go:28,76` | Incremental tail projection (§6.2) |
| Journal-scan fallback as a silent default | `readservice/runs.go:413,710`, **and `runs.go:358`–`362`** — a third silent fallback (`listLatestWorkflowOutcomesScanning`) added by #1894 after this list was written | Explicit `--authoritative` (§11.3) |
| Index-free standalone construction | `cmd/goobers/dashboard.go:394` | One topology (§11.2) |
| `SetMaxOpenConns(1)` | `rollup/db.go:61` | Reader pool (§5.2) |
| Full-journal re-read per instance-log append | `journal/instance.go:81` | Bounded tail read under the same lock (§17 Wave 1.1) |
| Pagination reset on live invalidation | `runsHistory.ts:94–102,178` | In-place patching (§8.2) |

Kept deliberately: the three observer seams from the diagnosis's Appendix B
(`openRunObserver`, `reconcileScanObserver`/`reconcileInspectObserver`,
`journalReadObserver`), re-pointed at the new components.

**Sixth-pass correction: `journalReadObserver` cannot simply be "kept."** It is
declared at `eventstream.go:293` and fired at `:385` — *inside* the change-detection
region this same table deletes — and its three consumers
(`eventstream_test.go:789` `TestScanPollsOnlyActiveRunsNotEveryRunInHistory`,
`:827`, `:872`) assert properties of a mechanism that ceases to exist. "Re-pointed
at the new components" is the right intent but it is not a no-op: the seam must be
**re-homed onto the projector's journal reads**, and the three tests must be
**replaced** by equivalents asserting the same property against the `change` tail —
that change detection is bounded by active work rather than by history. Deleting
them without replacement silently drops the only coverage of that property.

Also corrected: **`eventstream.go` is 1,009 lines but the deletion is ~640 of
them** (roughly `:107`–`742`: `sourceState`, baseline, scan, sweep, poll, discover,
per-run offset/digest state, and the in-memory history ring). The remainder — SSE
framing, heartbeat writing, the `ResponseController` write-deadline pattern §7.1
now depends on, and the handler itself — is kept and rewired.

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
| 8 | Crash, restart, and **adverse-state** safety | One `read.db` transaction plus a guarded post-commit ack; epoch swap with `rebuildFromSeq` catch-up (§6.2, §6.5) | Kill at every boundary during project, rebuild, and **epoch swap**: previous epoch intact or resumed; no partial epoch readable. **A run that advances during a rebuild and is acknowledged by the old epoch is still caught up in the new one** (§6.5's exact scenario, as a fixture), and **no run's `last_projected_seq` regresses across the swap**. An external process writing intake across a swap loses nothing. **The rebuilt epoch inherits the projection floor and tombstones** rather than re-admitting expired journals, and **the change-retention pin is released on every terminal outcome** — success, abort, discard, and startup recovery of an orphaned build — so an aborted rebuild cannot block `change` pruning. **A run that advances while a stale removal intent is pending is not deleted.** Restart is O(active + pending). Plus **disk-full, read-only volume, corrupt store, and daemon↔standalone transitions** |
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

**15.1 The writer set, and is it a choice?** Daemon runners, the out-of-process
`goobers run`, the stalled-run terminalizer (`cmd/goobers/stalledruns.go:194`),
and retention. `goobers run` **stays first-class**, recording source watermarks
through `Intake` (§4.3) on every query-visible transition. The daemon has no
in-memory knowledge of runs written while it was down, which is the real reason
reconciliation is currently a correctness requirement.

**Sixth-pass corrections. The set is six, not four, and two of the four
descriptions were wrong.**

| Claim | Correction |
|---|---|
| Four writers | **Six.** Add (5) **`goobers run abort`**, which appends a terminal `run.finished` / `PhaseAborted` directly (`cmd/goobers/run.go:349`) plus a branch touch, taking **no `up.lock`, no delegation, and writing no index update** — the one writer that can mutate a run journal concurrently with a live daemon; and (6) **`goobers journal redact`**, which reopens a run for append (`cmd/goobers/journal.go`). Any intake-based discovery that omits them is incomplete by construction, and `run abort` is the case that matters, because the transition it writes is terminal and therefore query-visible |
| `goobers run` "stops being a silent writer" | **It already ingests.** `runStandaloneTrigger` (`run.go:113`) builds the same `trackedStarter` whose `Start` calls `ingestRunTelemetry` (`daemon.go:799`), and `run.go:200`'s `wg.Wait` exists so the ingest completes before exit. The change under this design is that it records an **intake watermark** rather than performing the projection itself — a different and smaller claim than "becomes non-silent" |
| "`up.lock` can go stale, as it is on the measured instance" — offered as a qualifier on delegation | **It cannot affect delegation.** The delegation branch keys on `ErrHeld` from `syscall.Flock(LOCK_EX\|LOCK_NB)` (`platform/lock/lock_unix.go:11`–`21`), and a kernel `flock` is released when the holding process dies — so a dead daemon cannot make `goobers run` delegate. What *does* go stale is the **JSON payload** (`pid`, `version`) that liveness *display* reads; the measured instance shows exactly that, a stale payload with no live holder. The distinction matters because §11.2's standalone mode selection inspects that payload (`dashboard.go:255`) |
| Retention's removal signal is new | **A durable removal intent already exists.** `journal.ReserveTerminalForPrune` (`prune.go:17`) takes the per-run flock, refuses a `PhaseRunning` run, and atomically writes a `.pruning` marker; `WithPruneProtection` (`:50`) is how `IngestRun` already refuses to ingest a run being pruned. §4.3's intent → unlink → project → confirm protocol should be **built on this marker**, not beside it — otherwise there are two removal intents with different crash semantics |

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
every run-detail timeout is head-of-line blocking — is **answered in §2.3**:
head-of-line blocking is confirmed as a mechanism (0.99× → 3.54× on eight
concurrent aggregates), and rejected as the sole cause. Both halves matter; a
confirmed mechanism is a reason to fix it, not a licence to stop looking.

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
   **Settled**: the concurrency half is answered by
   `TestReaderPoolAllowsConcurrentReads` — 0.99× before, 3.54× after — and the
   answer is recorded in §2.3 rather than left as a hypothesis.
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

**Risk:** ~~none — test-only.~~ **Sixth-pass correction: low, but not zero, and not
"test-only" in the sense that matters.** "Test-only" is true of the product and
false of the gate. Four in-tree constraints shape the work:

- **`test/scale` is a package under `./...`**, so it runs in the `unit` group of
  every `make ci` (`test/ci/main.go`) and lands in the coverage denominator
  (`test/coveragegate/main.go` excludes only `/cmd/`, generated files, and
  `/api/schemas/`). A large harness added naively spends the unit group's wall-clock
  budget on every PR in the repository. The heavy corpora must therefore be
  **opt-in and excluded from the default gate**, and the mechanism has to be
  visible to a reviewer rather than an env var nobody sees.
- **`make ci`'s shape is golden-pinned.** `test/ci/portability_test.go` asserts the
  `ci` target has *exactly* prerequisites `[deadcode]` and recipe
  `[$(GO) run ./test/ci]`, so the harness cannot be hung off `make ci` as a new
  prerequisite. New checks go **inside** `test/ci`, added to the exact ordered
  golden list in `test/ci/main_test.go` and assigned exactly one group —
  `TestEveryMergeCheckHasAGroup` and `TestGroupPartitionCoversMergeGate` enforce
  both.
- **The required check's name is pinned by the branch ruleset**
  (`.github/workflows/ci.yml:606`, `make ci (fmt-check · vet · build · test ·
  lint)`). Any new job must join `required-ci`'s `needs` list rather than rename or
  replace it.
- **There is no benchmark tier to extend.** The repository contains exactly **one**
  `func Benchmark` (`test/scale/scale_test.go`), no `make` target running
  `go test -bench`, and no CI job that does. §5.7's "each combination ships with a
  rows-visited benchmark" therefore requires **inventing the tier**, and whatever
  is invented becomes new required-gate surface and new wall clock. Design it as a
  `test/ci` check with an explicit group, not as loose benchmarks.

**Also: no reference host exists.** §14.12's absolute targets are stated "on the
reference benchmark host," and the CI runners (`ubuntu-latest` / `macos-latest`,
contended, shared) are not a defensible one. Wave 0 must **declare** the reference
hardware with its numbers, and report CI-runner figures separately as a
regression signal rather than as the acceptance measurement.

**Exit:** every §2 number reproducible in-tree; every §14 property has a failing
test; the blob-mount rebuild figure exists (simulated is acceptable, labelled as
such); §2.3's mechanism is confirmed or replaced; the reference host is declared;
and the harness's default-gate cost is stated and bounded.

### Wave 1 — Reduce the measured hot paths *(five small independent diffs)*

| # | Change | Effect | Risk / rollback |
|---|---|---|---|
| 1.1 | **Bound the instance-log append without breaking sequence allocation.** Under the *existing* cross-process journal lock, scan backward from EOF (the bounded chunked pattern in `eventstream.go:612`) **until a line that, after NUL stripping, parses as an event with non-zero `Seq`** — not merely to the last newline. Impose a byte budget; **if the budget is exhausted, fall back to the full `readEvents` recovery path rather than allocating from zero.** Torn-tail detection comes from the same read | Removes 1.3 s and growing per journaled scheduling decision, under a lock, in the serving process | **Medium, not low.** The reread *is* the cross-process sequence allocator; `TestInstanceLogConcurrentAppendsAllocateUniqueMonotonicSequence` exists because of #530's "two events sharing seq:5", and per-handle in-memory sequence would reintroduce it. **And the last complete line is not necessarily the last event:** `readEventRecords` strips NUL crash-fill and `continue`s on a line that collapses to empty (`reader.go:449–454`), so the #116 cascade can leave a newline-terminated fill-only tail — reading to the last newline would recover `seq=0` and reallocate from 1, duplicating every sequence. Hence scan-to-valid-event with a budget and a full-recovery fallback. **Both independent-writer tests retained, plus a bytes-read-per-append bound, plus the existing NUL-cascade fixtures added to sequence-allocation coverage.** Rejected alternative: a sequence sidecar — a second file and a second recovery path. Rollback: revert the commit |
| 1.2 | Indexes on `runs`: gaggle-leading scoped indexes plus a global recency index (§5.5) | Removes full-scan + sort per page and the unindexed window function | Very low. Additive `CREATE INDEX` migration. Rollback: drop them |
| 1.3 | Split reader/writer handles; reader pool `MaxOpenConns=NumCPU`, `mode=ro`; `synchronous(NORMAL)` | Readers stop serializing behind each other and the writer | Low–medium. Uses WAL as already configured. Gate on §16.3's mixed-load run |
| 1.4 | Background active-run sampler off the request path, served from memory with its age reported. **No sample yet returns `read_model_rebuilding` with progress — never the synchronous scan.** Deliberately throwaway; Wave 2 replaces it | `/instance`, `/gaggles`, `/workflows` stop paying 4–17 s | Low. Corrected from the second pass, which kept the 17-second scan as the no-sample fallback and so preserved the exact failure on a cold daemon |
| 1.5 | **Per-request `context.WithTimeout` + `ResponseController.SetWriteDeadline` per route — not `http.Server.WriteTimeout`, which is server-global and would cut SSE (§7.1).** Requires threading `context.Context` through `internal/telemetry/rollup` first: ~36 exported methods across ~12 files, **zero of which take a context today** | No request can hang, and a deadline actually aborts the statement | **Medium, reclassified from low — and the largest diff in Wave 1.** The plumbing is mechanical and compiler-checked, but it is wide and it is a prerequisite rather than a follow-on: without it a router deadline cannot reach a statement, so 1.5 has no effect. Land the context threading as its own mechanical commit, then the budgets. Also add the 503 mapping for `context.DeadlineExceeded`, which today falls through to 500 `read_error` (`router.go:486` keys only on `context.Canceled`) |

**Exit:** Overview, Workflows, and the warnings widget load on the live instance.
Whether run-detail timeouts are resolved is the §2.3 experiment's answer, reported
either way.

**Sixth-pass additions to Wave 1, all measurement honesty:**

- **1.1's before/after must be measured on a compacted instance journal, or the
  improvement is misattributed.** **88.8% of the live journal's 324 MB — 287 MB
  across just 108 records, largest 2.66 MB — is residue of an already-fixed defect**
  (#1414's unbounded aggregate, since bounded at 16 KiB), all written 2026-07-21/22.
  A "1.30 s → X" number taken against that corpus measures the residue, not the
  algorithm. Publish both: against the live corpus as found, and against a
  compacted one.
- **1.1's byte budget must assume the fallback is live, not theoretical.** With a
  2.66 MB worst-case record in the real corpus and `maxEventBytes` permitting 8 MiB
  (`reader.go:480`), any budget below ~2.7 MB makes the full-recovery path fire on
  real data. Size it above the observed maximum and count fallbacks as a metric.
- **The uniqueness oracle cannot assert global uniqueness over the real corpus.**
  The live journal already contains **1,394 duplicate `seq` values and 119
  in-file-order regressions**, all inside 2026-07-15T21:27→2026-07-16T17:21 —
  #530's bug, from before its fix. Any acceptance test that replays the real corpus
  must scope its uniqueness assertion to records written after that window, or it
  fails on history it did not cause. §14.11's synthetic 25-handle test is
  unaffected and remains the real gate.
- **1.1's scope decision, stated:** the same whole-file read is *also* on the read
  path — `readservice/status.go:53` and `:113` call `journal.ReadInstanceLog`
  directly, and `localscheduler/scheduler.go:328` does so per `ReconcileAll`.
  Wave 1.1 as scoped fixes `Append` only, which leaves three O(history) reads
  live. Those are **in scope for Wave 1.1**, because leaving them makes §2.2's
  headline only half-true and the fix is the same bounded reader.
- **A corruption-detection narrowing to state, not discover later.** Today one
  unparseable complete line makes `readEvents` return a hard error
  (`reader.go:456`), so every `Append` re-validates the entire file. A bounded tail
  read validates only the tail — corruption in older regions stops being detected
  on append. That is the right trade (it is still detected at open, and by every
  reader), but it is a behavior change and needs a test pinning **where** detection
  now happens.

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

**Risk:** ~~low. Additive to the wire contract; touches router and contract, not the
projection.~~ **Sixth-pass correction: low *given Wave 1.5*, and the reason is
that Wave 1.5 now carries the cost.** "Touches router and contract, not the
projection" was only ever true if request queries already took a context; none do
(§7.1). With the plumbing landed in 1.5, Wave 4 really is additive — cost classes,
budgets, the `readState` envelope, and admission control are router-and-contract
changes.

Two remaining decisions this wave must make explicitly rather than by default:

- **Where `readState` is attached.** Adding it to each `readservice` response
  struct changes `apicontract/wire.go`, `wire.generated.ts`, and the portal's
  types; wrapping it at the `httpapi` layer in `writeJSON` keeps `readservice`
  unaware of the envelope but changes every response's JSON shape from one place.
  The latter is preferred — the envelope is a transport concern — but it must be a
  decision, because the generated-types drift guard will surface it either way.
- **Whether `/api/v1/runs` adopts strict unknown-parameter rejection** (as
  `telemetry` already does) before §14.2's enumeration test is written. It is the
  honest pairing with a closed combination set, and it is potentially breaking for
  any client sending extra parameters — so it ships with the refusal, or not at all.

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
- **Existing issues:** #1741 → 1.4 then 2, **widened to all six call sites**
  (§2.1). #1782 → 2. #1665 → Wave 0's experiment then Wave 1.
  #1711/#1713/#1714 → 5. #1888–#1892 (filed under #1883) are approximately Waves
  2–3 and should be **re-scoped against this document rather than started as
  written**. #1439/#1429 unblocked by `disposition`. #644 unblocked by §5.5.
  #652 → 7. #1410 closed by Waves 1–3.

  **Sixth-pass corrections to this list.** **#1712 and #1882 are already closed** —
  #1712 by PR #1894 and #1882 by PR #1886, both merged before this document's own
  baseline, so "#1712 → 2 + 5" and "#1882's abort/restart bug remains separately
  real" are stale. **#1891 needs re-scoping, not implementing as written:** its body
  predates #1894 and asks to "add an additive read-service query that returns the
  latest terminal run outcome" — which #1894 already added. Its real content under
  this design is *make the existing aggregate indexed and bounded, and decide the
  fate of the `latestPerWorkflow` query parameter and the `workflowActivity`
  response field*, which are public contract surface (`apicontract/wire.go`,
  `wire.generated.ts`) that no one has ruled on. One of them must appear in §5.7's
  enumerated set before #1920 can close.

- **Two merged design documents contradict this one and need supersede pointers,**
  not silent override: `dashboard.md` §8 specifies the process-scoped session id,
  `Last-Event-ID` replay, and `409 stale_cursor` as the complete change-notification
  contract — all of which §8.1 replaces, and all of which are shipped behavior with
  tests. `dashboard.md` §14 already carries a supersede pointer; §7/§8 must too,
  before #1929.

- **The diagnosis is not in the repository.** §14 and §12 cite
  `Goobers-Reviews/2026-07-29_portal-architecture-findings.md` — including Appendix
  B, which is where §12's observer-seam list comes from — but that file lives
  outside this repo, so an implementer with only the repository cannot read the
  document the acceptance bar is written against. Either vendor it under `docs/` or
  inline the two lists §12 and §16 actually depend on.

---

## 18. Revision history

Recorded because this class of error is the subject of the diagnosis, and a design
document that cannot show its own corrections has not earned §14.

### 18.0 Fifth pass → sixth pass (implementation premise audit)

Not a review. Before Wave 0 began, **every factual claim this document makes about
the code was checked against the code** — 156 claims, subsystem by subsystem.
**105 held.** The rest are recorded here because a design whose citations have
drifted misdirects the implementation that trusts it, and because two of them are
mechanisms §14 depends on that cannot work as written.

The pattern in the misses is worth naming, since it is the same pattern §18.1–§18.3
record: **this document is most wrong where it reasoned from an API's surface
instead of its behavior** — `WriteTimeout` looks per-request and is not,
`sqlite3_interrupt` looks observable and is not, `QueryContext` looks like a
call-site change and is a signature change across a package. That is the same error
as the fourth pass's interrupt claim, which was itself found by checking exported
symbols rather than driver behavior.

**Twelve findings that change the plan:**

| # | Premise | What the code says | Consequence |
|---|---|---|---|
| 1 | A per-route budget is enforced by `http.Server.WriteTimeout` (§7.1, §14.12) | `WriteTimeout` is a single server-global field applied once per connection (`httpapi/server.go:85`, the only `http.Server` in the API path). It cannot carry a per-route value, **and setting it would terminate the SSE stream** (`eventstream.go:936`) and large artifact downloads | §7.1 rewritten: `context.WithTimeout` for the work plus **`http.NewResponseController(w).SetWriteDeadline`** for the socket — a per-request primitive already used in-tree at `eventstream.go:982`. No global `WriteTimeout`. `Route.Budget` gains a declared unbounded disposition for `/events`, and byte-serving routes are budgeted on work-before-first-byte |
| 2 | `SQLITE_INTERRUPT` maps to 503 (§7.1, §7.2, §14.6) | **The interrupt is never observable.** `interruptOnDone` does fire (`sqlite.go:78`), but `stmt.go:108`–`112` immediately rewrites the result to `ctx.Err()`. The caller sees `context.DeadlineExceeded` — never a SQLite code. Worse, today `DeadlineExceeded` falls through to **500 `read_error`** because `clientCancelled` (`router.go:486`) keys only on `context.Canceled` | The asserted *property* is unchanged and still right. The **discriminant** changes: `DeadlineExceeded` → 503 + `Retry-After`, `Canceled` → the existing 499. Also: an interrupted file-backed connection is **discarded** by `database/sql` (`conn.go:950`–`967`), so every budget expiry costs a reconnect — which turns shed-at-admission from a preference into a resource argument |
| 3 | "Every request query uses `QueryContext`/`ExecContext`"; Wave 4 risk "low — touches router and contract" (§7.1, §17) | **Zero** `QueryContext`/`ExecContext`/`QueryRowContext`/`PrepareContext` call sites exist in the repository, and `internal/telemetry/rollup` has **no `context.Context` in any non-test signature** — ~36 exported methods across ~12 files | Reclassified. The plumbing is a **prerequisite of Wave 1.5**, not part of Wave 4, and it is the widest mechanical diff in the plan. Wave 4 is genuinely low-risk *once it exists*; until then every Wave 4 property is inert |
| 4 | The journal-walking active-run count has 4 call sites (§2.1, §12) | **Six**, including `inventory.go:590` (workflow detail, added by #1894 *after* #1741 was filed) and `cmd/goobers/status.go:902`. Separately, the exported `localscheduler.ActiveRunCounts` (`reconcile.go:22`) has **no production caller at all** — `ReconcileAll` uses the unexported `activeRuns` (`scheduler.go:315`) | §2.1 now tables all six. **#1741's acceptance widens from three routes to six**, or the Overview's own list path stays on the 17.2 s walk. And §12 should **delete** `ActiveRunCounts` rather than "retain it as the cold-start primitive" — `activeRuns` is the primitive |
| 5 | §6.6 step 4: drop the "unused" `runs` table; §5.1: `telemetry.db` is analytics | `runs` has **32 non-test SQL references across 8 files**, and **18 of the store's 22 tables are per-run** (`ingest.go:76`) | Step 4 is a migration across most of the analytics query surface, not a cleanup. The store split is unaffected (§5.1's argument is about *rebuild input*), but `runs` in `telemetry.db` becomes a narrow **join key** while the full row moves to `read.db`, and step 4 retires superseded *columns* with a named owner |
| 6 | Both stores are deletable and rebuildable from journals (§3.2); rebuild is byte-identical (§14.9) | **`first_success_milestones` is not journal-derivable.** v14 seeds it from projected rows (`schema.go:423`) and `RebuildAll` preserves it by reading the **old database** before deleting it (`rebuild.go:31`, replayed at `:44`) | §3.2 gains its one exception, generalized into a rule: **a rebuild must carry forward every derived fact not reconstructible from journals, enumerated in code.** This is the same rule §6.5 step 2 already states for the projection floor and tombstones. §14.9's byte-identity claim is scoped to journal-derived rows |
| 7 | Four writers (§15.1) | **Six.** `goobers run abort` appends a terminal `run.finished` with **no `up.lock`, no delegation, and no index write** (`run.go:349`); `goobers journal redact` reopens a run for append | Intake coverage that omits them is incomplete by construction, and `run abort` is the one writer that can mutate a journal *concurrently with a live daemon* while writing a query-visible terminal transition |
| 8 | §2.4: the read path locks directories that can never be ingested | **#1708 closed that path before the measurement was taken** (`runs.go:918`, `if !journal.Recorded(runDir) { continue }`). The 10,906 stray locks are residue | §2.4 rewritten as archaeology. The surviving defect is narrower and still real — a read path taking a journal lock and running an 18-table delete-and-insert inside an HTTP GET, for recorded-but-unindexed runs — and Wave 3's exit is written against **that** |
| 9 | The closed filter set is enforced because "the UI can only construct supported combinations" (§5.7) | The portal parses filters from **arbitrary hash query parameters** (`routing.ts:56`–`67`), validating enum membership but not combinations. Every drill-through target is a bookmarkable `#/runs?…` href, and `population="premium-measured"` is reachable *only* by URL | The set must be closed over **URL-reachable** combinations — a strictly larger space — and §14.2's enumeration test must walk the router's parseable cross-product. The refusal must also **degrade gracefully in the client**, because a stale bookmark is normal, not misuse |
| 10 | Wave 0 is "test-only. Risk: none" (§17) | `test/scale` is under `./...`, so it runs in the `unit` group of every `make ci` and lands in the coverage denominator. `make ci`'s shape is **golden-pinned** (`portability_test.go`: exactly `[deadcode]` + `[$(GO) run ./test/ci]`), the required check's **name** is ruleset-pinned, and the repository has **one** benchmark and no bench tier to extend | Wave 0 is four PRs of real gate surface. Heavy corpora are opt-in and out of the default gate; new checks go *inside* `test/ci` with a group; §5.7's rows-visited benchmarks require **inventing** the tier |
| 11 | The 1× baseline reproduces the live instance (§2.5, §16) | Measured: the coded 1× yields a **76 MiB** scheduler journal against the live **324 MB**. Two §16.5 pathologies are **inverted** — orphan dirs are fixed at 5 and carry *no* `.lock`, and span payloads are ~45 bytes. Generation is fsync-bound: **6.9 runs/s** with fsync versus **124/s** without | §16 re-anchors and date-stamps the baseline, fixes both pathologies, and generates fixtures by writing journals directly. Also: **88.8% of the live journal's 324 MB is residue of already-fixed #1414**, so Wave 1.1's before/after must be published against a compacted corpus too, or it measures the residue |
| 12 | `ResumeFromTerminal` is a live writer needing a floor override (§6.3) | **Zero production callers** — the CLI surfaces are stubs pending #465–#469 | The rule is right and stays, implemented as a property of the intake protocol (a watermark is authority to re-admit) rather than wired to a caller that does not exist |

**Three live defects found in the audit that this document did not name.** None are
caused by the design; all are in code it touches, and each would otherwise surface
as unexplained "drift" once the differential oracle runs:

- **`goobers telemetry compact` can permanently break scheduler ingest.**
  `CompactInstanceEvents` rewrites `scheduler/events.jsonl` shorter while preserving
  the surviving records' `seq`, but `IngestSchedulerLog` advances a **byte-offset**
  cursor. A projector must key its cursor on `seq`, not offset.
- **A run terminalized by the stalled-run sweep never has its rollup rows
  refreshed.** `sweepStalledRuns` (`stalledruns.go:85`) holds no store handle.
- **`advanceStreams` mutates each stream's cursor before the caller checks for
  failure** (`runsHistory.ts:243`–`244`), so a failed page can advance a cursor past
  data that was never delivered.

**Citations corrected** (no semantic change): §2.1 `runs.go:366` → the call is
`runs.go:529` via `:371`; §5.4's "finish-time update with a swallowed error at
`runs.go:921`" → `:921` is the *backfill*'s `IngestRun`, and the finish-time write
is on the writer path at `runnerwiring.go:145`–`167`; §12's `eventstream.go` "~1,000
lines" → the file is 1,009 but the deletion is ~640 (`:107`–`742`); §11A's
`LiveFreshness → DaemonQueryState` → the indicator is `PortalShell.tsx:97`–`131`
(`DaemonQueryState.tsx` is 33 lines and never reads freshness); §11A's "a spinner
that dies at 10 s with no reason" → the client *does* show a reason
(`errors.ts:44`), what is missing is a **server-supplied cause**; §11A's
`InsightPage.tsx:1051`–`1073` → `:1045`–`1062` and `:1064`–`1076`; §7.3's "50/200
limits" → inventory is 50/**100**, and the client never requests 200; §2.2's "13
scheduler call sites" → one physical `Append` (`scheduler.go:1521`) behind two
wrappers fed by 12 sites; §2.2's "there is no open issue for it" → #1914; and the
Status block's "fourth pass" → sixth.

**Also settled here, with the product owner, rather than left open:** the two
§17 Wave 6 product decisions. **Full-fidelity retention is configurable with a
90-day default** — a no-op on the measured instance, whose oldest run directory is
two weeks old — under a key named distinctly from journal retention. **Freshness
defaults to serve-labelled-stale**, with `strictFreshness` per instance and
`If-Source-Applied` per request. §14.12's one permitted revision is spent in Wave 0
against a **declared reference host**, since none existed and the CI runners are
contended shards that cannot defend a p99.9.

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

### 18.1a Fourth-pass amendments (convergence review)

Accepted as design authority with three non-blocking amendments and one
consistency fix, all taken here. Three of the four close holes the fourth pass
itself introduced.

| Amendment | Why | Resolution |
|---|---|---|
| A newer observation must cancel stale removal intent | Retention crashing between intent and unlink leaves `removing = 1`; if the run then advances or is resumed, the projector takes the removal branch and deletes a **live** run's rows. A higher `source_seq` is proof the intent is stale | §4.3 — the upsert clears `removing` when the incoming watermark advances. The one place a writer overrides retention, and the correct direction. Fixture: "run advances while removal intent is pending." Closes before Wave 3 |
| A rebuilt epoch must inherit the projection floor and tombstones | They are derived *policy* state in the old `read.db`, not journal facts, so a rebuild from journals alone does not reproduce them — briefly re-admitting every expired journal, bursting removals and the change feed, and making rebuild size proportional to total history rather than the retained window. That defeats §14.12's rebuild-time and store-size targets | §6.5 step 2 — copy floor and tombstones **before** projecting any journal, and apply the floor while building. Gates Wave 6 |
| The change-retention pin must release on **every** terminal rebuild outcome | The pin was specified without a release rule, so an aborted or discarded rebuild blocks `change` pruning indefinitely and the table grows without bound | §6.5 — release on success, abort, discard, and startup recovery of a stale build; the pin is recorded with its build so startup can drop an orphan. Added to kill-at-every-boundary coverage |
| §6.1 still listed the watermark ack inside the serialized commit tuple | Contradicted §4.3/§6.2, which had correctly moved it to a guarded post-commit write in `intake.db` | §6.1 — tuple is now `(run row, run_stage rows, population rollups, change row, last_projected_seq)` in one `read.db` transaction, with the ack explicitly outside it |

Added in the same pass, not from review: **§11A**, stating what changes on screen
and naming the two places this design does not achieve strict UX parity — the
enumerated filter-combination set and the 90-day window.

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
