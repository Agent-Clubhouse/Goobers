package runner

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
)

// freezeMapRoster freezes one map visit's roster (design
// plan-driven-dynamic-fan-out.md §4.2, §6.2) and returns the map.started event
// to append before any item starts. events is the run journal so far and
// journalDir the run directory its artifacts live under.
//
// The roster comes only from the producer's journal-bound attempt; the
// returned event is the durable source of truth for the visit, so neither
// execution nor resume consults the producer again. Every source failure is a
// typed *fanout.SourceError (fanout_source_invalid); an I/O failure reading
// the journal is returned as-is.
//
// No production caller exists yet: map execution, which appends this event
// and lifts the run-start refusal, is FAN-4 (#7385).
func freezeMapRoster(ctx context.Context, journalDir string, events []journal.Event, m apiv1.Map) (journal.Event, error) {
	source, data, err := resolveFanOutSource(ctx, journalDir, events, m.Source)
	if err != nil {
		return journal.Event{}, err
	}
	items, err := fanout.FreezeRoster(data, source.MediaType)
	if err != nil {
		return journal.Event{}, err
	}
	return journal.Event{
		Type:     journal.EventMapStarted,
		Parallel: m.Name,
		MapRoster: &journal.MapRoster{
			Source:             source,
			FailurePolicy:      m.FailurePolicy,
			ItemTimeoutSeconds: m.ItemTimeoutSeconds,
			MaxConcurrent:      fanout.EffectiveMaxConcurrent(m),
			Items:              items,
		},
	}, nil
}

// resolveFanOutSource resolves a fan-out source slot from the producer's
// journal-bound attempt (design §4.2 step 1): the newest settled root-branch
// attempt of source.Task, which must have succeeded and whose named
// artifact-set index must bind source.Slot to that same attempt's publication
// visit. The media type and size are checked from the bound pointer before a
// byte is read; the bytes are then read digest-verified.
func resolveFanOutSource(ctx context.Context, journalDir string, events []journal.Event, source apiv1.FanOutSource) (journal.FanOutSourceRef, []byte, error) {
	invalid := func(reason, format string, args ...any) (journal.FanOutSourceRef, []byte, error) {
		return journal.FanOutSourceRef{}, nil, &fanout.SourceError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
	}
	started, finished, ok := producerAttempt(events, source.Task)
	if !ok {
		return invalid(fanout.ReasonProducer, "source task %q has no settled attempt on the root branch", source.Task)
	}
	if finished.Status != string(apiv1.ResultSuccess) {
		return invalid(fanout.ReasonProducer, "source task %q attempt %d finished %q, not success", source.Task, finished.Attempt, finished.Status)
	}
	reader, err := artifactset.OpenJournal(journalDir)
	if err != nil {
		return journal.FanOutSourceRef{}, nil, err
	}
	defer func() { _ = reader.Close() }()
	pointers := contextPointersFor(source.Task, artifactPointersFrom(finished.Artifacts))
	binding, err := artifactset.LookupSlot(ctx, reader, pointers, source.Task, source.Slot)
	if err != nil {
		if errors.Is(err, artifactset.ErrInvalid) {
			return invalid(fanout.ReasonProducer, "source slot %s.%s: %v", source.Task, source.Slot, err)
		}
		return journal.FanOutSourceRef{}, nil, err
	}
	if binding.Stage != source.Task || binding.Visit != started.Seq || int(binding.Attempt) != finished.Attempt {
		return invalid(fanout.ReasonProducer, "source slot %s.%s is bound to visit %d attempt %d, not the journal-bound attempt %d at seq %d",
			source.Task, source.Slot, binding.Visit, binding.Attempt, finished.Attempt, started.Seq)
	}
	pointer, ok := journaledSourcePointer(finished, binding.Artifact)
	if !ok {
		return invalid(fanout.ReasonProducer, "source slot %s.%s is not among the attempt's journaled artifacts", source.Task, source.Slot)
	}
	if pointer.MediaType != fanout.SourceMediaType {
		return invalid(fanout.ReasonMediaType, "source slot %s.%s has media type %q, want %q", source.Task, source.Slot, pointer.MediaType, fanout.SourceMediaType)
	}
	if pointer.Size > fanout.MaxSourceBytes {
		return invalid(fanout.ReasonTooLarge, "source slot %s.%s is %d bytes, bound is %d", source.Task, source.Slot, pointer.Size, fanout.MaxSourceBytes)
	}
	data, err := artifactset.ReadVerified(ctx, reader, pointer, fanout.MaxSourceBytes)
	if err != nil {
		if errors.Is(err, artifactset.ErrInvalid) {
			return invalid(fanout.ReasonProducer, "source slot %s.%s: %v", source.Task, source.Slot, err)
		}
		return journal.FanOutSourceRef{}, nil, err
	}
	return journal.FanOutSourceRef{
		Task: source.Task, Slot: source.Slot, Visit: started.Seq, Attempt: finished.Attempt,
		Digest: pointer.Digest, Integrity: pointer.Integrity,
		Path: pointer.Path, Size: pointer.Size, MediaType: pointer.MediaType,
	}, data, nil
}

// producerAttempt returns the newest settled root-branch attempt of task:
// its stage.finished (interrupted-attempt markers excluded) and the matching
// stage.started that opened it. Re-entering the producer supersedes an older
// attempt; otherwise the same recorded attempt is used again (design §4.4).
func producerAttempt(events []journal.Event, task string) (journal.Event, journal.Event, bool) {
	finished, ok := latestProducerAttempt(events, task)
	if !ok {
		return journal.Event{}, journal.Event{}, false
	}
	for i := len(events) - 1; i >= 0; i-- {
		started := events[i]
		if started.Seq < finished.Seq && started.Type == journal.EventStageStarted && started.Branch == 0 &&
			started.Stage == task && started.Attempt == finished.Attempt {
			return started, finished, true
		}
	}
	return journal.Event{}, journal.Event{}, false
}

// journaledSourcePointer finds the bound artifact among the attempt's
// journaled artifacts, whose integrity is authoritative: the stage result
// normalized it before it was journaled.
func journaledSourcePointer(finished journal.Event, bound apiv1.ArtifactPointer) (apiv1.ArtifactPointer, bool) {
	for _, pointer := range artifactPointersFrom(finished.Artifacts) {
		if pointer.Digest == bound.Digest && pointer.Path == bound.Path && pointer.Size == bound.Size {
			if !pointer.Integrity.Valid() {
				pointer.Integrity = apiv1.IntegrityDerived
			}
			return pointer, true
		}
	}
	return apiv1.ArtifactPointer{}, false
}
