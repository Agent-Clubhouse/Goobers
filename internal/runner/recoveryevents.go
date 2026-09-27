package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

const recoveryObservationFailureCode = "recovery_observation_failed"
const recoveryObservationBatchSize = 128 // journal.AppendBatchIfAbsent's generic transaction ceiling

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
		code = recoveryObservationFailureCode
	}
	return &journal.ErrorDetail{Code: code, Message: err.Error()}
}

type recoveryEventJournal interface {
	AppendBatchIfAbsent(context.Context, []journal.Event, func(journal.Event) string) (int, error)
}

func (r *Runner) recordRecoveryAfterCleanup(ctx context.Context, log recoveryEventJournal, runID string, cleanupErr error) error {
	if cleanupErr != nil {
		return cleanupErr
	}
	if err := r.recordRecoveryEvents(context.WithoutCancel(ctx), log, runID); err != nil {
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
