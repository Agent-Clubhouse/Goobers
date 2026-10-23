package invariants

var registry = []Invariant{
	{
		Name:        "no-duplicate-emit-key",
		Description: "no (run, emit_key) appears twice in the journal",
		Reads:       []string{"journal.event(instance_id, run_id, emit_key)"},
		SQL: `SELECT instance_id, run_id, emit_key, COUNT(*) AS n
FROM journal.event WHERE emit_key IS NOT NULL
GROUP BY instance_id, run_id, emit_key HAVING COUNT(*) > 1`,
	},
	{
		Name:        "journal-seq-contiguous",
		Description: "(run, branch, seq) starts at 1 and has no gap or duplicate",
		Reads:       []string{"journal.event(instance_id, run_id, branch, seq)"},
		SQL: `SELECT instance_id, run_id, branch, COUNT(*) AS n, MAX(seq) AS max_seq
FROM journal.event GROUP BY instance_id, run_id, branch
HAVING COUNT(*) <> MAX(seq) OR MIN(seq) <> 1`,
	},
	{
		Name:        "no-claim-with-two-runs",
		Description: "no claim key is held by two run IDs over overlapping intervals",
		Reads:       []string{"control.claim_history(instance_id, gaggle, provider, external_id, run_id, held_from, held_until)"},
		SQL: `SELECT a.instance_id, a.gaggle, a.provider, a.external_id, a.run_id AS run_a, b.run_id AS run_b
FROM control.claim_history a JOIN control.claim_history b
  ON a.instance_id = b.instance_id AND a.gaggle = b.gaggle
 AND a.provider = b.provider AND a.external_id = b.external_id
 AND a.run_id < b.run_id
WHERE (b.held_until IS NULL OR a.held_from < b.held_until)
  AND (a.held_until IS NULL OR b.held_from < a.held_until)`,
	},
	{
		Name:        "one-acceptance-per-source-key",
		Description: "each trigger source key has exactly one acceptance",
		Reads:       []string{"control.acceptance(instance_id, source_kind, source_key)"},
		SQL: `SELECT instance_id, source_kind, source_key, COUNT(*) AS n
FROM control.acceptance GROUP BY instance_id, source_kind, source_key HAVING COUNT(*) > 1`,
	},
	{
		Name:        "no-overlapping-lease-intervals",
		Description: "two epochs of one lease never hold it at the same time",
		Reads:       []string{"control.lease_history(instance_id, name, epoch, holder, acquired_at, released_at)"},
		SQL: `SELECT a.instance_id, a.name, a.epoch AS epoch_a, b.epoch AS epoch_b
FROM control.lease_history a JOIN control.lease_history b
  ON a.instance_id = b.instance_id AND a.name = b.name AND a.epoch < b.epoch
WHERE a.released_at IS NULL OR a.released_at > b.acquired_at`,
	},
	{
		Name:        "no-notmine-side-effects",
		Description: "a start answered NotMine wrote no claim, slot or journal row",
		Reads:       []string{"control.start_audit(instance_id, acceptance_id, epoch, outcome, claim_written, slot_written, journal_written)"},
		SQL: `SELECT instance_id, acceptance_id, epoch FROM control.start_audit
WHERE outcome = 'not_mine' AND (claim_written <> 0 OR slot_written <> 0 OR journal_written <> 0)`,
	},
	{
		Name:        "no-stale-owner-emits",
		Description: "no journal row was committed by an epoch after a later epoch had acquired the lease",
		Reads: []string{
			"journal.event(instance_id, run_id, branch, seq, owner_epoch, committed_at)",
			"control.lease_history(instance_id, name, epoch, acquired_at)",
		},
		SQL: `SELECT e.instance_id, e.run_id, e.branch, e.seq, e.owner_epoch
FROM journal.event e
WHERE e.owner_epoch IS NOT NULL AND EXISTS (
  SELECT 1 FROM control.lease_history h
  WHERE h.instance_id = e.instance_id AND h.name = 'control'
    AND h.epoch > e.owner_epoch AND h.acquired_at <= e.committed_at)`,
	},
	{
		Name:        "no-stage-executed-by-control-plane",
		Description: "stages only ever execute in worker or stage pods",
		Reads:       []string{"control.stage_exec_audit(instance_id, run_id, stage, pod_role)"},
		SQL: `SELECT instance_id, run_id, stage, pod_role FROM control.stage_exec_audit
WHERE pod_role IN ('control', 'api')`,
	},
}
