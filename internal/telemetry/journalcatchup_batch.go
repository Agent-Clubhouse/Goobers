package telemetry

import (
	"context"
	"time"
)

// Sparse committed hints may arrive before the producer finishes its state
// checkpoint. Give them one bounded window to coalesce before background disk
// work starts. This is worker-only: Commit remains a nonblocking hint offer.
// The window is not restarted by arrivals. Full input batches and known retained
// backlog do not wait, and explicit flushes use coalesceHints without a timer.
func (s *journalCatchup) collectHints(ctx context.Context, first journalCatchupHint) []journalCatchupHint {
	batch := newJournalHintBatch(first)
	timer := time.NewTimer(journalLogBatchDelay)
	defer timer.Stop()
	for !batch.full() {
		select {
		case <-ctx.Done():
			return batch.hints
		case <-timer.C:
			return batch.hints
		case hint := <-s.hints:
			batch.add(hint)
		}
	}
	return batch.hints
}

type journalHintBatch struct {
	hints        []journalCatchupHint
	positions    map[string]int
	count, bytes int
	resume       bool
}

func newJournalHintBatch(first journalCatchupHint) *journalHintBatch {
	batch := &journalHintBatch{positions: make(map[string]int)}
	batch.add(first)
	return batch
}

func (b *journalHintBatch) add(hint journalCatchupHint) {
	b.count++
	b.bytes += hint.bytes
	b.resume = b.resume || hint.resume
	if i, ok := b.positions[hint.dir]; ok {
		if b.hints[i].identity != hint.identity || hint.seq > b.hints[i].seq {
			b.hints[i] = hint
		}
		return
	}
	b.positions[hint.dir] = len(b.hints)
	b.hints = append(b.hints, hint)
}

func (b *journalHintBatch) full() bool {
	return b.resume || b.count >= journalLogBatchLimit || b.bytes >= journalLogBatchBytes
}
