package journalpg

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/journal"
)

// CommitTerminalIntervention checks the exact generation and intent binding
// under the live lease and owner-token fence; receipt and reopening events
// commit atomically with their checkpoint. Duplicate checks are fenced too.
func (b *Backend) CommitTerminalIntervention(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, req journal.TerminalIntervention) (journal.TerminalInterventionReceipt, error) {
	r, err := b.writeRun(h, o)
	if err != nil {
		return journal.TerminalInterventionReceipt{}, err
	}
	clean := req
	clean.Events = make([]journal.Event, len(req.Events))
	for i, event := range req.Events {
		clean.Events[i], err = prepareEvent(r, event)
		if err != nil {
			return journal.TerminalInterventionReceipt{}, err
		}
	}
	var receipt journal.TerminalInterventionReceipt
	err = b.withHistory(ctx, r, func(s historySnapshot) (tailWrite, error) {
		check, err := journal.NewTerminalInterventionCheck(clean, s.events)
		if err != nil {
			return nil, err
		}
		replay := newReplayState(r.key.RunID, s.events)
		return func(ctx context.Context, tx pgx.Tx, seq int64, tail []journal.Event) error {
			check.Observe(tail)
			replay.fold(tail)
			skip, accepted, err := check.Remaining()
			if err != nil {
				return err
			}
			receipt = accepted
			if receipt.Duplicate {
				return nil
			}
			for _, event := range clean.Events[skip:] {
				var stored journal.Event
				seq, stored, err = b.appendClean(ctx, tx, r, seq, event)
				if err != nil {
					return err
				}
				receipt.Seq = stored.Seq
				replay.fold([]journal.Event{stored})
			}
			checkpoint, err := replay.checkpoint()
			if err != nil {
				return err
			}
			return b.storeCheckpoint(ctx, tx, r, checkpoint)
		}, nil
	})
	if err != nil {
		return journal.TerminalInterventionReceipt{}, err
	}
	if !receipt.Duplicate {
		b.changed(r, receipt.Seq)
	}
	return receipt, nil
}

var _ journal.TerminalInterventionBackend = (*Backend)(nil)
