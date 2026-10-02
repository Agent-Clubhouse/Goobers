package livejournal

import (
	"errors"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// OperatorMessageJournal binds lifecycle writes to the writer that already
// owns the run lock. The binding never exposes or retains a borrowed Run handle.
type OperatorMessageJournal struct {
	writer *Writer
	gaggle string
	runID  string
}

// OperatorMessages returns a scoped operator-message backend. Every operation
// reacquires the current handle, including after idle closure or adoption ends.
func (w *Writer) OperatorMessages(gaggle, runID string) *OperatorMessageJournal {
	return &OperatorMessageJournal{writer: w, gaggle: gaggle, runID: runID}
}

func (b *OperatorMessageJournal) withRun(apply func(*journal.Run) error) error {
	w := b.writer
	if w == nil || !apiv1.ValidRunID(b.runID) {
		return errors.New("livejournal: invalid operator-message journal")
	}
	runsDir, ok := w.runsDir(b.gaggle)
	if !ok {
		return errors.New("livejournal: unknown operator-message gaggle")
	}
	for {
		w.mu.Lock()
		run := w.open[b.runID]
		w.mu.Unlock()
		if run != nil {
			run.mu.Lock()
			if run.gaggle != b.gaggle {
				run.mu.Unlock()
				return errors.New("livejournal: operator-message gaggle mismatch")
			}
			if run.jr != nil {
				run.lastEmit = w.now()
				if run.clock != nil {
					run.clock.set(run.lastEmit)
				}
				err := apply(run.jr)
				run.mu.Unlock()
				return err
			}
			run.mu.Unlock()
		}
		release, reserved := w.Reserve(b.runID)
		if !reserved {
			// An emitter, idle closer, or repair owns the transition. Observe its
			// completion before retrying; never Recover beside an active writer.
			w.mu.Lock()
			wait := w.reserved[b.runID]
			w.mu.Unlock()
			if wait != nil {
				<-wait
			}
			continue
		}
		err := b.recoverAndApply(filepath.Join(runsDir, b.runID), apply)
		release()
		return err
	}
}

func (b *OperatorMessageJournal) recoverAndApply(dir string, apply func(*journal.Run) error) error {
	opts := []journal.Option{journal.WithClock(b.writer.now)}
	if b.writer.scrubber != nil {
		opts = append(opts, journal.WithScrubber(b.writer.scrubber))
	}
	if b.writer.observer != nil {
		opts = append(opts, journal.WithAppendObserver(b.writer.observer))
	}
	run, _, err := journal.Recover(dir, opts...)
	if err != nil {
		return err
	}
	defer func() { _ = run.Close() }()
	return apply(run)
}

// AcceptOperatorMessage durably deduplicates one request through the shared writer.
func (b *OperatorMessageJournal) AcceptOperatorMessage(request apiv1.OperatorMessageRequest) (record apiv1.OperatorMessageRecord, accepted bool, err error) {
	err = b.withRun(func(run *journal.Run) error {
		var applyErr error
		record, accepted, applyErr = run.AcceptOperatorMessage(request)
		return applyErr
	})
	return
}

// RejectOperatorMessage records an authorization denial through the shared writer.
func (b *OperatorMessageJournal) RejectOperatorMessage(request apiv1.OperatorMessageRequest, code, detail string) (record apiv1.OperatorMessageRecord, accepted bool, err error) {
	err = b.withRun(func(run *journal.Run) error {
		var applyErr error
		record, accepted, applyErr = run.RejectOperatorMessage(request, code, detail)
		return applyErr
	})
	return
}

// AcknowledgeOperatorMessage records adapter acknowledgement through the shared writer.
func (b *OperatorMessageJournal) AcknowledgeOperatorMessage(ack apiv1.OperatorMessageAcknowledgement) (record apiv1.OperatorMessageRecord, err error) {
	err = b.withRun(func(run *journal.Run) error {
		var applyErr error
		record, applyErr = run.AcknowledgeOperatorMessage(ack)
		return applyErr
	})
	return
}

// CompleteOperatorMessage records the typed delivery outcome through the shared writer.
func (b *OperatorMessageJournal) CompleteOperatorMessage(outcome apiv1.OperatorMessageOutcome) (record apiv1.OperatorMessageRecord, err error) {
	err = b.withRun(func(run *journal.Run) error {
		var applyErr error
		record, applyErr = run.CompleteOperatorMessage(outcome)
		return applyErr
	})
	return
}
