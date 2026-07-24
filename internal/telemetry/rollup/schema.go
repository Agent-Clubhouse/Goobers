package rollup

// migrations is the ordered, append-only list of forward migrations applied on
// Open (TEL-032; seeds the V1 upgrade story, #33). Never edit a migration once
// released — append a new one. schema_meta.version tracks how many have run.
var migrations = []string{
	// v1: initial rollup schema. Every table carries run_id so it can be wiped
	// and re-derived per run (IngestRun's delete-then-insert), which is also
	// how Rebuild works: wipe everything, IngestRun every run directory.
	`
CREATE TABLE IF NOT EXISTS runs (
	run_id           TEXT PRIMARY KEY,
	workflow         TEXT NOT NULL,
	workflow_version INTEGER NOT NULL,
	workflow_digest  TEXT,
	gaggle           TEXT NOT NULL,
	trigger_kind     TEXT,
	trigger_ref      TEXT,
	status           TEXT,
	started_at       TEXT NOT NULL,
	finished_at      TEXT,
	duration_ms      INTEGER
);

CREATE TABLE IF NOT EXISTS stage_attempts (
	run_id        TEXT NOT NULL,
	stage         TEXT NOT NULL,
	attempt       INTEGER NOT NULL,
	attempt_class TEXT,
	status        TEXT,
	started_at    TEXT,
	finished_at   TEXT,
	duration_ms   INTEGER,
	error_code    TEXT,
	error_class   TEXT,
	runner_json   TEXT,
	PRIMARY KEY (run_id, stage, attempt)
);

CREATE TABLE IF NOT EXISTS gate_verdicts (
	run_id      TEXT NOT NULL,
	seq         INTEGER NOT NULL,
	gate        TEXT NOT NULL,
	verdict     TEXT,
	target      TEXT,
	occurred_at TEXT,
	runner_json TEXT,
	PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS provider_mutations (
	run_id      TEXT NOT NULL,
	seq         INTEGER NOT NULL,
	provider    TEXT NOT NULL,
	kind        TEXT NOT NULL,
	external_id TEXT NOT NULL,
	url         TEXT,
	operation   TEXT,
	occurred_at TEXT,
	runner_json TEXT,
	PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS run_errors (
	run_id      TEXT NOT NULL,
	seq         INTEGER NOT NULL,
	stage       TEXT,
	attempt     INTEGER,
	code        TEXT NOT NULL,
	error_class TEXT,
	message     TEXT,
	occurred_at TEXT,
	PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS spans (
	run_id         TEXT NOT NULL,
	span_id        TEXT NOT NULL,
	parent_span_id TEXT,
	name           TEXT NOT NULL,
	kind           TEXT,
	status         TEXT,
	status_message TEXT,
	start_time     TEXT,
	end_time       TEXT,
	duration_ms    INTEGER,
	PRIMARY KEY (run_id, span_id)
);

CREATE TABLE IF NOT EXISTS span_events (
	run_id          TEXT NOT NULL,
	span_id         TEXT NOT NULL,
	seq             INTEGER NOT NULL,
	name            TEXT NOT NULL,
	occurred_at     TEXT,
	attributes_json TEXT,
	PRIMARY KEY (run_id, span_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_stage_attempts_run    ON stage_attempts(run_id);
CREATE INDEX IF NOT EXISTS idx_gate_verdicts_run      ON gate_verdicts(run_id);
CREATE INDEX IF NOT EXISTS idx_provider_mutations_run ON provider_mutations(run_id);
CREATE INDEX IF NOT EXISTS idx_run_errors_run         ON run_errors(run_id);
CREATE INDEX IF NOT EXISTS idx_spans_run              ON spans(run_id);
CREATE INDEX IF NOT EXISTS idx_span_events_span       ON span_events(run_id, span_id);
`,
	// v2 (issue #128): the signals nomination/Tutor need that v1 silently
	// dropped. harness_transcripts makes within-stage agent transcripts
	// (span.recorded, GBO-020) queryable by pointer (the blob itself stays in
	// the run journal's content-addressed spans/ store — this is an index
	// over it, not a copy). scheduler_events makes "why didn't a run start at
	// tick N" (trigger.fired/tick.skipped/claim.*) queryable at all — v1 never
	// ingested scheduler/events.jsonl, only run directories. Both are plain
	// CREATE TABLE/INDEX IF NOT EXISTS, so — unlike a future ALTER TABLE
	// (tracked separately, #129) — reapplying this migration after a crash
	// mid-batch is inherently safe.
	`
CREATE TABLE IF NOT EXISTS harness_transcripts (
	run_id      TEXT NOT NULL,
	seq         INTEGER NOT NULL,
	stage       TEXT NOT NULL,
	name        TEXT NOT NULL,
	ref_digest  TEXT,
	ref_size    INTEGER,
	occurred_at TEXT,
	PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS scheduler_events (
	seq         INTEGER NOT NULL PRIMARY KEY,
	type        TEXT NOT NULL,
	workflow    TEXT,
	run_id      TEXT,
	reason      TEXT,
	status      TEXT,
	occurred_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_harness_transcripts_run  ON harness_transcripts(run_id);
CREATE INDEX IF NOT EXISTS idx_scheduler_events_workflow ON scheduler_events(workflow);
CREATE INDEX IF NOT EXISTS idx_scheduler_events_run      ON scheduler_events(run_id);
`,
	// v3 (issue #710): spans.status/status_message already exist, but before
	// this issue's Span.Complete fix, every business outcome — success AND
	// failure alike — reported codes.Ok, so status alone couldn't distinguish
	// them even post-fix without inspecting free-text status_message. A
	// dedicated table makes the actual business outcome (success, failed,
	// completed, escalated, aborted...) directly queryable, independent of
	// OTel's own coarser Ok/Error axis. A satellite table (an index over
	// spans, not a widened spans schema) rather than ALTER TABLE spans ADD
	// COLUMN, matching v2's own precedent and its stated reason: unlike
	// CREATE TABLE/INDEX IF NOT EXISTS (idempotent under SQLite's own schema
	// locking, so concurrent first-Opens of a fresh telemetry.db never
	// collide), ALTER TABLE ADD COLUMN is NOT safe against two such
	// first-Opens racing the SAME migration — migrateOnce's transactional
	// pairing only protects a crash between statement and version-bump, not
	// two separate connections each reading schema_meta.version as
	// not-yet-migrated before either commits (confirmed live: this exact
	// migration written as ALTER TABLE flaked "duplicate column name" under
	// TestConcurrentIngestAndQueryUnderWAL's N-way concurrent Open()).
	`
CREATE TABLE IF NOT EXISTS span_business_status (
	run_id          TEXT NOT NULL,
	span_id         TEXT NOT NULL,
	business_status TEXT NOT NULL,
	PRIMARY KEY (run_id, span_id)
);

CREATE INDEX IF NOT EXISTS idx_span_business_status_run ON span_business_status(run_id);
`,
	// v4: instance-journal maintenance failures use the same error envelope as
	// run errors. Preserve their structured fields in a scheduler-keyed table
	// so they can participate in recurring error-signature queries.
	`
CREATE TABLE IF NOT EXISTS scheduler_errors (
	seq          INTEGER NOT NULL PRIMARY KEY,
	code         TEXT NOT NULL,
	error_class  TEXT,
	message      TEXT,
	occurred_at  TEXT
);
`,
	// v5 (issue #779): schema-stamp new transcript writes without altering or
	// backfilling existing harness_transcripts rows. A missing satellite row is
	// the explicit legacy representation.
	`
CREATE TABLE IF NOT EXISTS harness_transcript_schemas (
	run_id TEXT NOT NULL,
	seq    INTEGER NOT NULL,
	schema TEXT NOT NULL,
	PRIMARY KEY (run_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_harness_transcript_schemas_run ON harness_transcript_schemas(run_id);
`,
	// v6 (issue #778): canonical agent usage is carried by task-span
	// attributes and belongs to the same run/stage/attempt identity as the
	// stage_attempts row. A satellite table keeps the migration replay-safe,
	// while nullable columns preserve TEL-041's absent-versus-zero contract;
	// existing attempts have no row rather than being backfilled as zero.
	`
CREATE TABLE IF NOT EXISTS stage_usage (
	run_id                   TEXT NOT NULL,
	stage                    TEXT NOT NULL,
	attempt                  INTEGER NOT NULL,
	input_tokens             INTEGER,
	output_tokens            INTEGER,
	copilot_premium_requests REAL,
	cost_usd                 REAL,
	PRIMARY KEY (run_id, stage, attempt)
);

CREATE INDEX IF NOT EXISTS idx_stage_usage_run ON stage_usage(run_id);
`,
	// v7 (issue #778): task repasses restart their dispatch-local attempt
	// number at one. Add a monotonic per-stage traversal identity so repeated
	// (stage, attempt) pairs remain distinct in both the attempt and usage
	// tables. A legacy rollup may already have collapsed a repeated attempt;
	// only a journal rebuild can recover that row. Rank the surviving rows by
	// start time so migration never mistakes a reset local attempt number for
	// traversal order.
	`
ALTER TABLE stage_attempts RENAME TO stage_attempts_v6;
ALTER TABLE stage_usage RENAME TO stage_usage_v6;

CREATE TABLE stage_attempts (
	run_id        TEXT NOT NULL,
	stage         TEXT NOT NULL,
	traversal     INTEGER NOT NULL,
	attempt       INTEGER NOT NULL,
	attempt_class TEXT,
	status        TEXT,
	started_at    TEXT,
	finished_at   TEXT,
	duration_ms   INTEGER,
	error_code    TEXT,
	error_class   TEXT,
	runner_json   TEXT,
	PRIMARY KEY (run_id, stage, traversal)
);

INSERT INTO stage_attempts (
	run_id, stage, traversal, attempt, attempt_class, status, started_at,
	finished_at, duration_ms, error_code, error_class, runner_json
)
SELECT
	run_id, stage,
	ROW_NUMBER() OVER (
		PARTITION BY run_id, stage
		ORDER BY started_at IS NULL, started_at, attempt
	),
	attempt, attempt_class, status, started_at, finished_at, duration_ms,
	error_code, error_class, runner_json
FROM stage_attempts_v6;

CREATE TABLE stage_usage (
	run_id                   TEXT NOT NULL,
	stage                    TEXT NOT NULL,
	traversal                INTEGER NOT NULL,
	attempt                  INTEGER NOT NULL,
	input_tokens             INTEGER,
	output_tokens            INTEGER,
	copilot_premium_requests REAL,
	cost_usd                 REAL,
	PRIMARY KEY (run_id, stage, traversal)
);

INSERT INTO stage_usage (
	run_id, stage, traversal, attempt, input_tokens, output_tokens,
	copilot_premium_requests, cost_usd
)
SELECT
	su.run_id, su.stage, sa.traversal, su.attempt, su.input_tokens,
	su.output_tokens, su.copilot_premium_requests, su.cost_usd
FROM stage_usage_v6 su
JOIN stage_attempts sa
	ON sa.run_id = su.run_id
	AND sa.stage = su.stage
	AND sa.attempt = su.attempt;

DROP TABLE stage_attempts_v6;
DROP TABLE stage_usage_v6;

CREATE INDEX idx_stage_attempts_run ON stage_attempts(run_id);
CREATE INDEX idx_stage_usage_run ON stage_usage(run_id);
`,
	// v8 (issue #1193): model and harness version are span attributes on every
	// agentic task and reviewer gate. Index them in a satellite table keyed to
	// the source span; task rows also retain stage-attempt traversal identity so
	// aggregate queries do not collapse repasses that restart attempt numbers.
	`
CREATE TABLE IF NOT EXISTS agent_invocations (
	run_id          TEXT NOT NULL,
	span_id         TEXT NOT NULL,
	kind            TEXT NOT NULL,
	stage           TEXT NOT NULL,
	traversal       INTEGER,
	attempt         INTEGER,
	model           TEXT NOT NULL,
	harness_version TEXT NOT NULL,
	PRIMARY KEY (run_id, span_id)
);

CREATE INDEX IF NOT EXISTS idx_agent_invocations_run ON agent_invocations(run_id);
CREATE INDEX IF NOT EXISTS idx_agent_invocations_attempt ON agent_invocations(run_id, stage, traversal);
CREATE INDEX IF NOT EXISTS idx_agent_invocations_model_version ON agent_invocations(model, harness_version);
`,
	// v9 (issue #1208): preserve the model dimension without changing
	// stage_usage's one-row-per-attempt aggregate contract. Existing attempts
	// remain explicitly unmeasured by model because no rows are backfilled.
	`
CREATE TABLE IF NOT EXISTS stage_model_usage (
	run_id                   TEXT NOT NULL,
	stage                    TEXT NOT NULL,
	traversal                INTEGER NOT NULL,
	attempt                  INTEGER NOT NULL,
	model                    TEXT NOT NULL,
	input_tokens             INTEGER,
	output_tokens            INTEGER,
	copilot_premium_requests REAL,
	cost_usd                 REAL,
	PRIMARY KEY (run_id, stage, traversal, model)
);

CREATE INDEX IF NOT EXISTS idx_stage_model_usage_run ON stage_model_usage(run_id);
`,
	// v10 (issue #1192): pin the participating resolved goober definitions
	// alongside the workflow definition so efficacy cohorts can distinguish
	// instruction, skill, model, and harness changes. A satellite table keeps
	// the migration replay-safe and preserves an explicit absence for legacy
	// runs.
	`
CREATE TABLE IF NOT EXISTS run_goober_digests (
	run_id        TEXT PRIMARY KEY,
	goober_digest TEXT NOT NULL
);
`,
	// v11 (CURE-7): project scalar curation outcomes and deterministic ready-pool
	// snapshots from stage.finished outputs. These remain derived from journals,
	// preserving the two-store doctrine while making curation and backlog health
	// directly queryable.
	`
CREATE TABLE IF NOT EXISTS curation_actions (
	run_id             TEXT PRIMARY KEY,
	status             TEXT,
	reported           INTEGER NOT NULL,
	ready_count        INTEGER NOT NULL,
	needs_human_count  INTEGER NOT NULL,
	closed_count       INTEGER NOT NULL,
	deduped_count      INTEGER NOT NULL,
	split_count        INTEGER NOT NULL,
	stale_count        INTEGER NOT NULL,
	reconciled_count   INTEGER NOT NULL,
	milestoned_count   INTEGER NOT NULL,
	bounced_count      INTEGER NOT NULL,
	occurred_at        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ready_pool_samples (
	run_id                    TEXT PRIMARY KEY,
	depth                     INTEGER NOT NULL,
	average_age_seconds       REAL NOT NULL,
	oldest_age_seconds        REAL NOT NULL,
	observed_at               TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ready_claims (
	run_id             TEXT NOT NULL,
	seq                INTEGER NOT NULL,
	item_id             TEXT,
	ready_age_seconds   REAL NOT NULL,
	claimed_at          TEXT NOT NULL,
	PRIMARY KEY (run_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_curation_actions_time ON curation_actions(occurred_at);
CREATE INDEX IF NOT EXISTS idx_ready_pool_samples_time ON ready_pool_samples(observed_at);
CREATE INDEX IF NOT EXISTS idx_ready_claims_time ON ready_claims(claimed_at);
`,
	// v12 (CURE-7): preserve provider-issued ready-label transitions from each
	// curation health snapshot. Snapshots overlap by design; event_id provides
	// lossless deduplication when computing ready cohorts and bounce rates.
	`
CREATE TABLE IF NOT EXISTS ready_label_transitions (
	run_id       TEXT NOT NULL,
	event_id     INTEGER NOT NULL,
	item_id      TEXT NOT NULL,
	transition   TEXT NOT NULL,
	occurred_at  TEXT NOT NULL,
	PRIMARY KEY (run_id, event_id)
);

CREATE INDEX IF NOT EXISTS idx_ready_label_transitions_time ON ready_label_transitions(occurred_at);
CREATE INDEX IF NOT EXISTS idx_ready_label_transitions_item ON ready_label_transitions(item_id, occurred_at);
`,
	// v13 (issue #1411): incremental scheduler-log ingest. Before this, every
	// IngestSchedulerLog deleted and re-inserted scheduler_events and
	// scheduler_errors from the ENTIRE instance journal on every scheduler
	// tick — O(journal size) writes that churned the WAL and, on a large
	// journal, pinned it open (the #1410 dashboard bloat). The cursor stores
	// how far the journal has been ingested (byte offset) plus the highest seq
	// applied, so steady-state ingest reads only the new tail and writes only
	// new rows. Single-row table (id is pinned to 1); a full Rebuild drops the
	// db file and starts the cursor empty.
	`
CREATE TABLE IF NOT EXISTS scheduler_ingest_cursor (
	id          INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
	byte_offset INTEGER NOT NULL,
	last_seq    INTEGER NOT NULL
);
`,
}
