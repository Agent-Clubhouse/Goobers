package runner

import (
	"fmt"

	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// pendingFanOut is the latest fan-out visit that has started and not
// finished (design plan-driven-dynamic-fan-out.md §8 step 1).
type pendingFanOut struct {
	// start indexes the visit's parallel.started or map.started; -1 when no
	// visit is pending.
	start int
	// mapVisit is the verified roster of a pending map visit; nil for a
	// static parallel.
	mapVisit *fanout.MapVisit
}

// findPendingFanOut locates the latest unmatched parallel.started or
// map.started. A run.resumed starts a new run segment, so a visit does not
// cross it, except an intervention that reopens a static parallel branch at
// its join; run.finished alone does not end a visit, because RerunStage can
// reopen a branch.
//
// Every map visit the scan passes over, pending or already finished, is
// verified against its frozen roster as it goes (§8 steps 3-5). Any missing,
// extra, duplicate or conflicting record refuses with a typed
// *fanout.IntegrityError (workflow_integrity) and is never repaired by
// rerunning the source task. Static parallels keep their existing lenient
// reconstruction, so their resume behavior is unchanged.
func findPendingFanOut(events []journal.Event, machine *workflow.Machine) (pendingFanOut, error) {
	pending := pendingFanOut{start: -1}
	for i, event := range events {
		if pending.mapVisit != nil && event.Type != journal.EventMapStarted && event.Type != journal.EventMapFinished {
			if err := pending.mapVisit.Observe(event); err != nil {
				return pendingFanOut{start: -1}, err
			}
		}
		if err := pending.advance(events, i, machine); err != nil {
			return pendingFanOut{start: -1}, err
		}
	}
	return pending, nil
}

func (p *pendingFanOut) advance(events []journal.Event, i int, machine *workflow.Machine) error {
	event := events[i]
	switch event.Type {
	case journal.EventParallelStarted:
		p.start, p.mapVisit = i, nil
	case journal.EventMapStarted:
		return p.openMap(event, i, machine)
	case journal.EventParallelFinished:
		if p.start >= 0 && p.mapVisit == nil && event.Parallel == events[p.start].Parallel {
			p.start = -1
		}
	case journal.EventMapFinished:
		if p.mapVisit == nil || event.Parallel != p.mapVisit.Name() {
			return &fanout.IntegrityError{Kind: fanout.KindMissing, FanOut: event.Parallel, Seq: event.Seq,
				Detail: "map.finished has no open map.started visit"}
		}
		if err := p.mapVisit.VerifyFinished(event); err != nil {
			return err
		}
		p.start, p.mapVisit = -1, nil
	case journal.EventRunResumed:
		p.resume(events, i, machine)
	}
	return nil
}

func (p *pendingFanOut) openMap(event journal.Event, i int, machine *workflow.Machine) error {
	if p.mapVisit != nil {
		return &fanout.IntegrityError{Kind: fanout.KindDuplicate, FanOut: event.Parallel, Seq: event.Seq,
			Detail: fmt.Sprintf("map.started while map %q visit is still open", p.mapVisit.Name())}
	}
	def, ok := machine.Map(event.Parallel)
	if !ok {
		return &fanout.IntegrityError{Kind: fanout.KindConflicting, FanOut: event.Parallel, Seq: event.Seq,
			Detail: "map.started names a map the pinned definition does not declare"}
	}
	visit, err := fanout.NewMapVisit(event, def)
	if err != nil {
		return err
	}
	p.start, p.mapVisit = i, visit
	return nil
}

func (p *pendingFanOut) resume(events []journal.Event, i int, machine *workflow.Machine) {
	event := events[i]
	if event.Target != workflow.TargetJoin {
		p.start, p.mapVisit = -1, nil
		return
	}
	parallel, _, ok := interventionParallelContext(events[:i], machine, event.Gate)
	if !ok {
		// A join intervention inside a map item keeps the visit open; the
		// map verifier lets that item settle once more.
		return
	}
	for candidate := i - 1; candidate >= 0; candidate-- {
		if events[candidate].Type == journal.EventParallelStarted && events[candidate].Parallel == parallel {
			p.start, p.mapVisit = candidate, nil
			return
		}
	}
}

// exec rebuilds the pending visit's branch set from its start event alone
// (§8 step 2): a static parallel's declared branches minus the skip set its
// parallel.started selection recorded, or a map's frozen roster. It returns
// nil when the visit names a state the machine lacks.
func (p pendingFanOut) exec(events []journal.Event, machine *workflow.Machine) *parallelExec {
	start := events[p.start]
	if p.mapVisit != nil {
		def, _ := machine.Map(start.Parallel)
		roster := *start.MapRoster
		roster.Items = p.mapVisit.Items()
		return newMapExec(def, roster, start.Seq)
	}
	spec, ok := machine.Parallel(start.Parallel)
	if !ok {
		return nil
	}
	par := newParallelExec(spec)
	// A selective visit's skip set comes from its parallel.started alone.
	par.applySelection(start.Selection)
	return par
}
