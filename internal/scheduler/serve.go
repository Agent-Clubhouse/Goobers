package scheduler

import "context"

// DecisionHandler observes the outcome of each dispatched event. err is non-nil
// only on an unexpected scheduler/start failure (a blocked or already-running
// event is a normal Decision, not an error).
type DecisionHandler func(ev Event, d Decision, err error)

// Serve consumes events until the channel closes or ctx is cancelled,
// dispatching each. A per-event error is reported to handler (if set) and does
// not stop the loop — one bad item must not stall the gaggle.
func (s *Scheduler) Serve(ctx context.Context, events <-chan Event, handler DecisionHandler) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			d, err := s.Dispatch(ctx, ev)
			if handler != nil {
				handler(ev, d, err)
			}
		}
	}
}
