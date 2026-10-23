package engine

import (
	"fmt"

	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func (o *journalCommitOwner) preserveAppendReceipts(ops []livejournal.Op, receipts []livejournal.AppendReceipt, watermark uint64) error {
	pending := make(map[string]livejournal.AppendReceipt)
	next := 0
	for _, op := range ops {
		if !op.Receipt {
			continue
		}
		if next >= len(receipts) {
			return fmt.Errorf("engine: missing lifecycle receipt for %q", op.Key)
		}
		receipt := receipts[next]
		next++
		if receipt.Seq > watermark {
			return fmt.Errorf("engine: lifecycle receipt exceeds acknowledged watermark")
		}
		if err := receipt.Validate(op); err != nil {
			return err
		}
		if prior, ok := o.receipts[op.Key]; ok && prior != receipt {
			return fmt.Errorf("engine: changed lifecycle receipt for %q", op.Key)
		}
		if _, duplicate := pending[op.Key]; duplicate {
			return fmt.Errorf("engine: duplicate lifecycle receipt for %q", op.Key)
		}
		pending[op.Key] = receipt
	}
	if next != len(receipts) {
		return fmt.Errorf("engine: unexpected lifecycle receipts")
	}
	if o.receipts == nil {
		o.receipts = make(map[string]livejournal.AppendReceipt)
	}
	for key, receipt := range pending {
		o.receipts[key] = receipt
	}
	return nil
}

// commitLifecycle returns only an acknowledged per-event identity. The owner
// retains its original emit key if an acknowledgement is lost or incomplete.
func (r *runJournal) commitLifecycle(ctx workflow.Context, event journal.Event) (livejournal.AppendReceipt, error) {
	if r.owner == nil {
		return livejournal.AppendReceipt{}, fmt.Errorf("engine: lifecycle requires a commit owner")
	}
	r.append(ctx, event)
	i := len(r.proj.Ops) - 1
	r.proj.Ops[i].Receipt = true
	if err := r.emitPending(ctx); err != nil {
		return livejournal.AppendReceipt{}, err
	}
	receipt, ok := r.owner.receipts[r.proj.Ops[i].EmitKey]
	if !ok {
		return livejournal.AppendReceipt{}, fmt.Errorf("engine: lifecycle identity not acknowledged")
	}
	return receipt, nil
}
