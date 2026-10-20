package engine

import (
	"errors"
	"github.com/goobers/goobers/internal/livejournal"
)

// preserveChildOrigins validates the entire response before advancing any
// receipt state. Legacy batches/responses are unchanged; a newly opted-in batch
// cannot use an old writer's watermark as a substitute for committed identity.
func (r *runJournal) preserveChildOrigins(ops []livejournal.Op, receipts []livejournal.ChildOriginReceipt) error {
	expected := make(map[string]livejournal.Op)
	for _, op := range ops {
		if op.ChildStageStart != nil {
			if _, dup := expected[op.Key]; dup {
				return errors.New("engine: duplicate child-origin request key")
			}
			expected[op.Key] = op
		}
	}
	if len(expected) != len(receipts) {
		return errors.New("engine: missing or unexpected committed child origins")
	}
	verified := make(map[string]livejournal.ChildOriginReceipt, len(receipts))
	for _, receipt := range receipts {
		op, ok := expected[receipt.Key]
		if !ok {
			return errors.New("engine: unexpected committed child origin key")
		}
		if _, dup := verified[receipt.Key]; dup {
			return errors.New("engine: duplicate committed child origin key")
		}
		if err := receipt.Validate(r.proj.Identity.RunID, op); err != nil {
			return err
		}
		if prior, ok := r.childOrigins[receipt.Key]; ok && prior != receipt {
			return errors.New("engine: committed child origin changed on retry")
		}
		verified[receipt.Key] = receipt
	}
	if len(verified) > 0 && r.childOrigins == nil {
		r.childOrigins = make(map[string]livejournal.ChildOriginReceipt)
	}
	for key, receipt := range verified {
		r.childOrigins[key] = receipt
	}
	return nil
}
