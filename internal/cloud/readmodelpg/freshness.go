package readmodelpg

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/readmodel"
)

// gapExpectedWithin is the honest deadline for a projection gap. The PG projector
// retries every unprojected run on every catch-up pass (about a second apart), so
// a gap still open after this is a defect rather than work in progress.
const gapExpectedWithin = 5 * time.Minute

var (
	_ readmodel.FreshnessReporter = (*Store)(nil)
	_ readmodel.FreshnessReporter = (*ActiveReader)(nil)
)

// ReadState builds the freshness envelope for the active model. Unlike the
// SQLite model there is no repair sweep: the projector's catch-up pass is what
// bounds the un-watermarked case, so no sweep fields are reported, and neither
// is the sweep's degraded condition (reporting it would be permanently false).
// The pending count and failure counts are the caller's, as for SQLite.
func (s *Store) ReadState(ctx context.Context, input readmodel.ReadStateInput) (readmodel.ReadState, error) {
	state, err := s.State(ctx)
	if err != nil {
		return readmodel.ReadState{}, err
	}
	latest, err := s.LatestChangeSeq(ctx)
	if err != nil {
		return readmodel.ReadState{}, err
	}
	now := time.Now()
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
	if input.IntakeWriteFailures > 0 {
		out.Degraded = append(out.Degraded, readmodel.DegradedIntakeWriteFailure)
	}
	if input.PendingIntake > 0 {
		out.Degraded = append(out.Degraded, readmodel.DegradedProjectionLag)
	}
	if input.ProjectFailures > 0 {
		out.Degraded = append(out.Degraded, readmodel.DegradedProjectFailure)
	}
	if input.IntakeCountUnavailable {
		out.Degraded = append(out.Degraded, readmodel.DegradedIntakeCountUnavailable)
	}
	if gap := input.ProjectFailures + input.IntakeWriteFailures; gap > 0 {
		out.Completeness = readmodel.CompletenessPartial
		out.Missing = append(out.Missing, readmodel.MissingPartition{
			Name: readmodel.MissingProjectedRuns,
			Reason: fmt.Sprintf("%d run(s) are not projected: %d failed to apply, %d have no recorded watermark; the projector retries them every pass",
				gap, input.ProjectFailures, input.IntakeWriteFailures),
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
