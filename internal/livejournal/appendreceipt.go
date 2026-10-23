package livejournal

import (
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
func (run *liveRun) appendReceipt(op Op) (AppendReceipt, error) {
	if run.jr != nil {
		if err := run.jr.Checkpoint(); err != nil {
			return AppendReceipt{}, err
		}
	}
	reader, err := journal.OpenReadOnly(run.dir)
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
