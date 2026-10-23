package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

func (b *fileBackend) CommitTerminalIntervention(_ context.Context, h RunHandle, owner OwnerToken, req TerminalIntervention) (TerminalInterventionReceipt, error) {
	run, _, err := b.writeRun(h, owner)
	if err != nil {
		return TerminalInterventionReceipt{}, err
	}
	return run.CommitTerminalIntervention(req)
}

// CommitTerminalIntervention holds the existing run lock across validation
// and each prefix append. A crash between marker and resume remains closed to
// ordinary emits; the identical intent can finish that durable prefix.
func (r *Run) CommitTerminalIntervention(req TerminalIntervention) (TerminalInterventionReceipt, error) {
	r.mu.Lock()
	var observed uint64
	defer func() {
		r.mu.Unlock()
		if observed != 0 && r.observer != nil {
			r.observer(r.id.RunID, observed)
		}
	}()
	if r.closed {
		return TerminalInterventionReceipt{}, ErrClosed
	}
	clean, err := r.cleanIntervention(req)
	if err != nil {
		return TerminalInterventionReceipt{}, err
	}
	history, _, err := readEvents(filepath.Join(r.dir, fileEvents))
	if err != nil {
		return TerminalInterventionReceipt{}, err
	}
	check, err := NewTerminalInterventionCheck(clean, history)
	if err != nil {
		return TerminalInterventionReceipt{}, err
	}
	skip, receipt, err := check.Remaining()
	if err != nil {
		return receipt, interventionError(err)
	}
	for _, event := range clean.Events[skip:] {
		if err := r.append(event); err != nil {
			return receipt, err
		}
		r.applyLifecycle(event)
		receipt.Seq = r.seq
		observed = r.seq
		if err := r.checkpoint(); err != nil {
			return receipt, err
		}
	}
	return receipt, nil
}

func (r *Run) cleanIntervention(req TerminalIntervention) (TerminalIntervention, error) {
	clean := req
	clean.Events = make([]Event, len(req.Events))
	for i, event := range req.Events {
		event.Seq = r.seq + uint64(i) + 1
		event.Schema = EventSchema
		event.Time = r.now()
		raw, err := marshalEvent(event)
		if err != nil {
			return clean, err
		}
		raw = r.scrubber.Scrub(raw)
		if len(raw)+1 > maxEventBytes {
			return clean, fmt.Errorf("journal: event exceeds %d-byte limit", maxEventBytes)
		}
		if err := json.Unmarshal(raw, &clean.Events[i]); err != nil {
			return clean, err
		}
	}
	return clean, nil
}
