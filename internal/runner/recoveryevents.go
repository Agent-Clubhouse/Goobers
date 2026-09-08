package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type recoveryEventJournal interface {
	AppendBatchIfAbsent(context.Context, []journal.Event, func(journal.Event) string) (int, error)
}

func (r *Runner) recordRecoveryAfterCleanup(ctx context.Context, log recoveryEventJournal, runID string, cleanupErr error) error {
	if cleanupErr != nil {
		return cleanupErr
	}
	return r.recordRecoveryEvents(context.WithoutCancel(ctx), log, runID)
}

func (r *Runner) recordRecoveryEvents(ctx context.Context, log recoveryEventJournal, runID string) error {
	if r.cfg.RecoveryEvents == nil {
		return nil
	}
	events, err := r.cfg.RecoveryEvents(ctx, runID)
	if err != nil {
		return err
	}
	if len(events) > 128 {
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
	_, err = log.AppendBatchIfAbsent(ctx, events, recoveryObservationKey)
	return err
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
