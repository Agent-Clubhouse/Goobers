package livejournal

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
)

// AppendReceipt identifies one durably committed event, not a batch watermark.
// It is returned only when the append explicitly requests a receipt.
type AppendReceipt struct {
	Key      string            `json:"key"`
	Seq      uint64            `json:"seq"`
	Type     journal.EventType `json:"type"`
	Branch   int               `json:"branch"`
	Parallel string            `json:"parallel,omitempty"`
}

// Validate binds the receipt to the requested append and its branch scope.
func (r AppendReceipt) Validate(op Op) error {
	if !op.Receipt || op.Kind != OpAppend || op.Event == nil || r.Key != op.Key || r.Seq == 0 {
		return fmt.Errorf("livejournal: invalid append receipt for %q", op.Key)
	}
	branch := op.Event.Branch
	if op.Branch != 0 {
		branch = op.Branch
	}
	if r.Type != op.Event.Type || r.Branch != branch || r.Parallel != op.Event.Parallel {
		return fmt.Errorf("livejournal: append receipt differs from event %q", op.Key)
	}
	return nil
}

// appendReceipt reads the committed event under the writer lock. Lifecycle
// receipts are infrequent; deriving them from the journal avoids a second
// cache whose recovery could diverge from the durable emit-key index.
func (w *Writer) appendReceipt(run *liveRun, op Op) (AppendReceipt, error) {
	// Recovery can retain an append whose checkpoint failed. Deduplication
	// alone cannot authorize a receipt: retry the checkpoint through the
	// active owner, whether it is a borrowed journal or a backend handle.
	if w.hasRun(run) {
		var err error
		if run.adopted {
			err = run.jr.Checkpoint()
		} else {
			err = w.backend.Checkpoint(context.Background(), run.handle, run.owner)
		}
		if err != nil {
			return AppendReceipt{}, err
		}
	}
	reader, err := w.runReader(context.Background(), run)
	if err != nil {
		return AppendReceipt{}, err
	}
	events, err := reader.Events()
	if err != nil {
		return AppendReceipt{}, err
	}
	seq := run.keys[op.Key]
	for _, event := range events {
		key, _ := event.Runner[EmitKeyRunnerField].(string)
		if event.Seq != seq || key != op.Key {
			continue
		}
		receipt := AppendReceipt{Key: op.Key, Seq: seq, Type: event.Type, Branch: event.Branch, Parallel: event.Parallel}
		return receipt, receipt.Validate(op)
	}
	return AppendReceipt{}, fmt.Errorf("livejournal: no committed append for %q", op.Key)
}

// requestedAppendReceipts validates the optional protocol before opening a writer.
func requestedAppendReceipts(ops []Op) (bool, error) {
	requested := false
	for i, op := range ops {
		if !op.Receipt {
			continue
		}
		if op.Kind != OpAppend || op.Event == nil {
			return false, fmt.Errorf("livejournal: op %d requests a receipt without an append", i)
		}
		requested = true
	}
	return requested, nil
}
