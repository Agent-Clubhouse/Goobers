package readmodel

import "context"

// Where stage measurement enters the projection (#1782, design §5.7).
//
// # The data is not in the journal
//
// Token counts, cost, and premium-request figures do not appear on any journal
// event. They exist only in the telemetry rollup, written from spans on a
// different clock. So `ProjectRun`, which is a pure fold over journal events and
// must stay one (§10), cannot compute the measurement flags — no amount of
// reading the journal harder would produce them.
//
// # Why the source attaches to the Store rather than to each writer
//
// Three writers project runs: the live projector, the initial build, and the
// repair sweep. All three reach `UpsertRun`, and none of them should have to
// remember to apply measurement — a writer that forgot would silently clear
// every flag on the runs it touched, because stage rows are rewritten wholesale.
// That failure is invisible: the run still lists, it just stops matching a
// population filter it used to match.
//
// Attaching the source to the Store puts the merge at the one point all three
// share, so forgetting is not possible.
//
// # The cost, and why it is in the right place now
//
// One telemetry query per run PROJECTION. #1782's complaint was one telemetry
// query per candidate EXAMINED, inside an HTTP request, with no cap on
// candidates — a filter matching few runs issued ~19,852 of them in one request.
// The same query moved to the write path runs once per run change, off the
// request path, and answers every later filter from an index.

// MeasurementSource supplies telemetry-derived stage measurement for a run.
//
// Returning a nil slice and a nil error means "nothing to say", which preserves
// whatever flags the projection already carried. Returning an empty non-nil
// slice means "this run has no measured stages", which clears them. The
// distinction matters because a telemetry store that has not yet ingested a
// just-finished run should not be read as evidence that the run has no cost.
type MeasurementSource interface {
	RunMeasurement(ctx context.Context, runID string) ([]StageMeasurement, error)
}

// WithMeasurement attaches a measurement source to the store.
//
// Returns the store so it can be chained onto Open. Passing nil detaches, which
// is what a topology without telemetry does — and detaching is safe precisely
// because a nil source means "nothing to say" rather than "no measurements".
func (s *Store) WithMeasurement(source MeasurementSource) *Store {
	s.measurement = source
	return s
}

// applyMeasurement merges telemetry measurement into a projection before it is
// written.
//
// Called OUTSIDE the projection transaction, deliberately: telemetry lives in a
// different database, and holding the read model's single writer open across a
// query to another store would put an unrelated store's latency — and its
// failures — on the read model's write lock.
//
// A measurement failure is not a projection failure. Losing a population flag
// costs a filter match; refusing the projection would lose the run from every
// list. The error is returned so a caller that wants to care can, and UpsertRun
// deliberately does not.
func (s *Store) applyMeasurement(ctx context.Context, p *Projection) {
	if s.measurement == nil {
		// Still roll up from whatever the stage rows carry, so the two grains
		// cannot disagree even with no source attached.
		p.rollUpMeasurement()
		return
	}
	measurements, err := s.measurement.RunMeasurement(ctx, p.Run.RunID)
	if err != nil {
		// Nil, not empty: an error is "nothing to say", not "no measurements".
		// Clearing on a transient telemetry failure would make a population
		// filter's results depend on whether telemetry happened to be up when the
		// run was last projected.
		p.ApplyMeasurement(nil)
		return
	}
	p.ApplyMeasurement(measurements)
}
