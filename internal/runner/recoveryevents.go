package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

const recoveryObservationBatchSize = 128 // journal.AppendBatchIfAbsent's generic transaction ceiling

// RecoveryInventoryError marks a post-cleanup observation failure that came
// from reading the shared recovery inventory itself (its configuration or its
// directory), as opposed to anything specific to the observing run: its own
// records, the observation-count and ownership checks, or the append into its
// journal. The two are journaled under different codes (#5404) because only
// the inventory failure can be shown cleared by ANOTHER run's later
// successful observation; a run-specific failure is cleared only by a later
// successful observation of the same run.
type RecoveryInventoryError struct{ Err error }

func (e *RecoveryInventoryError) Error() string { return e.Err.Error() }
func (e *RecoveryInventoryError) Unwrap() error { return e.Err }

// IsRecoveryInventoryError reports whether err failed reading the shared
// recovery inventory rather than anything run-specific.
func IsRecoveryInventoryError(err error) bool {
	var inventoryErr *RecoveryInventoryError
	return errors.As(err, &inventoryErr)
}

// recoveryObservationError distinguishes a failure to observe already-retained
// evidence after cleanup from a failure to perform the cleanup itself.
type recoveryObservationError struct{ err error }

func (e *recoveryObservationError) Error() string {
	return "observe retained recovery state: " + e.err.Error()
}
func (e *recoveryObservationError) Unwrap() error { return e.err }

func workspaceCleanupErrorDetail(err error) *journal.ErrorDetail {
	code := "worktree_remove_failed"
	var observationErr *recoveryObservationError
	if errors.As(err, &observationErr) {
		code = journal.ErrorCodeRecoveryObservationRunFailed
		if IsRecoveryInventoryError(observationErr) {
			code = journal.ErrorCodeRecoveryObservationFailed
		}
	}
	return journal.ErrorDetailFor(code, err)
}

type recoveryEventJournal interface {
	AppendBatchIfAbsent(context.Context, []journal.Event, func(journal.Event) string) (int, error)
}

func (r *Runner) recordRecoveryAfterCleanup(ctx context.Context, log recoveryEventJournal, runID string, cleanupErr error) error {
	if cleanupErr != nil {
		return cleanupErr
	}
	// The time the observation STARTED is what a success proves: the
	// inventory was readable from then on. Stamping the report any later — at
	// the end of the journal append, or when a recorder gets the instance-log
	// lock — would let a success that read the inventory before a concurrent
	// failure retract that newer failure (#5404).
	startedAt := time.Now().UTC()
	err := r.recordRecoveryEvents(context.WithoutCancel(ctx), log, runID)
	// Report successes as well as failures: a recovery observation blocker is
	// retracted only by evidence that a later observation succeeded.
	if r.cfg.RecoveryEvents != nil && r.cfg.RecoveryObserved != nil {
		r.cfg.RecoveryObserved(runID, startedAt, err)
	}
	if err != nil {
		return &recoveryObservationError{err: err}
	}
	return nil
}

func (r *Runner) recordRecoveryEvents(ctx context.Context, log recoveryEventJournal, runID string) error {
	if r.cfg.RecoveryEvents == nil {
		return nil
	}
	events, limit, err := r.cfg.RecoveryEvents(ctx, runID)
	if err != nil {
		return err
	}
	if limit <= 0 {
		return fmt.Errorf("invalid recovery observation limit")
	}
	if len(events) > limit {
		return fmt.Errorf("recovery observations exceed inventory bound")
	}
	for _, event := range events {
		if event.RunID != runID || event.Type != journal.EventRunnerAnnotation || event.Runner["operation"] != "recovery-retained" {
			return fmt.Errorf("recovery observation has mismatched ownership or type")
		}
		for _, field := range []string{"recoveryRef", "recoveryRepositoryKey", "recoveryRetainUntil"} {
			if value, ok := event.Runner[field].(string); !ok || value == "" {
				return fmt.Errorf("recovery observation has invalid identity field %s", field)
			}
		}
	}
	for len(events) > 0 {
		size := min(len(events), recoveryObservationBatchSize)
		if _, err := log.AppendBatchIfAbsent(ctx, events[:size], recoveryObservationKey); err != nil {
			return err
		}
		events = events[size:]
	}
	return nil
}

func recoveryObservationKey(event journal.Event) string {
	// The live journal stamps this field on stage-originated emissions. Such
	// an annotation is not the host's acknowledgement of an archived snapshot
	// and must never suppress the host writer's authoritative observation.
	if _, emitted := event.Runner[livejournal.EmitKeyRunnerField]; emitted {
		return ""
	}
	if event.Type != journal.EventRunnerAnnotation || event.Runner["operation"] != "recovery-retained" {
		return ""
	}
	parts := []string{event.RunID}
	for _, field := range []string{"recoveryRef", "recoveryRepositoryKey", "recoveryRetainUntil"} {
		value, ok := event.Runner[field].(string)
		if !ok || value == "" {
			return ""
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, "\x00")
}
