package journalpg

import (
	"encoding/json"
	"slices"

	"github.com/goobers/goobers/internal/journal"
)

// replayState is the working set needed to extend stateForEvents, not an event
// cache. Build it before the fence; apply only the concurrent tail and this
// batch under the lock. Prefix parity is tested against the shared full replay.
type replayState struct {
	state        journal.State
	cursor       string
	openParallel string
	openFinish   journal.EventType
	branches     map[string]map[int]journal.BranchCursor
	gates        map[int]*gateReplay
}

type gateMark struct {
	seq      uint64
	executed bool
}
type gateBarrier struct {
	seq  uint64
	gate string
}
type gateReplay struct {
	markers          map[string]gateMark
	newest, previous gateBarrier
}

func newReplayState(runID string, events []journal.Event) *replayState {
	r := &replayState{state: journal.State{Schema: journal.StateSchema, RunID: runID, Phase: journal.PhaseRunning}, branches: map[string]map[int]journal.BranchCursor{}, gates: map[int]*gateReplay{}}
	r.fold(events)
	return r
}
func (r *replayState) fold(events []journal.Event) {
	for _, event := range events {
		if event.Seq <= r.state.LastSeq {
			continue
		} // emit-key replay is not a new event
		r.applyLifecycle(event)
		r.applyBranches(event)
		r.trackGate(event)
		r.state.LastSeq = event.Seq
		r.state.UpdatedAt = event.Time
	}
}
func (r *replayState) applyLifecycle(event journal.Event) {
	switch event.Type {
	case journal.EventRunResumed, journal.EventGateOverridden:
		r.cursor = event.Target
		r.state.Phase, r.state.Reason = journal.PhaseRunning, ""
	case journal.EventStageRerunRequested:
		r.cursor = event.Stage
		r.state.Phase, r.state.Reason = journal.PhaseRunning, ""
	case journal.EventRunFinished:
		r.cursor = ""
		r.state.Phase = journal.PhaseFromEvents([]journal.Event{event})
		r.state.Reason = ""
		if event.Error != nil {
			r.state.Reason = event.Error.Message
		}
	case journal.EventGateEvaluated:
		if event.Target == journal.TargetAbort || event.Target == journal.TargetEscalate {
			r.state.Phase = journal.PhaseRunning
			if r.gateExecuted(event) {
				r.state.Phase = journal.PhaseFromEvents([]journal.Event{event})
			}
		}
	}
}

// Match terminalGateExecuted: the newest own-gate marker or a newer
// non-start/pause event belonging to another gate decides execution. Two
// distinct-gate barriers suffice to answer "newest except this gate" in O(1).
func (r *replayState) gateExecuted(event journal.Event) bool {
	g := r.gates[event.Branch]
	if g == nil {
		return true
	}
	mark, ok := g.markers[event.Gate]
	if !ok {
		return true
	}
	barrier := g.newest
	if barrier.gate == event.Gate {
		barrier = g.previous
	}
	return barrier.seq > mark.seq || mark.executed
}
func (r *replayState) trackGate(event journal.Event) {
	g := r.gates[event.Branch]
	if g == nil {
		g = &gateReplay{markers: map[string]gateMark{}}
		r.gates[event.Branch] = g
	}
	switch event.Type {
	case journal.EventGatePaused, journal.EventGateStarted, journal.EventGateEvaluated:
		g.markers[event.Gate] = gateMark{seq: event.Seq, executed: event.Type != journal.EventGatePaused}
	}
	if event.Type != journal.EventGatePaused && event.Type != journal.EventGateStarted {
		if g.newest.gate != event.Gate {
			g.previous = g.newest
		}
		g.newest = gateBarrier{seq: event.Seq, gate: event.Gate}
	}
}
func (r *replayState) applyBranches(event journal.Event) {
	switch event.Type {
	case journal.EventParallelStarted:
		r.openParallel, r.openFinish = event.Parallel, journal.EventParallelFinished
	case journal.EventMapStarted:
		r.openParallel, r.openFinish = event.Parallel, journal.EventMapFinished
	case journal.EventParallelFinished, journal.EventMapFinished:
		if event.Type == r.openFinish && event.Parallel == r.openParallel {
			r.openParallel = ""
		}
	case journal.EventBranchStarted:
		cursors := r.branches[event.Parallel]
		if cursors == nil {
			cursors = map[int]journal.BranchCursor{}
			r.branches[event.Parallel] = cursors
		}
		cursors[event.Branch] = journal.BranchCursor{Branch: event.Branch, Name: event.BranchName, Parallel: event.Parallel, ItemIdentity: event.MapItemIdentity, ItemDigest: event.MapItemDigest, MachineState: event.Stage}
	case journal.EventBranchFinished:
		cursors := r.branches[event.Parallel]
		if cursor, ok := cursors[event.Branch]; ok {
			cursor.Status, cursor.MachineState = event.BranchStatus, ""
			cursors[event.Branch] = cursor
		}
	case journal.EventStageStarted, journal.EventGateEvaluated:
		r.advanceBranch(event)
	}
}
func (r *replayState) advanceBranch(event journal.Event) {
	for parallel, cursors := range r.branches {
		if event.Branch == 0 && event.Parallel != parallel {
			continue
		}
		cursor, ok := cursors[event.Branch]
		if !ok || cursor.Status != "" {
			continue
		}
		if event.Stage != "" {
			cursor.MachineState = event.Stage
		} else if event.Gate != "" {
			cursor.MachineState = event.Gate
		}
		cursors[event.Branch] = cursor
	}
}
func (r *replayState) checkpoint() (*preparedCheckpoint, error) {
	state := r.state
	state.MachineState = r.cursor
	if state.Phase != journal.PhaseRunning {
		state.MachineState = ""
	}
	if r.openParallel != "" {
		cursors := r.branches[r.openParallel]
		ids := make([]int, 0, len(cursors))
		for id := range cursors {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			state.Branches = append(state.Branches, cursors[id])
		}
	}
	raw, err := json.Marshal(state)
	return &preparedCheckpoint{state: state, raw: raw}, err
}
