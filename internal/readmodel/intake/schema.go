package intake

// migrations is the ordered, append-only list of forward migrations applied on
// Open. Never edit a released migration; append a new one.
var migrations = []string{
	`
CREATE TABLE IF NOT EXISTS run_intake (
	run_id      TEXT PRIMARY KEY,
	source_seq  INTEGER NOT NULL,
	-- removing is retention's intent, recorded BEFORE the journal is unlinked.
	-- It is a separate column rather than a sentinel source_seq because a
	-- removal must never be mistakable for progress: the ordinary ack carries
	-- removing = 0 in its WHERE clause, so it cannot consume a removal marker.
	removing    INTEGER NOT NULL DEFAULT 0,
	observed_at TEXT NOT NULL
);

-- The projector drains oldest-first so a burst cannot starve an early marker.
CREATE INDEX IF NOT EXISTS idx_run_intake_observed ON run_intake(observed_at, run_id);
`,
}
