package triggerqueue

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// HumanRestartReplay is a bounded host-owned tombstone. Its opaque replay binds
// exact human identity and command; source pins survive in the original envelope.
type HumanRestartReplay struct {
	Record                                Record
	Gaggle, SourceRun, Epoch, Disposition string
	Replay                                []byte
	Retiring                              bool
}

// HumanRestartReplay reads compacted history without turning it into a plan.
func (s *Store) HumanRestartReplay(ctx context.Context, id string) (HumanRestartReplay, error) {
	return readHumanRestartReplay(ctx, s.db, id)
}
func readHumanRestartReplay(ctx context.Context, q sourceQuerier, id string) (HumanRestartReplay, error) {
	var r HumanRestartReplay
	var retiring sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT gaggle,source_run,epoch,replay,retiring_ns FROM human_restart_plans WHERE acceptance_id=?`, id).Scan(&r.Gaggle, &r.SourceRun, &r.Epoch, &r.Replay, &retiring)
	if err != nil {
		return r, err
	}
	r.Retiring = retiring.Valid
	if len(r.Replay) == 0 {
		return r, nil
	}
	c, err := readStartControl(ctx, q, r.Gaggle, id)
	if err != nil {
		return r, err
	}
	if !cancelledRestartProof(c, r.Epoch) || len(r.Replay) > 4096 || !json.Valid(r.Replay) {
		return r, ErrConflict
	}
	r.Record = c.Record
	r.Disposition = c.Disposition
	return r, nil
}
func cancelledRestartProof(c StartControl, epoch string) bool {
	return c.Record.State == Rejected && c.Record.RunID == "" && c.Scope.Source == "human-restart" && c.Scope.ReservedRunID == epoch && (c.Disposition == "cancelled" || c.Disposition == "expired") && !c.DisposedAt.IsZero()
}

// CompactHumanRestart atomically discards large execution context after its
// replay window, keeping the exact key until source retirement is qualified.
func (s *Store) CompactHumanRestart(ctx context.Context, id string, expectedPlan, replay []byte, now time.Time) error {
	if len(replay) == 0 || len(replay) > 4096 || !json.Valid(replay) || now.IsZero() {
		return ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := readHumanRestartReplay(ctx, tx, id)
	if err != nil {
		return err
	}
	if len(r.Replay) > 0 {
		if !bytes.Equal(r.Replay, replay) {
			return ErrConflict
		}
		return nil
	}
	c, err := readStartControl(ctx, tx, r.Gaggle, id)
	if err != nil {
		return err
	}
	if !cancelledRestartProof(c, r.Epoch) || now.Before(c.DisposedAt.Add(ReplayRetention)) {
		return ErrTransition
	}
	var saved []byte
	if err = tx.QueryRowContext(ctx, `SELECT plan FROM human_restart_plans WHERE acceptance_id=?`, id).Scan(&saved); err != nil {
		return err
	}
	if !bytes.Equal(saved, expectedPlan) {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE human_restart_plans SET plan=x'7b7d',replay=? WHERE acceptance_id=?`, replay, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE start_controls SET reserved_bytes=0 WHERE acceptance_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// HumanRestartMaintenance returns a bounded window for compaction and qualified
// source-retirement checks. Neither age nor a missing plan grants deletion.
func (s *Store) HumanRestartMaintenance(ctx context.Context, after string, limit int) ([]HumanRestartReplay, error) {
	if len(after) > 128 || limit < 1 || limit > 100 {
		return nil, ErrTransition
	}
	rows, err := s.db.QueryContext(ctx, `SELECT h.acceptance_id FROM human_restart_plans h JOIN start_controls c ON c.acceptance_id=h.acceptance_id WHERE h.acceptance_id>? AND c.disposition IN ('cancelled','expired') ORDER BY h.acceptance_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	result := make([]HumanRestartReplay, 0, len(ids))
	for _, id := range ids {
		r, err := readHumanRestartReplay(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		if len(r.Replay) == 0 {
			c, err := s.StartControl(ctx, r.Gaggle, id)
			if err != nil {
				return nil, err
			}
			r.Record = c.Record
			r.Disposition = c.Disposition
		}
		result = append(result, r)
	}
	return result, nil
}

// MarkHumanRestartSourceRetiring is called only by the existing source journal
// prune guard. Rollback of filesystem pruning leaves these keys intact.
func (s *Store) MarkHumanRestartSourceRetiring(ctx context.Context, gaggle, source string, now time.Time) error {
	if !validChildText(gaggle, 256, true) || !validChildText(source, 256, true) || now.IsZero() {
		return ErrTransition
	}
	_, err := s.db.ExecContext(ctx, `UPDATE human_restart_plans SET retiring_ns=COALESCE(retiring_ns,?) WHERE gaggle=? AND source_run=? AND EXISTS(SELECT 1 FROM triggers t JOIN start_controls c ON c.acceptance_id=t.id WHERE t.id=human_restart_plans.acceptance_id AND t.state='rejected' AND t.run_id='' AND c.disposition IN ('cancelled','expired'))`, now.UnixNano(), gaggle, source)
	return err
}

// ForgetRetiredHumanRestart requires the host to have proved this marked source
// absent from both live and staged journals under run-root maintenance locks.
func (s *Store) ForgetRetiredHumanRestart(ctx context.Context, id, gaggle, source string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := readHumanRestartReplay(ctx, tx, id)
	if err != nil {
		return err
	}
	if r.Gaggle != gaggle || r.SourceRun != source || !r.Retiring || len(r.Replay) == 0 {
		return ErrTransition
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM triggers WHERE id=? AND state='rejected' AND run_id=''`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// HumanRestartSourceScope qualifies the source-retirement guard without relying
// on a current workflow name. Multiple gaggle owners are ambiguous and refused.
func (s *Store) HumanRestartSourceScope(ctx context.Context, source string) (string, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.gaggle,MIN(h.retiring_ns IS NOT NULL) FROM human_restart_plans h JOIN start_controls c ON c.acceptance_id=h.acceptance_id WHERE h.source_run=? AND c.disposition IN ('cancelled','expired') GROUP BY h.gaggle LIMIT 2`, source)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rows.Close() }()
	var gaggle string
	var allMarked bool
	count := 0
	for rows.Next() {
		count++
		if err = rows.Scan(&gaggle, &allMarked); err != nil {
			return "", false, err
		}
	}
	if count > 1 {
		return "", false, ErrConflict
	}
	return gaggle, allMarked, rows.Err()
}
