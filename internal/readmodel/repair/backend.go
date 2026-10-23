package repair

import (
	"context"
	"errors"
	"os"

	"github.com/goobers/goobers/internal/journal"
)

// stepBackend walks the backend's logical namespace with one durable cursor.
// Filesystem roots and unpublished-directory memos have no meaning here.
func (s *Sweeper) stepBackend(ctx context.Context) error {
	cursor, err := s.store.SweepCursor(ctx)
	if err != nil {
		return err
	}
	reverseLimit := s.options.BatchSize / 2
	if s.options.BatchSize == 1 && !cursor.ForwardNext {
		reverseLimit = 1
	}
	examined := 0
	if reverseLimit > 0 {
		examined, err = s.sweepReverse(ctx, &cursor, reverseLimit, nil)
		if err != nil {
			return err
		}
	}
	if s.options.BatchSize == 1 {
		cursor.ForwardNext = !cursor.ForwardNext
	}
	limit := s.options.BatchSize - examined
	if limit > 0 {
		if cursor.CycleStartedAt.IsZero() {
			cursor.CycleStartedAt = s.options.Now().UTC()
		}
		refs, err := s.options.Journal.ListRuns(ctx, journal.RunFilter{After: cursor.AfterName, Limit: limit})
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return err
			}
			s.stats.EntriesExamined++
			cursor.EntriesThisCycle++
			if err := s.reconcilePublished(ctx, ref.ID, ref.ID); err != nil {
				s.recordRunFailure(ref.ID, "repair reconcile failed", err)
			}
			cursor.AfterName = ref.ID
		}
		if len(refs) < limit {
			cursor.AfterName = ""
			cursor.EntriesThisCycle = 0
			cursor.LastCycleCompletedAt = s.options.Now().UTC()
			cursor.CycleStartedAt = cursor.LastCycleCompletedAt
			s.stats.CyclesCompleted++
		}
	}
	return s.writer.SaveSweepCursor(ctx, cursor)
}

func (s *Sweeper) runExists(ctx context.Context, runID string, roots []string) (bool, error) {
	if s.options.Journal == nil {
		_, found := s.locate(runID, roots)
		return found, nil
	}
	_, err := s.options.Journal.OpenRun(ctx, journal.RunRef{ID: runID, ReadOnly: true})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	// An unavailable or corrupt source is not evidence of deletion.
	return err == nil, err
}
