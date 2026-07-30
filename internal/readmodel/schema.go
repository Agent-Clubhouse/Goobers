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
}
