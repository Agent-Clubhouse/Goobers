package readmodelpg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/readmodel"
)

// gapExpectedWithin is the honest deadline for a projection gap. The PG projector
// retries every unprojected run on every catch-up pass (about a second apart), so
// a gap still open after this is a defect rather than work in progress.
const gapExpectedWithin = 5 * time.Minute

// ProjectorStaleAfter is how old the last recorded projector pass may be before
// the model reports projector_stale. The projector records a pass at least every
// PassRecordEvery, so several missed records mean it is gone, not idle.
const (
	ProjectorStaleAfter = 2 * time.Minute
	PassRecordEvery     = 15 * time.Second
)

// ProjectorPass is the projector's last recorded completed pass.
type ProjectorPass struct {
	// Recorded is false until the projector has recorded a pass.
	Recorded bool
	// Age is measured on the database clock, so reader and projector clock skew
	// cannot make a dead projector look alive.
	Age time.Duration
	// OpenFailures is the count of runs the projector could not project.
	OpenFailures int
}

// RecordPass stamps a completed pass on the active model under the projector's
// fence. A stale lease or a newer model owner is refused like any other write.
func (s *Store) RecordPass(ctx context.Context, openFailures int) error {
	return s.fence.WithFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		if err := s.checkActive(ctx, tx, epoch); err != nil {
			return err
		}
		query := fmt.Sprintf(`
			UPDATE %s SET last_pass_at = clock_timestamp(), open_failures = $1, owner_epoch = $2
			WHERE singleton AND owner_epoch <= $2`, s.table("meta"))
		tag, err := tx.Exec(ctx, query, openFailures, epoch)
		if err != nil {
			return fmt.Errorf("readmodelpg: record projector pass: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}

// ProjectorPass reads the recorded pass.
func (s *Store) ProjectorPass(ctx context.Context) (ProjectorPass, error) {
	var age *float64
	var pass ProjectorPass
	query := fmt.Sprintf(`
		SELECT extract(epoch FROM clock_timestamp() - last_pass_at), open_failures
		FROM %s WHERE singleton`, s.table("meta"))
	if err := s.pool.QueryRow(ctx, query).Scan(&age, &pass.OpenFailures); err != nil {
		return ProjectorPass{}, fmt.Errorf("readmodelpg: projector pass: %w", err)
	}
	if age != nil {
		pass.Recorded = true
		pass.Age = time.Duration(max(*age, 0) * float64(time.Second))
	}
	return pass, nil
}

var (
	_ readmodel.FreshnessReporter = (*Store)(nil)
	_ readmodel.FreshnessReporter = (*ActiveReader)(nil)
)

// ReadState builds the freshness envelope for the active model. Unlike the
// SQLite model there is no repair sweep: the projector's catch-up pass is what
// bounds the un-watermarked case, so no sweep fields are reported, and neither
// is the sweep's degraded condition (reporting it would be permanently false).
// The pending count and intake failures are the caller's, as for SQLite; the
// projector's liveness is not: it is read from the pass the projector persisted
// in the model, because only that is visible to the API role, and a dead
// projector cannot report itself dead. Open failures are the larger of the
// caller's and the persisted count.
func (s *Store) ReadState(ctx context.Context, input readmodel.ReadStateInput) (readmodel.ReadState, error) {
	state, err := s.State(ctx)
	if err != nil {
		return readmodel.ReadState{}, err
	}
	latest, err := s.LatestChangeSeq(ctx)
	if err != nil {
		return readmodel.ReadState{}, err
	}
	pass, err := s.ProjectorPass(ctx)
	if err != nil {
		return readmodel.ReadState{}, err
	}
	now := time.Now()
	projectFailures := max(input.ProjectFailures, pass.OpenFailures)
	out := readmodel.ReadState{
		Epoch:               state.Epoch,
		AppliedSeq:          latest,
		ObservedAt:          now,
		PendingIntake:       input.PendingIntake,
		IntakeWriteFailures: input.IntakeWriteFailures,
		MinChangeSeq:        state.MinChangeSeq,
		Completeness:        readmodel.CompletenessComplete,
		Degraded:            []string{},
	}
	if !input.OldestPendingAt.IsZero() {
		out.OldestPendingSourceAgeSeconds = now.Sub(input.OldestPendingAt).Seconds()
	}
	out.LagSeconds = max(out.OldestPendingSourceAgeSeconds, input.ProjectionLagSeconds)
	switch {
	case !pass.Recorded:
		out.Degraded = append(out.Degraded, readmodel.DegradedNoProjectorPass)
	case pass.Age > ProjectorStaleAfter:
		out.Degraded = append(out.Degraded, readmodel.DegradedProjectorStale)
		out.LagSeconds = max(out.LagSeconds, pass.Age.Seconds())
	}
	if input.IntakeWriteFailures > 0 {
		out.Degraded = append(out.Degraded, readmodel.DegradedIntakeWriteFailure)
	}
	if input.PendingIntake > 0 {
		out.Degraded = append(out.Degraded, readmodel.DegradedProjectionLag)
	}
	if projectFailures > 0 {
		out.Degraded = append(out.Degraded, readmodel.DegradedProjectFailure)
	}
	if input.IntakeCountUnavailable {
		out.Degraded = append(out.Degraded, readmodel.DegradedIntakeCountUnavailable)
	}
	if gap := projectFailures + input.IntakeWriteFailures; gap > 0 {
		out.Completeness = readmodel.CompletenessPartial
		out.Missing = append(out.Missing, readmodel.MissingPartition{
			Name: readmodel.MissingProjectedRuns,
			Reason: fmt.Sprintf("%d run(s) are not projected: %d failed to apply, %d have no recorded watermark; the projector retries them every pass",
				gap, projectFailures, input.IntakeWriteFailures),
			ExpectedBy: now.Add(gapExpectedWithin),
		})
	}
	return out, nil
}

// SourceApplied reports the projected journal position of a run; absent means
// the projection has not seen it yet, which a caller treats as not yet applied.
func (s *Store) SourceApplied(ctx context.Context, runID string) (readmodel.SourcePosition, bool, error) {
	row, found, err := s.GetRun(ctx, runID)
	if err != nil || !found {
		return readmodel.SourcePosition{}, false, err
	}
	return readmodel.SourcePosition{RunID: runID, JournalSeq: row.LastSeq}, true, nil
}

// SatisfiesSourceApplied reports whether the projection has caught up to a
// required source position.
func (s *Store) SatisfiesSourceApplied(ctx context.Context, required readmodel.SourcePosition) (bool, error) {
	applied, found, err := s.SourceApplied(ctx, required.RunID)
	if err != nil || !found {
		return false, err
	}
	return applied.JournalSeq >= required.JournalSeq, nil
}

// ReadState implements readmodel.FreshnessReporter.
func (r *ActiveReader) ReadState(ctx context.Context, input readmodel.ReadStateInput) (readmodel.ReadState, error) {
	return withStore(ctx, r, func(s *Store) (readmodel.ReadState, error) { return s.ReadState(ctx, input) })
}

// SourceApplied implements readmodel.FreshnessReporter.
func (r *ActiveReader) SourceApplied(ctx context.Context, runID string) (readmodel.SourcePosition, bool, error) {
	store, err := r.store(ctx)
	if err != nil {
		return readmodel.SourcePosition{}, false, err
	}
	return store.SourceApplied(ctx, runID)
}

// SatisfiesSourceApplied implements readmodel.FreshnessReporter.
func (r *ActiveReader) SatisfiesSourceApplied(ctx context.Context, required readmodel.SourcePosition) (bool, error) {
	return withStore(ctx, r, func(s *Store) (bool, error) { return s.SatisfiesSourceApplied(ctx, required) })
}
