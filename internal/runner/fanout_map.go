package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/workflow"
)

// Map execution on the local runner (design plan-driven-dynamic-fan-out.md
// §4.3, §6, §9). A map visit freezes its roster into map.started, expands it
// into the static parallel branch representation, and runs every item through
// the bounded parallel dispatcher: the worker pool stays the only concurrency
// mechanism. The join receives mapCompleteness and mapResults, assembled from
// the journal and the per-occurrence result store, never from goroutine
// completion order.

// MapItemResult remains the runner-facing name of the shared join contract.
type MapItemResult = runsemantics.MapItemResult

// mapVisit is the frozen roster one map execution expands.
type mapVisit struct {
	def    apiv1.Map
	source journal.FanOutSourceRef
	items  []journal.MapItem
	// seq is the map.started sequence: the visit key of the per-occurrence
	// result store. Zero until the event is appended.
	seq uint64
	// join holds the assembled join inputs once the visit has finished and
	// its join will run.
	join *mapJoin
}

// mapJoin is the assembled input a map join receives.
type mapJoin struct {
	completeness []journal.BranchOutcome
	results      []MapItemResult
	// rosterIntegrity grades mapCompleteness, which carries only roster
	// data; resultsIntegrity is the weakest of the roster and every item
	// result, and grades mapResults.
	rosterIntegrity  apiv1.Integrity
	resultsIntegrity apiv1.Integrity
}

// rosterIntegrity is the integrity every bound item value inherits (§4.3): the
// frozen source artifact's grade, reconstructed from the journaled source
// reference on resume and never defaulted to trusted static input. An
// unrecorded or unknown grade stays as it is, so admission and result grading
// fail closed on it.
func (v *mapVisit) rosterIntegrity() apiv1.Integrity {
	return v.source.Integrity
}

// newMapExec builds the live state of one map visit from its frozen roster
// and the sequence of the map.started that recorded it (zero before the event
// is appended). Branch ids follow roster order from 1, so the same roster
// yields the same (branch, identity, digest) triples on every runner.
func newMapExec(m apiv1.Map, roster journal.MapRoster, seq uint64) *parallelExec {
	items := append([]journal.MapItem(nil), roster.Items...)
	par := newParallelExec(mapParallelSpec(m, items))
	for i, item := range items {
		par.branches[i].itemIdentity, par.branches[i].itemDigest = item.Identity, item.Digest
	}
	par.mapVisit = &mapVisit{def: m, source: roster.Source, items: items, seq: seq}
	return par
}

// isMap reports whether this execution expands a map visit.
func (p *parallelExec) isMap() bool { return p != nil && p.mapVisit != nil }

// mapItemBinding is the typed per-item invocation input of one map branch
// (§6.1): the declared bindings resolved against the branch's frozen item.
type mapItemBinding struct {
	bindings  map[string]apiv1.MapBinding
	item      journal.MapItem
	integrity apiv1.Integrity
}

// itemBinding returns the binding of map branch branchID, or nil for a static
// parallel or an out-of-range branch.
func (p *parallelExec) itemBinding(branchID int) *mapItemBinding {
	if !p.isMap() || branchID < 1 || branchID > len(p.mapVisit.items) {
		return nil
	}
	return &mapItemBinding{
		bindings:  p.mapVisit.def.Bindings,
		item:      p.mapVisit.items[branchID-1],
		integrity: p.mapVisit.rosterIntegrity(),
	}
}

// apply binds every declared value into the invocation inputs. Item is the
// frozen canonical object, decoded with exact numbers; identity and digest
// are strings. The compiler already refused a binding that collides with a
// static input or names anything outside the closed set.
func (b *mapItemBinding) apply(inputs map[string]any) error {
	if b == nil {
		return nil
	}
	for name, binding := range b.bindings {
		switch binding {
		case apiv1.MapBindingItem:
			decoder := json.NewDecoder(bytes.NewReader(b.item.Item))
			decoder.UseNumber()
			var value map[string]any
			if err := decoder.Decode(&value); err != nil {
				return fmt.Errorf("decode frozen map item %q: %w", b.item.Identity, err)
			}
			inputs[name] = value
		case apiv1.MapBindingIdentity:
			inputs[name] = b.item.Identity
		case apiv1.MapBindingDigest:
			inputs[name] = b.item.Digest
		default:
			return fmt.Errorf("map binding %q has unknown value %q", name, binding)
		}
	}
	return nil
}

// addGrades grades every bound input with the roster's integrity, exactly as
// an inputsFrom value is graded with its producer's (§4.3).
func (b *mapItemBinding) addGrades(grades map[string]apiv1.Integrity) map[string]apiv1.Integrity {
	if b == nil || len(b.bindings) == 0 {
		return grades
	}
	if grades == nil {
		grades = make(map[string]apiv1.Integrity, len(b.bindings))
	}
	for name := range b.bindings {
		grades[name] = b.integrity
	}
	return grades
}

// taskInputGrades is the provenance of every value a task attempt is admitted
// with beyond its context pointers: inputsFrom values, map item bindings, and
// a map join's assembled inputs.
func taskInputGrades(tf taskFrame) map[string]apiv1.Integrity {
	grades := resolvedInputGrades(tf.t, tf.in.Machine, tf.upstreamResult, tf.completed, tf.fanIn)
	grades = tf.mapItem.addGrades(grades)
	if join := mapJoinFor(tf.fanIn, tf.t.Name); join != nil {
		if grades == nil {
			grades = make(map[string]apiv1.Integrity, 2)
		}
		grades[runsemantics.MapCompletenessInput] = join.rosterIntegrity
		grades[runsemantics.MapResultsInput] = join.resultsIntegrity
	}
	return grades
}

// mapJoinFor returns the assembled map join inputs when state is the join of
// the map visit fanIn just finished.
func mapJoinFor(fanIn *parallelExec, state string) *mapJoin {
	if !fanIn.isMap() || fanIn.spec.Join != state {
		return nil
	}
	return fanIn.mapVisit.join
}

// joinInputs adds the fan-in's join inputs to a join's invocation inputs: a
// map's mapCompleteness and mapResults, or a parallel's branchCompleteness.
func joinInputs(fanIn *parallelExec, state string, inputs map[string]any) {
	if fanIn == nil || fanIn.spec.Join != state {
		return
	}
	if fanIn.isMap() {
		join := fanIn.mapVisit.join
		if join == nil {
			join = &mapJoin{completeness: fanIn.completeness(), results: []MapItemResult{}}
		}
		inputs[runsemantics.MapCompletenessInput] = join.completeness
		inputs[runsemantics.MapResultsInput] = join.results
		return
	}
	inputs[runsemantics.BranchCompletenessInput] = fanIn.completeness()
}

// assembleMapJoin rebuilds the per-occurrence result store from the journal
// and assembles the visit's join inputs in identity order (§9). It reads the
// journal rather than anything a branch worker held, so a live run and a
// resumed one assemble the same inputs.
func assembleMapJoin(runID string, events []journal.Event, machine *workflow.Machine, par *parallelExec) *fanOutResults {
	store := reconstructFanOutResults(runID, events, machine)
	if !par.isMap() {
		return store
	}
	visit := par.mapVisit
	if visit.seq == 0 {
		visit.seq = latestMapStartSeq(events, visit.def.Name)
	}
	completeness := par.completeness()
	join := &mapJoin{
		completeness:    completeness,
		results:         make([]MapItemResult, 0, len(completeness)),
		rosterIntegrity: visit.rosterIntegrity(),
	}
	grades := []apiv1.Integrity{join.rosterIntegrity}
	for _, outcome := range completeness {
		entry := MapItemResult{Identity: outcome.MapItemIdentity, Digest: outcome.MapItemDigest, Branch: outcome.Branch, Status: outcome.Status}
		if result, ok := store.branchResult(visit.def.Name, visit.seq, outcome.Branch, visit.def.Task); ok {
			entry.Outputs, entry.Integrity, entry.Pointers = result.Outputs, result.Integrity, result.Pointers
			for i := range entry.Pointers {
				entry.Pointers[i].Branch, entry.Pointers[i].BranchName = outcome.Branch, outcome.Name
			}
			grades = append(grades, result.Integrity)
		}
		join.results = append(join.results, entry)
	}
	join.resultsIntegrity = apiv1.WeakestIntegrity(grades...)
	visit.join = join
	return store
}

func latestMapStartSeq(events []journal.Event, name string) uint64 {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventMapStarted && events[i].Parallel == name {
			return events[i].Seq
		}
	}
	return 0
}

// mapJoinSubjectEvents narrows the events a resumed map join reads its
// subject (the last finished stage) from to those before the visit's
// map.started, matching the live run, which never makes an item's result the
// join's subject. Any other fan-in keeps every event.
func mapJoinSubjectEvents(events []journal.Event, fanIn *parallelExec) []journal.Event {
	if !fanIn.isMap() {
		return events
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventMapStarted && events[i].Parallel == fanIn.spec.Name {
			return events[:i]
		}
	}
	return events
}

// isMappedTask reports whether stage is a map's mapped task. Its results are
// per item and live only in the per-occurrence store: the stage-name store
// would keep one arbitrary item, so it never records them (§6.3).
func isMappedTask(machine *workflow.Machine, stage string) bool {
	if machine == nil {
		return false
	}
	for _, m := range machine.Def.Spec.Maps {
		if m.Task == stage {
			return true
		}
	}
	return false
}

// enterFanOut enters the parallel or map ws.state names. entered is false
// when the state is neither; done reports that walk must return result.
func (r *Runner) enterFanOut(ctx context.Context, ws *walkState, runConcurrent concurrentParallelRunner) (result Result, entered, done bool, err error) {
	if p, ok := ws.in.Machine.Parallel(ws.state); ok {
		result, done, err = r.enterParallel(ctx, ws, p, runConcurrent)
		return result, true, done, err
	}
	if m, ok := ws.in.Machine.Map(ws.state); ok {
		result, done, err = r.enterMap(ctx, ws, m, runConcurrent)
		return result, true, done, err
	}
	return Result{}, false, false, nil
}

// enterMap freezes map m's roster and runs the visit through the bounded
// parallel dispatcher. A source that cannot be frozen fails the map state
// along its onFailure route, or @abort, without writing map.started (§4.2).
func (r *Runner) enterMap(ctx context.Context, ws *walkState, m apiv1.Map, runConcurrent concurrentParallelRunner) (Result, bool, error) {
	spec := mapParallelSpec(m, nil)
	if err := supportedFailurePolicy(m.FailurePolicy); err != nil {
		res, failErr := r.failTerminal(ctx, ws.in.RunID, ws.jr, ws.in.RepoRef, ws.state, ws.steps,
			fmt.Errorf("runner: map %q: %w", m.Name, err))
		return res, true, failErr
	}
	reader, err := journal.OpenReadOnly(ws.jr.Dir())
	if err != nil {
		res, failErr := r.failTerminal(ctx, ws.in.RunID, ws.jr, ws.in.RepoRef, ws.state, ws.steps, err)
		return res, true, failErr
	}
	events, err := reader.Events()
	if err != nil {
		res, failErr := r.failTerminal(ctx, ws.in.RunID, ws.jr, ws.in.RepoRef, ws.state, ws.steps, err)
		return res, true, failErr
	}
	started, err := freezeMapRoster(ctx, ws.jr.Dir(), events, m)
	if err != nil {
		var invalid *fanout.SourceError
		if !errors.As(err, &invalid) {
			res, failErr := r.failTerminal(ctx, ws.in.RunID, ws.jr, ws.in.RepoRef, ws.state, ws.steps, err)
			return res, true, failErr
		}
		next, res, done, routeErr := r.routeFanOutSourceFailure(ws, spec,
			&FanOutSourceError{State: m.Name, Source: m.Source, Err: err})
		if done || routeErr != nil {
			return res, true, routeErr
		}
		ws.state = next
		return Result{}, false, nil
	}
	spec = mapParallelSpec(m, started.MapRoster.Items)
	return runConcurrent(parallelVisit{spec: spec, mapDef: &m, mapStart: &started})
}
