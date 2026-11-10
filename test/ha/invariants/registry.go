package invariants

import (
	"fmt"

	"github.com/goobers/goobers/internal/cloud/lease"
)

const (
	journalRows  = `SELECT (SELECT COUNT(*) FROM journal.run_event) + (SELECT COUNT(*) FROM journal.instance_event)`
	claimRows    = `SELECT (SELECT COUNT(*) FROM control.claim_run_history) + (SELECT COUNT(*) FROM control.claim)`
	acceptRows   = `SELECT COUNT(*) FROM control.acceptance`
	receiptRows  = `SELECT COUNT(*) FROM control.source_receipt`
	instanceRows = `SELECT COUNT(*) FROM journal.instance_event`
)

// leaseRows counts controller lease epochs only: the migration runner records
// its own lease row, which must not make an empty controller history look covered.
var leaseRows = fmt.Sprintf(`SELECT COUNT(*) FROM control.lease_history WHERE name = '%s'`, lease.ControllerName)

var registry = []Invariant{
	{
		Name:        "no-duplicate-emit-key",
		CoverageSQL: journalRows,
		Description: "no (run, emit_key) or (stream, emit_key) appears twice in the journal, independent of the unique indexes",
		Reads: []string{
			"journal.run_event(instance_id, run_id, emit_key)",
			"journal.instance_event(instance_id, stream, emit_key)",
		},
		SQL: `SELECT 'run_event' AS source, instance_id, run_id AS scope, emit_key, COUNT(*) AS n
FROM journal.run_event WHERE emit_key IS NOT NULL
GROUP BY instance_id, run_id, emit_key HAVING COUNT(*) > 1
UNION ALL
SELECT 'instance_event', instance_id, stream, emit_key, COUNT(*)
FROM journal.instance_event WHERE emit_key IS NOT NULL
GROUP BY instance_id, stream, emit_key HAVING COUNT(*) > 1`,
	},
	{
		Name:        "journal-seq-contiguous",
		CoverageSQL: journalRows,
		Description: "run and instance sequences start at 1 with no gap or duplicate, and run_seq.last_seq matches the log",
		Reads: []string{
			"journal.run_event(instance_id, run_id, seq)",
			"journal.run_seq(instance_id, run_id, last_seq)",
			"journal.instance_event(instance_id, stream, seq)",
		},
		SQL: `SELECT 'run_event' AS source, instance_id, run_id AS scope, COUNT(*) AS n, MAX(seq) AS max_seq
FROM journal.run_event GROUP BY instance_id, run_id
HAVING COUNT(*) <> MAX(seq) OR MIN(seq) <> 1 OR COUNT(DISTINCT seq) <> COUNT(*)
UNION ALL
SELECT 'run_seq', s.instance_id, s.run_id, s.last_seq, COALESCE(MAX(e.seq), 0)
FROM journal.run_seq s
LEFT JOIN journal.run_event e ON e.instance_id = s.instance_id AND e.run_id = s.run_id
GROUP BY s.instance_id, s.run_id, s.last_seq HAVING s.last_seq <> COALESCE(MAX(e.seq), 0)
UNION ALL
SELECT 'instance_event', instance_id, stream, COUNT(*), MAX(seq)
FROM journal.instance_event GROUP BY instance_id, stream
HAVING COUNT(*) <> MAX(seq) OR MIN(seq) <> 1 OR COUNT(DISTINCT seq) <> COUNT(*)
UNION ALL
SELECT 'instance_seq', s.instance_id, s.stream, s.last_seq, COALESCE(MAX(e.seq), 0)
FROM journal.instance_seq s
LEFT JOIN journal.instance_event e ON e.instance_id = s.instance_id AND e.stream = s.stream
GROUP BY s.instance_id, s.stream, s.last_seq HAVING s.last_seq <> COALESCE(MAX(e.seq), 0)`,
	},
	{
		Name:        "no-claim-with-two-runs",
		CoverageSQL: claimRows,
		Description: "no claim key is held by two run IDs over overlapping retained run intervals and live claims",
		Reads: []string{
			"control.claim(instance_id, gaggle, provider, external_id, run_id, claimed_at)",
			"control.claim_run_history(instance_id, gaggle, provider, external_id, run_id, claimed_at, expires_at, released_at)",
		},
		SQL: `WITH held AS (
  SELECT h.instance_id, h.gaggle, h.provider, h.external_id, h.run_id, h.claimed_at,
         COALESCE(h.released_at, h.expires_at) AS released_at
  FROM control.claim_run_history h
  WHERE NOT EXISTS (
    SELECT 1 FROM control.claim c
    WHERE c.instance_id=h.instance_id AND c.gaggle=h.gaggle AND c.provider=h.provider
      AND c.external_id=h.external_id AND c.run_id=h.run_id
  )
  UNION ALL
  SELECT instance_id, gaggle, provider, external_id, run_id, claimed_at, NULL FROM control.claim
)
SELECT a.instance_id, a.gaggle, a.provider, a.external_id, a.run_id AS run_a, b.run_id AS run_b
FROM held a JOIN held b
  ON a.instance_id = b.instance_id AND a.gaggle = b.gaggle
 AND a.provider = b.provider AND a.external_id = b.external_id
 AND a.run_id < b.run_id
WHERE (b.released_at IS NULL OR a.claimed_at < b.released_at)
  AND (a.released_at IS NULL OR b.claimed_at < a.released_at)`,
	},
	{
		Name:        "one-acceptance-per-source-key",
		CoverageSQL: receiptRows,
		Description: "no acceptance is recorded under two source receipts, and every receipt names existing acceptances",
		Reads: []string{
			"control.source_receipt(instance_id, source_key, acceptance_ids)",
			"control.acceptance(instance_id, id)",
		},
		SQL: `WITH linked AS (
  SELECT r.instance_id, r.source_key, unnest(r.acceptance_ids) AS acceptance_id FROM control.source_receipt r
)
SELECT 'shared' AS problem, instance_id, acceptance_id, COUNT(DISTINCT source_key) AS n
FROM linked GROUP BY instance_id, acceptance_id HAVING COUNT(DISTINCT source_key) > 1
UNION ALL
SELECT 'missing', l.instance_id, l.acceptance_id, 1
FROM linked l LEFT JOIN control.acceptance a ON a.instance_id = l.instance_id AND a.id = l.acceptance_id
WHERE a.id IS NULL`,
	},
	{
		Name:        "no-duplicate-run-start",
		CoverageSQL: acceptRows,
		Description: "no run ID is started by two acceptances, independent of the unique index",
		Reads:       []string{"control.acceptance(instance_id, id, run_id)"},
		SQL: `SELECT instance_id, run_id, COUNT(*) AS n
FROM control.acceptance WHERE run_id <> ''
GROUP BY instance_id, run_id HAVING COUNT(*) > 1`,
	},
	{
		Name:        "no-lost-acceptance",
		CoverageSQL: acceptRows,
		Description: "a dispatched acceptance names the run it started",
		Reads:       []string{"control.acceptance(instance_id, id, state, run_id)"},
		SQL: `SELECT instance_id, id, state FROM control.acceptance
WHERE state = 'dispatched' AND run_id = ''`,
	},
	{
		Name:        "no-overlapping-lease-intervals",
		CoverageSQL: leaseRows,
		Description: "two epochs of one lease never hold it at the same time",
		Reads:       []string{"control.lease_history(instance_id, name, epoch, acquired_at, released_at)"},
		SQL: `SELECT a.instance_id, a.name, a.epoch AS epoch_a, b.epoch AS epoch_b
FROM control.lease_history a JOIN control.lease_history b
  ON a.instance_id = b.instance_id AND a.name = b.name AND a.epoch < b.epoch
WHERE a.released_at IS NULL OR a.released_at > b.acquired_at`,
	},
	{
		Name:        "no-stale-owner-emits",
		Description: "instance-log epochs never decrease in seq order and every epoch is a recorded controller lease epoch (clock-free)",
		Reads: []string{
			"journal.instance_event(instance_id, stream, seq, epoch)",
			"control.lease_history(instance_id, name, epoch)",
		},
		CoverageSQL: instanceRows,
		Args:        []any{lease.ControllerName},
		SQL: `SELECT 'regressed' AS problem, instance_id, stream, seq, epoch, prev_epoch FROM (
  SELECT instance_id, stream, seq, epoch,
         MAX(epoch) OVER (PARTITION BY instance_id, stream ORDER BY seq
                          ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS prev_epoch
  FROM journal.instance_event) ordered
WHERE epoch < prev_epoch
UNION ALL
SELECT 'unknown-epoch', e.instance_id, e.stream, e.seq, e.epoch, NULL::bigint
FROM journal.instance_event e
WHERE NOT EXISTS (
  SELECT 1 FROM control.lease_history h
  WHERE h.instance_id = e.instance_id AND h.name = $1 AND h.epoch = e.epoch)`,
	},
	{
		Name:        "no-notmine-side-effects",
		Description: "a start answered NotMine wrote no claim, slot or journal row",
		Pending:     "pending #7823",
	},
	{
		Name:        "no-stage-executed-by-control-plane",
		Description: "stages only ever execute in worker or stage pods",
		Pending:     "pending #7870",
	},
}
