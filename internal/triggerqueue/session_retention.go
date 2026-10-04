package triggerqueue

import (
	"context"
	"database/sql"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
)

// PruneSessions performs at most limit custody work units. One turn unit owns
// at most two message rows and its queue/request receipt. Only closed sessions
// with settled turns expire; running/uncertain custody cannot age out.
func (s *Store) PruneSessions(ctx context.Context, now time.Time, limit int) (int, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return 0, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	count, err := pruneSessionTombstones(ctx, tx, now, limit)
	if err != nil {
		return 0, err
	}
	added, err := tombstoneSessionTurns(ctx, tx, now, limit-count)
	if err != nil {
		return 0, err
	}
	count += added
	added, err = pruneClosedSessions(ctx, tx, now, limit-count)
	if err != nil {
		return 0, err
	}
	count += added
	return count, tx.Commit()
}

func pruneSessionTombstones(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	ids, err := childPruneIDs(ctx, tx, `SELECT id FROM interactive_turns WHERE tombstoned_ns<=? ORDER BY id LIMIT ?`, now.Add(-sessioning.Retention).UnixNano(), limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM interactive_requests WHERE acceptance_id=(SELECT acceptance_id FROM interactive_turns WHERE id=?)`, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM interactive_turns WHERE id=?`, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func tombstoneSessionTurns(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	cutoff := now.Add(-sessioning.Retention).UnixNano()
	ids, err := childPruneIDs(ctx, tx, `SELECT t.id FROM interactive_turns t JOIN interactive_sessions s ON s.id=t.session_id WHERE s.state='closed' AND s.active_turn='' AND s.closed_ns<=? AND t.state='settled' AND t.settled_ns<=? AND t.tombstoned_ns IS NULL AND NOT EXISTS(SELECT 1 FROM start_controls sc WHERE sc.acceptance_id=t.acceptance_id AND sc.cancel_ns IS NOT NULL AND sc.disposition='') ORDER BY t.id LIMIT ?`, cutoff, cutoff, limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE interactive_requests SET tombstoned_ns=? WHERE acceptance_id=(SELECT acceptance_id FROM interactive_turns WHERE id=?)`, now.UnixNano(), id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM interactive_messages WHERE turn_id=?`, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM triggers WHERE id=(SELECT acceptance_id FROM interactive_turns WHERE id=?)`, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE interactive_turns SET authority='{}',inputs='',tombstoned_ns=? WHERE id=?`, now.UnixNano(), id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func pruneClosedSessions(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	// Empty sessions keep request tombstones for the same second retention
	// window. Session IDs never become reusable, even after their rows expire.
	ids, err := childPruneIDs(ctx, tx, `SELECT id FROM interactive_sessions s WHERE state='closed' AND closed_ns<=? AND NOT EXISTS(SELECT 1 FROM interactive_turns t WHERE t.session_id=s.id) AND (EXISTS(SELECT 1 FROM interactive_requests r WHERE r.session_id=s.id AND r.tombstoned_ns IS NULL) OR NOT EXISTS(SELECT 1 FROM interactive_requests r WHERE r.session_id=s.id AND r.tombstoned_ns>?)) ORDER BY id LIMIT ?`, now.Add(-sessioning.Retention).UnixNano(), now.Add(-sessioning.Retention).UnixNano(), limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		var recent int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM interactive_requests WHERE session_id=? AND (tombstoned_ns IS NULL OR tombstoned_ns>?)`, id, now.Add(-sessioning.Retention).UnixNano()).Scan(&recent); err != nil {
			return 0, err
		}
		if recent > 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE interactive_requests SET tombstoned_ns=COALESCE(tombstoned_ns,?) WHERE session_id=?`, now.UnixNano(), id); err != nil {
				return 0, err
			}
			continue
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM interactive_requests WHERE session_id=?`, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM interactive_messages WHERE session_id=?`, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM interactive_sessions WHERE id=?`, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// UnsettledSessionTurns is a bounded host lifecycle inventory, not a user API.
func (s *Store) UnsettledSessionTurns(ctx context.Context, after string, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrTransition
	}
	rows, err := s.db.QueryContext(ctx, `SELECT acceptance_id FROM interactive_turns WHERE state IN ('dispatching','running') AND acceptance_id>? ORDER BY acceptance_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SessionRunRetained protects execution evidence used by an active session or
// its closed-session replay window. The session pruner removes this pin first.
func (s *Store) SessionRunRetained(ctx context.Context, runID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM interactive_turns WHERE acceptance_id=? AND tombstoned_ns IS NULL`, "trigger-"+runID).Scan(&count)
	return count > 0, err
}
