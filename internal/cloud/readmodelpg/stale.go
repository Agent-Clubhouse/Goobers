package readmodelpg

import (
	"context"
	"fmt"
)

// StaleRun is a journaled run whose projection is missing or behind its journal.
type StaleRun struct {
	RunID string
	// JournalSeq is the journal's last committed sequence for the run.
	JournalSeq int64
}

// StaleRuns lists runs of one instance, in run-ID order after the given cursor,
// whose journal has advanced past the active model's source watermark. It is the
// restart-safe catch-up source for the projector: nothing depends on an
// in-process notification having been delivered. It reads journal.run_seq, which
// only the controller role can do (writepaths.yaml).
func (s *Store) StaleRuns(ctx context.Context, instanceID, after string, limit int) ([]StaleRun, error) {
	if instanceID == "" || limit < 1 {
		return nil, fmt.Errorf("readmodelpg: stale runs need an instance and a positive limit")
	}
	query := fmt.Sprintf(`
		SELECT q.run_id, q.last_seq
		FROM journal.run_seq q
		LEFT JOIN %s w ON w.run_id = q.run_id
		WHERE q.instance_id = $1 AND q.run_id > $2 AND q.last_seq > 0
		  AND (w.run_id IS NULL OR w.source_seq < q.last_seq)
		ORDER BY q.run_id
		LIMIT $3`, s.table("source_watermark"))
	rows, err := s.pool.Query(ctx, query, instanceID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("readmodelpg: list stale runs: %w", err)
	}
	defer rows.Close()
	var stale []StaleRun
	for rows.Next() {
		var run StaleRun
		if err := rows.Scan(&run.RunID, &run.JournalSeq); err != nil {
			return nil, fmt.Errorf("readmodelpg: scan stale run: %w", err)
		}
		stale = append(stale, run)
	}
	return stale, rows.Err()
}
