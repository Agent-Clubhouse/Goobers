package localscheduler

import "time"

// SkewTolerantState tolerates a successor whose clock is behind the
// predecessor's. The two controllers run on different nodes, so the evaluation
// time the successor reconstructs at Prepare can be earlier than the last one the
// predecessor stored; the store refuses a regression, which would fail Prepare
// (and flap the lease) until the skew passes. Writes are clamped to the stored
// value instead: the persisted cursor never moves backward, and the scheduler's
// in-memory evaluation time stays at its own (earlier) value. That is safe
// because evaluating an already-evaluated span again cannot fire it twice:
// occurrence acceptance is exactly-once through the source cursor and receipts
// in the acceptance store, independent of this baseline. The local daemon is
// one host with one clock, so it has no such regression to tolerate.
type SkewTolerantState struct{ SchedulerState }

// WriteTriggerEvaluations clamps each evaluation to its last persisted value.
func (s SkewTolerantState) WriteTriggerEvaluations(dir string, evaluations map[WorkflowIdentity]time.Time) error {
	stored, err := s.ReadTriggerEvaluations(dir)
	if err != nil {
		return err
	}
	clamped := make(map[WorkflowIdentity]time.Time, len(evaluations))
	for identity, at := range evaluations {
		if previous, ok := stored[identity]; ok && at.Before(previous) {
			at = previous
		}
		clamped[identity] = at
	}
	return s.SchedulerState.WriteTriggerEvaluations(dir, clamped)
}
