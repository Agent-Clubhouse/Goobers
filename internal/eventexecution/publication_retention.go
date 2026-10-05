package eventexecution

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// SettlePublicationSweep fences a bounded page of non-resumable producers.
// Registry custody excludes in-process owners and TryRecover excludes external
// journal writers. Both remain held through the exact terminal queue receipt.
func (s *Service) SettlePublicationSweep(ctx context.Context, after string) (string, error) {
	page, err := s.Queue.EventPublicationPage(ctx, after, 100)
	if err != nil {
		return after, err
	}
	var failures error
	for _, p := range page {
		if err = ctx.Err(); err != nil {
			return after, errors.Join(failures, err)
		}
		after = p.ID
		if !p.SettledAt.IsZero() {
			continue
		}
		failures = errors.Join(failures, s.settlePublication(ctx, p))
	}
	if len(page) < 100 {
		after = ""
	}
	return after, failures
}

func (s *Service) settlePublication(ctx context.Context, p triggerqueue.EventPublication) error {
	if s.AcquireTerminal == nil || s.RunDirectory == nil || s.Now == nil {
		return nil
	}
	release, ok := s.AcquireTerminal(p.Acceptance.Producer.RunID)
	if !ok {
		return nil
	}
	defer release()
	directory, err := s.RunDirectory(ctx, p.Acceptance.Producer.RunID)
	if err != nil {
		return err
	}
	if directory == "" {
		return errors.New("event publication producer journal unavailable")
	}
	writer, _, err := journal.TryRecover(directory)
	if errors.Is(err, journal.ErrRecoveryBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = writer.Close() }()
	reader, err := journal.OpenReadOnly(directory)
	if err != nil {
		return err
	}
	proof, settled, err := publicationTerminalProof(reader, p)
	if err != nil || !settled {
		return err
	}
	return s.Queue.SettleEventPublication(ctx, p.ID, proof, s.Now())
}

func publicationTerminalProof(reader *journal.Reader, p triggerqueue.EventPublication) (triggerqueue.EventProducerTerminal, bool, error) {
	var proof triggerqueue.EventProducerTerminal
	id, err := reader.Identity()
	if err != nil {
		return proof, false, err
	}
	if id.Gaggle != p.Acceptance.Producer.Gaggle || id.RunID != p.Acceptance.Producer.RunID || id.ConfigGeneration != p.ConfigGeneration || id.Child != nil || id.ContinuedFromRunID != "" || id.EngineDriven() {
		return proof, false, errors.New("event publication terminal identity differs")
	}
	events, err := reader.Events()
	if err != nil {
		return proof, false, err
	}
	phase := journal.PhaseFromEvents(events)
	if phase != journal.PhaseCompleted && phase != journal.PhaseAborted {
		return proof, false, nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != journal.EventRunFinished {
			continue
		}
		if e.Status != string(phase) || e.Time.IsZero() || e.Seq == 0 {
			return proof, false, errors.New("event publication terminal sequence differs")
		}
		return triggerqueue.EventProducerTerminal{Gaggle: id.Gaggle, RunID: id.RunID, ConfigGeneration: id.ConfigGeneration, Phase: string(phase), Sequence: e.Seq}, true, nil
	}
	return proof, false, nil
}
