package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"reflect"
	"time"

	"github.com/goobers/goobers/providers"
)

// AttachWorkbenchProposalPlan retains exact validated source bytes once. It
// cannot change intent after a provider phase has been claimed.
func (s *Store) AttachWorkbenchProposalPlan(ctx context.Context, scope WorkbenchCommandScope, id, digest string, plan WorkbenchProposalPlan) (WorkbenchProposal, error) {
	header, before, after, planDigest, err := canonicalProposalPlan(plan)
	if err != nil {
		return WorkbenchProposal{}, err
	}
	tx, record, err := s.proposalTransaction(ctx, scope, id, digest)
	if err != nil {
		return record, err
	}
	defer func() { _ = tx.Rollback() }()
	if record.Plan != nil {
		if record.PlanDigest != planDigest {
			return record, ErrConflict
		}
		return record, nil
	}
	if record.State != "accepted" || validateWorkbenchProposalPlan(record, plan) != nil {
		return record, ErrTransition
	}
	size := len(header) + len(before) + len(after)
	if err = proposalConsumeReservation(ctx, tx, id, size+16*1024); err != nil {
		return record, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE workbench_proposals SET plan=?,plan_digest=?,before_source=?,after_source=?,state='prepared' WHERE id=? AND state='accepted'`, header, planDigest, before, after, id)
	if err != nil {
		return record, err
	}
	record, err = workbenchProposalTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	return record, tx.Commit()
}

// ClaimWorkbenchProposalPhase requires an explicit authorized command for each
// next phase. A pending/unknown attempt cannot be reclaimed after restart.
func (s *Store) ClaimWorkbenchProposalPhase(ctx context.Context, scope WorkbenchCommandScope, id, digest, phase string, now time.Time) (WorkbenchProposal, bool, error) {
	if now.IsZero() {
		return WorkbenchProposal{}, false, ErrTransition
	}
	tx, record, err := s.proposalTransaction(ctx, scope, id, digest)
	if err != nil {
		return record, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if record.State != "prepared" {
		return record, false, nil
	}
	names := proposalPhaseNames(record.Plan.Native.Repository.Provider)
	if len(record.Phases) >= len(names) || phase != names[len(record.Phases)] || now.Before(record.AcceptedAt) {
		return record, false, ErrTransition
	}
	if len(record.Phases) > 0 {
		previous := record.Phases[len(record.Phases)-1]
		if previous.FinishedAt != nil && now.Before(*previous.FinishedAt) {
			return record, false, ErrTransition
		}
		if observed, found := lastProposalObservation(record, len(record.Phases)-1); found && now.Before(observed.At) {
			return record, false, ErrTransition
		}
	}
	record.Phases = append(record.Phases, WorkbenchProposalPhase{Name: phase, ClaimedAt: now.UTC()})
	record.State = "attempting"
	if err = writeProposalHistory(ctx, tx, record); err != nil {
		return record, false, err
	}
	record, err = workbenchProposalTx(ctx, tx, scope, id)
	if err != nil {
		return record, false, err
	}
	return record, true, tx.Commit()
}

// CompleteWorkbenchProposalPhase records the originating call's one result. It
// cannot rewrite an unknown outcome into acknowledged after later observation.
func (s *Store) CompleteWorkbenchProposalPhase(ctx context.Context, scope WorkbenchCommandScope, id, digest string, index int, result providers.RepositoryProposalPhaseResult, now time.Time) (WorkbenchProposal, error) {
	tx, record, err := s.proposalTransaction(ctx, scope, id, digest)
	if err != nil {
		return record, err
	}
	defer func() { _ = tx.Rollback() }()
	if index < 0 || index >= len(record.Phases) || now.IsZero() || now.Before(record.Phases[index].ClaimedAt) || validateProposalResult(record, index, result) != nil {
		return record, ErrTransition
	}
	phase := &record.Phases[index]
	if phase.Result != nil {
		if !reflect.DeepEqual(*phase.Result, result) {
			return record, ErrConflict
		}
		return record, nil
	}
	stamp := now.UTC()
	phase.Result, phase.FinishedAt, phase.Outcome = &result, &stamp, proposalOutcome(result)
	setProposalHistoryState(&record, now)
	if err = writeProposalHistory(ctx, tx, record); err != nil {
		return record, err
	}
	record, err = workbenchProposalTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	return record, tx.Commit()
}

// ObserveWorkbenchProposal appends bounded read evidence without changing any
// phase receipt. Satisfying a phase permits only a subsequent explicit Claim.
func (s *Store) ObserveWorkbenchProposal(ctx context.Context, scope WorkbenchCommandScope, id, digest string, index int, result providers.RepositoryProposalObservation, now time.Time) (WorkbenchProposal, error) {
	tx, record, err := s.proposalTransaction(ctx, scope, id, digest)
	if err != nil {
		return record, err
	}
	defer func() { _ = tx.Rollback() }()
	if index != len(record.Phases)-1 || index < 0 || now.IsZero() || record.CompletedAt != nil || record.State == "blocked" {
		return record, ErrTransition
	}
	phase := record.Phases[index]
	if phase.Result != nil && !phase.Result.MutationAttempted {
		return record, ErrTransition
	}
	observation := WorkbenchProposalObservation{Phase: index, At: now.UTC(), Result: result}
	if validateProposalObservation(record, observation) != nil {
		return record, ErrTransition
	}
	if previous, found := lastProposalObservation(record, index); found && now.Before(previous.At) {
		return record, ErrTransition
	}
	record.Observations = append(record.Observations, observation)
	trimProposalObservations(&record)
	setProposalHistoryState(&record, now)
	if err = writeProposalHistory(ctx, tx, record); err != nil {
		return record, err
	}
	record, err = workbenchProposalTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	return record, tx.Commit()
}

// StopWorkbenchProposal stops before an unclaimed next phase. Previously
// visible branch/PR effects remain blocked and pinned for human inspection.
func (s *Store) StopWorkbenchProposal(ctx context.Context, scope WorkbenchCommandScope, id, digest string, now time.Time) (WorkbenchProposal, error) {
	tx, record, err := s.proposalTransaction(ctx, scope, id, digest)
	if err != nil {
		return record, err
	}
	defer func() { _ = tx.Rollback() }()
	if now.IsZero() || now.Before(record.AcceptedAt) || (record.State != "accepted" && record.State != "prepared") {
		return record, ErrTransition
	}
	record.State = "blocked"
	if !proposalHasVisibleEffect(record) {
		record.State = "not-applied"
		stamp := now.UTC()
		record.CompletedAt = &stamp
	}
	if err = writeProposalHistory(ctx, tx, record); err != nil {
		return record, err
	}
	record, err = workbenchProposalTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	return record, tx.Commit()
}

func (s *Store) proposalTransaction(ctx context.Context, scope WorkbenchCommandScope, id, digest string) (*sql.Tx, WorkbenchProposal, error) {
	if !validWorkbenchScope(scope) || !validWorkbenchID(id) || !validWorkbenchDigest(digest) {
		return nil, WorkbenchProposal{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, WorkbenchProposal{}, err
	}
	record, err := workbenchProposalTx(ctx, tx, scope, id)
	if err == nil && record.RequestDigest != digest {
		err = ErrConflict
	}
	if err == nil && record.TombstonedAt != nil {
		err = ErrWorkbenchCommandExpired
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, record, err
	}
	return tx, record, nil
}

func proposalConsumeReservation(ctx context.Context, tx *sql.Tx, id string, size int) error {
	var reserved int
	if err := tx.QueryRowContext(ctx, `SELECT reserved_bytes FROM workbench_proposals WHERE id=?`, id).Scan(&reserved); err != nil {
		return err
	}
	consume := min(size, reserved)
	if err := childByteCapacity(ctx, tx, size-consume); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE workbench_proposals SET reserved_bytes=reserved_bytes-? WHERE id=?`, consume, id)
	return err
}

func writeProposalHistory(ctx context.Context, tx *sql.Tx, record WorkbenchProposal) error {
	if err := validateWorkbenchProposalState(record); err != nil {
		return err
	}
	raw, err := json.Marshal(workbenchProposalHistory{record.Phases, record.Observations, record.OmittedObservations})
	if err != nil || len(raw) > MaxWorkbenchProposalHistoryBytes {
		return ErrTransition
	}
	var old int
	if err = tx.QueryRowContext(ctx, `SELECT length(history) FROM workbench_proposals WHERE id=?`, record.ID).Scan(&old); err != nil {
		return err
	}
	growth := max(0, len(raw)-old)
	if old == 0 {
		growth += 8192
	}
	if err = proposalConsumeReservation(ctx, tx, record.ID, growth); err != nil {
		return err
	}
	var completed interface{}
	if record.CompletedAt != nil {
		completed = record.CompletedAt.UnixNano()
	}
	_, err = tx.ExecContext(ctx, `UPDATE workbench_proposals SET history=?,history_digest=?,state=?,completed_ns=?,reserved_bytes=CASE WHEN ? IS NOT NULL THEN 0 ELSE reserved_bytes END WHERE id=?`, raw, childDigest(raw), record.State, completed, completed, record.ID)
	return err
}

func setProposalHistoryState(record *WorkbenchProposal, now time.Time) {
	record.State = proposalHistoryState(*record)
	if record.State == "confirmed" || record.State == "observed" || record.State == "not-applied" {
		if record.CompletedAt == nil {
			stamp := now.UTC()
			record.CompletedAt = &stamp
		}
	} else {
		record.CompletedAt = nil
	}
}

func trimProposalObservations(record *WorkbenchProposal) {
	if len(record.Observations) <= MaxWorkbenchProposalObservations {
		return
	}
	// Keep the final evidence for earlier phases: a later PR observation must
	// never evict the proof that allowed a previously uncertain commit to advance.
	for i, o := range record.Observations {
		last := true
		for _, later := range record.Observations[i+1:] {
			if later.Phase == o.Phase {
				last = false
				break
			}
		}
		if o.Phase < len(record.Phases)-1 && last {
			continue
		}
		record.Observations = append(record.Observations[:i], record.Observations[i+1:]...)
		if record.OmittedObservations < math.MaxInt64 {
			record.OmittedObservations++
		}
		return
	}
}
