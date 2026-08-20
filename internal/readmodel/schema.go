package readmodel

// migrations is the ordered, append-only list of forward migrations applied on
// Open. Never edit a migration once released — append a new one.
// projection_state.schema_version tracks how many have run.
//
// This is deliberately a SEPARATE list from internal/telemetry/rollup's. The two
// stores have different lifecycles: read.db is small, rebuilt as a whole, and
// gated on 191 MB of run events, while telemetry.db is 547 MB gated on 2,263 MB
// of spans (design §5.1). Sharing a migration counter would couple a read-model
// rebuild to an analytics schema change for no reason.
var migrations = []string{
	// v1: the run read model.
	//
	// # Why a separate store at all
	//
	// §5.1: list data is 191 MB and analytics data is 2,263 MB — a 12x difference
	// in rebuild cost between "the data the product IS" and "the data that
	// decorates it". Splitting them means cold start is gated on the former, the
	// two get independent retention policies (which they already need), and
	// read.db stays small enough that rebuild is a whole-store operation.
	//
	// # Why the run row is complete
	//
	// Every column here exists so a list page can be answered with ZERO journal
	// opens (§14.2). Measured today: a 50-row page opens 51 journals, one per
	// returned row, because the row in telemetry.db is not complete enough to
	// answer the list contract and each run must be hydrated from its journal.
	//
	// # Stored phase
	//
	// phase is a STORED, INDEXED fact rather than a reconstruction (§5.4).
	// Deriving it means replaying a run's whole event log, including in the path
	// that counts live runs across all history — measured at 17.2 s to answer
	// "2". It is also what makes a lagging run detectably dirty rather than
	// silently miscategorised, which is the class of defect #1943 is an instance
	// of: removing a completed run's journal currently reclassifies it as
	// *running*, because an absent log replays to zero events.
	`
CREATE TABLE IF NOT EXISTS run (
	run_id            TEXT PRIMARY KEY,
	gaggle            TEXT NOT NULL,
	workflow          TEXT NOT NULL,
	workflow_version  INTEGER NOT NULL DEFAULT 0,
	workflow_digest   TEXT,
	goober_digest     TEXT,
	trigger_kind      TEXT,
	trigger_ref       TEXT,

	-- Execution state. phase is stored rather than derived (§5.4).
	phase             TEXT NOT NULL,
	terminal          INTEGER NOT NULL DEFAULT 0,
	current_stage     TEXT,
	started_at        TEXT NOT NULL,
	finished_at       TEXT,
	last_activity_at  TEXT,
	-- last_seq is the highest journal sequence projected into this row. It is
	-- the per-run source position (§4.1) and what makes projection idempotent.
	last_seq          INTEGER NOT NULL DEFAULT 0,

	repass_count      INTEGER NOT NULL DEFAULT 0,
	retry_count       INTEGER NOT NULL DEFAULT 0,
	policy_retry_count INTEGER NOT NULL DEFAULT 0,
	infra_retry_count INTEGER NOT NULL DEFAULT 0,

	-- Terminal business outcome, the second axis distinct from phase (#851).
	outcome_verdict   TEXT,
	outcome_target    TEXT,

	-- disposition is reserved, not yet populated: 'produced' | 'no-work' |
	-- 'unknown'. §5.3 keeps the column so #1429's definition and #1439's filter
	-- are index-pushable rather than a new scan added later. Adding a column to
	-- an ordering index after clients depend on a cursor format is exactly the
	-- retrofit §5.5 warns about.
	disposition       TEXT NOT NULL DEFAULT 'unknown'
);

-- duration is deliberately NOT stored. A running run's duration is now-relative,
-- so a stored value would freeze the moment it was projected and a quiet
-- in-flight run would stop ageing (§5.3). It is computed at query time.

-- Ordering indexes. gaggle leads the scoped ones so authorization is a query
-- predicate rather than a post-filter (§5.5): filtering after LIMIT silently
-- omits rows, and filtering before LIMIT without an index reintroduces the scan.
-- The trailing run_id covers the keyset cursor's tiebreak, which is what keeps
-- page N as cheap as page 1.
CREATE INDEX IF NOT EXISTS idx_run_gaggle_recency ON run(gaggle, started_at DESC, run_id ASC);
CREATE INDEX IF NOT EXISTS idx_run_gaggle_workflow_recency ON run(gaggle, workflow, started_at DESC, run_id ASC);
-- The unrestricted fast path, for a principal provably scoped to every gaggle
-- (AllowAll today). Without it the common case pays for a column it does not
-- constrain.
CREATE INDEX IF NOT EXISTS idx_run_recency ON run(started_at DESC, run_id ASC);
-- Phase-scoped recency, which is what makes "how many runs are live" an indexed
-- aggregate instead of a directory walk (§5.4).
CREATE INDEX IF NOT EXISTS idx_run_phase_recency ON run(phase, started_at DESC, run_id ASC);
CREATE INDEX IF NOT EXISTS idx_run_gaggle_phase_recency ON run(gaggle, phase, started_at DESC, run_id ASC);

CREATE TABLE IF NOT EXISTS run_stage (
	run_id            TEXT NOT NULL,
	stage             TEXT NOT NULL,
	attempts          INTEGER NOT NULL DEFAULT 0,
	last_status       TEXT,
	last_attempt_class TEXT,
	started_at        TEXT,
	finished_at       TEXT,
	PRIMARY KEY (run_id, stage)
);

CREATE INDEX IF NOT EXISTS idx_run_stage_stage ON run_stage(stage, run_id);

-- projection_state is one row (id = 1) holding the store's own identity and
-- progress. §5.3: it replaces IndexedRunIDs(), which materializes every run id
-- into a Go map on the request path, and it is what lets a read ESTABLISH
-- completeness rather than assume it.
CREATE TABLE IF NOT EXISTS projection_state (
	id                 INTEGER PRIMARY KEY CHECK (id = 1),
	schema_version     INTEGER NOT NULL,
	-- epoch is an opaque identity minted fresh on every build (§4.2). It is
	-- load-bearing, not bookkeeping: a rebuilt read.db is a new SQLite file, so
	-- AUTOINCREMENT restarts at 1, and without an epoch a client holding cursor
	-- 918342 reconnects to a store whose maximum is 100 — neither below the
	-- retention floor nor a schema change, so no named condition fires and the
	-- client waits forever.
	--
	-- Equality semantics, not ordering: any inequality is 'epoch_changed' and
	-- instructs a snapshot refetch.
	epoch              TEXT NOT NULL,
	-- min_change_seq is the oldest retained change row — a defined retention
	-- floor rather than an implication (§4.2), so 'feed_truncated' is a
	-- persisted comparison instead of "roughly old".
	min_change_seq     INTEGER NOT NULL DEFAULT 0,
	-- projection_floor is the point below which runs are intentionally aged out
	-- (§6.3/§11.4). Repair skips journals older than it, which is what stops the
	-- retention livelock: without a floor, repair reprojects an aged-out run,
	-- retention deletes it, and the next cycle repeats.
	projection_floor   TEXT,
	last_sweep_at      TEXT,
	built_at           TEXT NOT NULL
);
`,

	// v2: the change feed (#1919, §4.2).
	//
	// One row per projected transition, written in the SAME transaction as the
	// fact it describes. That ordering is the whole point: today the projection
	// updates on run *finish* while the stream discovers change by polling the
	// filesystem — two mechanisms with different latency, completeness, and
	// failure modes, so "refetch" and "the data is there" can arrive out of
	// order. Committing them together makes that impossible rather than unlikely.
	//
	// The cursor is <schemaVersion>:<epoch>:<seq>, not seq alone. A rebuilt
	// read.db is a NEW SQLite file, so AUTOINCREMENT restarts at 1 — and a client
	// holding seq 918342 would reconnect to a store whose maximum is 100. That
	// cursor is neither below the retention floor nor from a different schema
	// version, so no named condition fires, the client waits forever, and §8.2's
	// rule discarding lower positions makes it permanent. The epoch is what turns
	// that silent hang into a named `epoch_changed`.
	//
	// AUTOINCREMENT (rather than a bare INTEGER PRIMARY KEY) is deliberate:
	// SQLite reuses rowids after a delete without it, and change pruning deletes
	// from the head of the table. A reused seq would hand two different
	// transitions the same cursor position.
	`
CREATE TABLE IF NOT EXISTS change (
	seq      INTEGER PRIMARY KEY AUTOINCREMENT,
	at       TEXT NOT NULL,
	kind     TEXT NOT NULL,
	run_id   TEXT,
	gaggle   TEXT,
	workflow TEXT
);

-- Resuming a per-run stream, and finding a run's latest transition.
CREATE INDEX IF NOT EXISTS idx_change_run ON change(run_id, seq);
-- Scoped tails: a client watching one gaggle should not walk another's changes.
CREATE INDEX IF NOT EXISTS idx_change_gaggle ON change(gaggle, seq);
`,

	// v3: the latest-terminal-outcome aggregate (#1891, §5.2).
	//
	// The Workflows page asks for each workflow's most recent TERMINAL run — the
	// last time it actually finished. The ordering indexes cover
	// (gaggle, workflow, started_at DESC, run_id), so adding `terminal = 1` to
	// that query makes terminality a RESIDUAL predicate: SQLite reports
	// `SEARCH run USING INDEX idx_run_gaggle_workflow_recency` while silently
	// evaluating the terminal test per row, which is exactly the shape §5.7 says
	// a plan cannot be trusted to reveal.
	//
	// It happens to be a cheap residual today, because almost every run is
	// terminal and the newest one usually is. That is a property of the current
	// corpus, not of the query: a workflow whose recent history is a run of
	// in-flight or abandoned attempts walks every one of them before it finds an
	// outcome, and nothing reports that it did. §5.7's discipline is that the
	// bound comes from a declared index rather than from a hope about the data,
	// so terminality moves into a partial index and stops being residual.
	//
	// A partial index rather than a fourth column: `terminal` is a constant
	// within the index, so it costs no key bytes, and only terminal rows are
	// indexed at all — roughly the whole table today, but the write cost tracks
	// what the query actually reads.
	`
CREATE INDEX IF NOT EXISTS idx_run_terminal_gaggle_workflow_recency
	ON run(gaggle, workflow, started_at DESC, run_id ASC)
	WHERE terminal = 1;

-- The active-run count the concurrency ceiling is compared against, as an
-- indexed aggregate rather than a directory walk (§5.4). Partial on the
-- running phase because that is the only value it is ever asked about, and
-- running rows are a small minority of the table.
CREATE INDEX IF NOT EXISTS idx_run_running_gaggle_workflow
	ON run(gaggle, workflow)
	WHERE phase = 'running';
`,

	// v4: the repair sweep's durable cursor, unpublished memo, and tombstones
	// (#1924, §6.3).
	`
-- sweep_cursor is where the continuous repair walk resumes.
--
-- §6.3's central correction: the old reasoning — "a run root whose mtime has not
-- advanced cannot hold anything new, so skip it without a ReadDir" — DOES NOT
-- BIND. Every new run bumps its parent's mtime, so on a live instance the root
-- is always dirty and repair reads all 40,665 entries every pass. A bound that
-- only holds when nothing is happening is not a bound.
--
-- The replacement is a fixed I/O budget, walked continuously and cycling. Cost
-- is constant per unit time; what scales with history is CYCLE TIME (H / rate),
-- which is reported rather than hidden. That only works if the walk can stop
-- anywhere and resume, which is what this is.
CREATE TABLE IF NOT EXISTS sweep_cursor (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	-- root is the runs directory being walked, after_name the last entry
	-- examined within it. Together they are a resumable position in a
	-- lexicographic walk.
	root       TEXT NOT NULL DEFAULT '',
	after_name TEXT NOT NULL DEFAULT '',
	-- These turn "how stale can repair be" into a number the freshness surface
	-- can report, rather than a promise.
	cycle_started_at        TEXT,
	last_cycle_completed_at TEXT,
	-- entries_this_cycle counts work done, so a cycle that is not converging
	-- shows up as a rising count with no completion.
	entries_this_cycle INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO sweep_cursor (id) VALUES (1);

-- unpublished remembers directories with no run.yaml, keyed by directory mtime.
--
-- 10,906 of 40,665 directories on the live instance are unpublished (27%) and
-- can never be ingested. Remembering them costs one stat per cycle instead of an
-- open. Writing run.yaml bumps the directory mtime, so promotion is detected
-- rather than cached forever.
CREATE TABLE IF NOT EXISTS unpublished (
	run_id    TEXT PRIMARY KEY,
	dir_mtime TEXT NOT NULL,
	seen_at   TEXT NOT NULL
);

-- tombstone records a run deliberately aged out below the projection floor.
--
-- Without it, "missing" and "deliberately gone" are the same observation, and
-- repair cannot tell an operator rm from ordinary retention. It is also what
-- stops the livelock: repair skips a journal below the floor instead of
-- reprojecting an aged-out run for retention to delete again next cycle.
CREATE TABLE IF NOT EXISTS tombstone (
	run_id        TEXT PRIMARY KEY,
	started_at    TEXT NOT NULL,
	tombstoned_at TEXT NOT NULL,
	reason        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tombstone_started ON tombstone(started_at);
`,

	// v5: day and month aggregate buckets (#1931, §5.6).
	//
	// Without pre-aggregation an all-time Insight query scans all matching
	// history, so "zero timeouts" does not hold on the analytics surface. A day
	// bucket makes even the widest window a bounded number of rows.
	//
	// # Recompute, not accumulate
	//
	// When a run in day D changes, D is marked dirty and its buckets are
	// RECOMPUTED by aggregating that day's indexed run rows. Bounded by runs in
	// a day, and idempotent — which serves the determinism property for free.
	//
	// Reversible deltas were considered and rejected: they require storing each
	// run's prior contribution and subtracting it on reprojection, which is
	// fiddly, easy to get wrong, and silently drifts when it is. A recompute
	// cannot drift, because it does not carry state between passes.
	`
CREATE TABLE IF NOT EXISTS bucket_day (
	day         TEXT NOT NULL,
	gaggle      TEXT NOT NULL,
	workflow    TEXT NOT NULL,
	phase       TEXT NOT NULL,
	outcome     TEXT NOT NULL DEFAULT '',
	runs        INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, gaggle, workflow, phase, outcome)
);

-- Recency-ordered so a windowed query seeks rather than scans, and gaggle-led
-- for the same reason the run indexes are: scope has to be a predicate inside
-- the indexed query, never a filter applied after LIMIT.
CREATE INDEX IF NOT EXISTS idx_bucket_day_recency ON bucket_day(day DESC, gaggle, workflow);
CREATE INDEX IF NOT EXISTS idx_bucket_day_gaggle ON bucket_day(gaggle, day DESC);

-- Month rollups recompute from the dailies on the same rule, so the same
-- idempotence argument covers both tiers.
CREATE TABLE IF NOT EXISTS bucket_month (
	month       TEXT NOT NULL,
	gaggle      TEXT NOT NULL,
	workflow    TEXT NOT NULL,
	phase       TEXT NOT NULL,
	outcome     TEXT NOT NULL DEFAULT '',
	runs        INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (month, gaggle, workflow, phase, outcome)
);
CREATE INDEX IF NOT EXISTS idx_bucket_month_recency ON bucket_month(month DESC, gaggle, workflow);

-- dirty_day is the recompute queue.
--
-- A queue rather than a recompute-on-write: a run finishing writes one row here
-- instead of re-aggregating its whole day inside the projection transaction,
-- which would put an O(runs-in-day) scan on the commit path. It is also what
-- lets bucket recompute be the FIRST thing shed under projector overload
-- (PRA-3b) — the work is durable and can wait, because it is derived from data
-- that is itself already durable.
CREATE TABLE IF NOT EXISTS dirty_day (
	day     TEXT PRIMARY KEY,
	marked_at TEXT NOT NULL
);
`,

	// v6: stage, stage-outcome, and population pushdown (#1782, §5.7).
	//
	// These three were the residual predicates the closed set refused. Serving
	// them meant hydrating every candidate journal AFTER the index had picked it,
	// with no cap on candidates — so a filter matching few runs walked the entire
	// indexed history. On the live instance that is ~19,852 journal opens, ~143 MB
	// read and ~1M unmarshals, in ONE request.
	//
	// # Why the columns land on run_stage, not only on run
	//
	// The filters are stage-SCOPED. `stage=build&population=cost-measured` asks
	// whether the BUILD stage has cost recorded, not whether the run has cost
	// anywhere. A run-level rollup answers the unscoped question only, so using it
	// for the scoped one would silently WIDEN the filter — returning runs whose
	// cost came from some other stage. Both grains are stored: run_stage answers
	// the scoped question, and run carries the OR across stages so the unscoped
	// question is a direct seek rather than a correlated subquery.
	//
	// # Why gaggle and the run's started_at are duplicated onto run_stage
	//
	// A stage-filtered list is still GAGGLE-scoped and still ordered by RUN
	// recency. Without both columns here, a stage-scoped query drives from
	// run_stage and then evaluates gaggle and the ordering against the joined run
	// row — which is a residual predicate plus a sort, the exact shape §5.7
	// exists to refuse. §5.5 additionally requires gaggle to be a query predicate
	// rather than a post-filter, so a gaggle-scoped principal that could not
	// combine gaggle with stage would simply lose stage filtering.
	//
	// run_stage.started_at is the STAGE's own start and is a different clock, so
	// it cannot serve run-recency ordering. Hence run_started_at alongside it.
	//
	// The duplication cannot drift: run_stage rows are deleted and rewritten
	// wholesale on every projection of their run (write.go), rather than
	// incrementally maintained.
	//
	// # On the index count
	//
	// Sixteen indexes is the honest price of a closed set: §5.7 requires every
	// supported combination to ship a covering index, and refusing to pay it
	// would mean either fewer answers or a residual predicate pretending to be
	// one. Twelve of the sixteen are PARTIAL — they contain only rows where a
	// measurement flag is set, which is a minority of stage rows — so the write
	// amplification is far below what the count suggests.
	//
	// The partial predicates are LITERAL (`= 1`). SQLite requires that; a bound
	// parameter makes a partial index unusable, and it does not say so when it
	// declines to use one.
	`
ALTER TABLE run_stage ADD COLUMN gaggle TEXT NOT NULL DEFAULT '';
ALTER TABLE run_stage ADD COLUMN run_started_at TEXT;
ALTER TABLE run_stage ADD COLUMN token_measured INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_stage ADD COLUMN premium_measured INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_stage ADD COLUMN cost_measured INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_stage ADD COLUMN retry_waste INTEGER NOT NULL DEFAULT 0;

ALTER TABLE run ADD COLUMN any_token_measured INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run ADD COLUMN any_premium_measured INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run ADD COLUMN any_cost_measured INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run ADD COLUMN any_retry_waste INTEGER NOT NULL DEFAULT 0;

-- Stage-scoped ordering indexes. Each leads with its equality columns and then
-- the run's recency, so a page is a seek plus limit+1 rows.
CREATE INDEX IF NOT EXISTS idx_run_stage_recency
	ON run_stage(stage, run_started_at DESC, run_id ASC);
CREATE INDEX IF NOT EXISTS idx_run_stage_gaggle_recency
	ON run_stage(gaggle, stage, run_started_at DESC, run_id ASC);

-- One partial index per population, at each grain. Partial rather than a
-- four-column composite because the predicates are independent booleans: a
-- composite would have to be probed with three wildcards, which is a scan of the
-- whole stage range.
CREATE INDEX IF NOT EXISTS idx_run_stage_token
	ON run_stage(stage, run_started_at DESC, run_id ASC) WHERE token_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_gaggle_token
	ON run_stage(gaggle, stage, run_started_at DESC, run_id ASC) WHERE token_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_premium
	ON run_stage(stage, run_started_at DESC, run_id ASC) WHERE premium_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_gaggle_premium
	ON run_stage(gaggle, stage, run_started_at DESC, run_id ASC) WHERE premium_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_cost
	ON run_stage(stage, run_started_at DESC, run_id ASC) WHERE cost_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_gaggle_cost
	ON run_stage(gaggle, stage, run_started_at DESC, run_id ASC) WHERE cost_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_retry_waste
	ON run_stage(stage, run_started_at DESC, run_id ASC) WHERE retry_waste = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_gaggle_retry_waste
	ON run_stage(gaggle, stage, run_started_at DESC, run_id ASC) WHERE retry_waste = 1;
CREATE INDEX IF NOT EXISTS idx_run_any_token
	ON run(started_at DESC, run_id ASC) WHERE any_token_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_gaggle_any_token
	ON run(gaggle, started_at DESC, run_id ASC) WHERE any_token_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_any_premium
	ON run(started_at DESC, run_id ASC) WHERE any_premium_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_gaggle_any_premium
	ON run(gaggle, started_at DESC, run_id ASC) WHERE any_premium_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_any_cost
	ON run(started_at DESC, run_id ASC) WHERE any_cost_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_gaggle_any_cost
	ON run(gaggle, started_at DESC, run_id ASC) WHERE any_cost_measured = 1;
CREATE INDEX IF NOT EXISTS idx_run_any_retry_waste
	ON run(started_at DESC, run_id ASC) WHERE any_retry_waste = 1;
CREATE INDEX IF NOT EXISTS idx_run_gaggle_any_retry_waste
	ON run(gaggle, started_at DESC, run_id ASC) WHERE any_retry_waste = 1;
`,

	// v7: the last-activity recency axis (#1777).
	//
	// # Why a second axis rather than a different default
	//
	// The two orderings answer different operator questions and both are wanted.
	// started_at is when a run BEGAN, which is what a history page shows.
	// last_activity_at is when it last DID something, which is what an attention
	// list needs — a run that started days ago and escalated a minute ago is the
	// most urgent thing on the instance and the least recent by started_at.
	//
	// #1199 could not be built portal-side precisely because of that: a run with
	// an old start is excluded from a bounded page BEFORE the portal sees it, and
	// no client-side filter can recover a row that was never sent.
	//
	// # Why these mirror the started_at indexes exactly
	//
	// A keyset page needs its equality columns first and its ordering column
	// last. Reusing the started_at indexes for an activity-ordered query would
	// give the planner an index that seeks the right rows and then has to SORT
	// them — bounded in returned rows, unbounded in examined ones, which is the
	// exact failure §5.7's closed set exists to prevent.
	//
	// NULL last_activity_at (a run projected from identity with no events) sorts
	// last under DESC and is excluded by any since-filter. That is the correct
	// semantic — no recorded activity is not recent activity — and it is why
	// these are not COALESCE expressions, which would make the index unusable.
	`
CREATE INDEX IF NOT EXISTS idx_run_activity
	ON run(last_activity_at DESC, run_id ASC);
CREATE INDEX IF NOT EXISTS idx_run_gaggle_activity
	ON run(gaggle, last_activity_at DESC, run_id ASC);
CREATE INDEX IF NOT EXISTS idx_run_gaggle_workflow_activity
	ON run(gaggle, workflow, last_activity_at DESC, run_id ASC);
CREATE INDEX IF NOT EXISTS idx_run_phase_activity
	ON run(phase, last_activity_at DESC, run_id ASC);
CREATE INDEX IF NOT EXISTS idx_run_gaggle_phase_activity
	ON run(gaggle, phase, last_activity_at DESC, run_id ASC);
`,

	// v8: stage-scoped outcome pushdown (#2091).
	//
	// last_status cannot answer this filter: the journal-derived contract matches
	// when ANY attempt has the requested status. The cumulative flags preserve
	// exactly that predicate without copying attempt history into read.db.
	// run_terminal is duplicated because every outcome-filtered list excludes
	// in-flight runs, and a predicate on the joined run row would be residual.
	`
ALTER TABLE run_stage ADD COLUMN had_success INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_stage ADD COLUMN had_failure INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_stage ADD COLUMN had_other INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_stage ADD COLUMN run_terminal INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projection_state ADD COLUMN ready INTEGER NOT NULL DEFAULT 1;

CREATE INDEX IF NOT EXISTS idx_run_stage_outcome_success
	ON run_stage(stage, run_started_at DESC, run_id ASC)
	WHERE run_terminal = 1 AND had_success = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_gaggle_outcome_success
	ON run_stage(gaggle, stage, run_started_at DESC, run_id ASC)
	WHERE run_terminal = 1 AND had_success = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_outcome_failure
	ON run_stage(stage, run_started_at DESC, run_id ASC)
	WHERE run_terminal = 1 AND had_failure = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_gaggle_outcome_failure
	ON run_stage(gaggle, stage, run_started_at DESC, run_id ASC)
	WHERE run_terminal = 1 AND had_failure = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_outcome_other
	ON run_stage(stage, run_started_at DESC, run_id ASC)
	WHERE run_terminal = 1 AND had_other = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_gaggle_outcome_other
	ON run_stage(gaggle, stage, run_started_at DESC, run_id ASC)
	WHERE run_terminal = 1 AND had_other = 1;
CREATE INDEX IF NOT EXISTS idx_run_stage_outcome_terminal
	ON run_stage(stage, run_started_at DESC, run_id ASC)
	WHERE run_terminal = 1 AND (had_success = 1 OR had_failure = 1);
CREATE INDEX IF NOT EXISTS idx_run_stage_gaggle_outcome_terminal
	ON run_stage(gaggle, stage, run_started_at DESC, run_id ASC)
	WHERE run_terminal = 1 AND (had_success = 1 OR had_failure = 1);
CREATE INDEX IF NOT EXISTS idx_run_stage_outcome_finished
	ON run_stage(stage, run_started_at DESC, run_id ASC)
	WHERE run_terminal = 1 AND (had_success = 1 OR had_failure = 1 OR had_other = 1);
CREATE INDEX IF NOT EXISTS idx_run_stage_gaggle_outcome_finished
	ON run_stage(gaggle, stage, run_started_at DESC, run_id ASC)
	WHERE run_terminal = 1 AND (had_success = 1 OR had_failure = 1 OR had_other = 1);

-- Existing rows cannot be backfilled from last_status without losing earlier
-- attempts. Marking the projection unready before emptying it makes startup
-- rebuild it from the authoritative journals before projected reads are enabled.
UPDATE projection_state SET ready = 0 WHERE id = 1;
DELETE FROM run_stage;
DELETE FROM run;
`,

	// v9: graph nodes used by cross-run credit assignment. This is separate
	// from run_stage because gates are nodes but are not stage attempts.
	`
CREATE TABLE IF NOT EXISTS run_node (
	run_id               TEXT NOT NULL,
	kind                 TEXT NOT NULL,
	name                 TEXT NOT NULL,
	identity             TEXT NOT NULL DEFAULT '',
	attempts             INTEGER NOT NULL DEFAULT 0,
	retry_waste_attempts INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, kind, name, identity)
);
CREATE INDEX IF NOT EXISTS idx_run_node_identity
	ON run_node(kind, name, identity, run_id);

UPDATE projection_state SET ready = 0 WHERE id = 1 AND ready <> 0;
DELETE FROM run_stage WHERE TRUE;
DELETE FROM run WHERE TRUE;
`,

	// v10: durable keyset cursor for the projected-to-journal repair direction.
	`
ALTER TABLE sweep_cursor ADD COLUMN reverse_after_started_at TEXT;
ALTER TABLE sweep_cursor ADD COLUMN reverse_after_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sweep_cursor ADD COLUMN reverse_cycle_before TEXT;
CREATE INDEX IF NOT EXISTS idx_run_oldest ON run(started_at ASC, run_id ASC);
`,

	// v11: durable direction alternation for a one-entry repair batch.
	`
ALTER TABLE sweep_cursor ADD COLUMN forward_next INTEGER NOT NULL DEFAULT 0;
`,

	// v12: covering active-run counts by workflow.
	`
CREATE INDEX IF NOT EXISTS idx_run_phase_workflow
	ON run(phase, gaggle, workflow);
`,

	// v13: operator-facing facts complete the zero-journal-open run list row.
	`
ALTER TABLE run ADD COLUMN operator_json TEXT NOT NULL DEFAULT '{}';
UPDATE projection_state SET ready = 0 WHERE id = 1 AND ready <> 0;
DELETE FROM run_node WHERE TRUE;
DELETE FROM run_stage WHERE TRUE;
DELETE FROM run WHERE TRUE;
`,
}
