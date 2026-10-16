package runner

import (
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// fanOutResultKey identifies one mapped stage result (design
// plan-driven-dynamic-fan-out.md §6.3): the fan-out state, the numeric branch,
// the stage, and the journal stage occurrence (#6881). A map runs one task name
// in every branch and again on every visit, so any key without the branch and
// the occurrence would let one item's result overwrite another's.
type fanOutResultKey struct {
	FanOut     string
	Branch     int
	Stage      string
	Occurrence string
}

// fanOutResult is one recorded mapped stage result.
type fanOutResult struct {
	// Visit is the sequence of the fan-out visit's start event (map.started),
	// so join assembly reads only the current visit's items.
	Visit     uint64
	Outputs   map[string]any
	Integrity apiv1.Integrity
	Pointers  []apiv1.ContextPointer
	order     uint64
}

// fanOutResults is the branch-aware result store for mapped execution. Join
// assembly, rerun, integrity propagation and resume read mapped results from
// here, never from stageOutputs, which is keyed by stage name alone. Linear
// execution and static parallel branches keep using stageOutputs unchanged.
// It is safe for concurrent branch workers.
type fanOutResults struct {
	mu      sync.Mutex
	results map[fanOutResultKey]fanOutResult
	next    uint64
}

func newFanOutResults() *fanOutResults {
	return &fanOutResults{results: map[fanOutResultKey]fanOutResult{}}
}

// record stores one finished mapped stage under its occurrence key, with the
// same record-or-forget rule stageOutputs.RecordCompleted applies: a
// tolerated failure (continueOnError) is forgotten, anything else is recorded
// with the provenance grade of the content that produced it. Outputs and
// pointers are copied.
func (s *fanOutResults) record(key fanOutResultKey, visit uint64, task apiv1.Task, result apiv1.ResultEnvelope, pointers []apiv1.ContextPointer) {
	if s == nil || key.Stage == "" || key.Branch < 1 || key.Occurrence == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if result.Status == apiv1.ResultFailure && task.ContinueOnError {
		delete(s.results, key)
		return
	}
	outputs := make(map[string]any, len(result.Outputs))
	for k, v := range result.Outputs {
		outputs[k] = v
	}
	s.next++
	s.results[key] = fanOutResult{
		Visit: visit, Outputs: outputs, Integrity: result.Integrity,
		Pointers: append([]apiv1.ContextPointer(nil), pointers...), order: s.next,
	}
}

// branchResult returns the newest result stage recorded on one branch of one
// fan-out visit: the per-item value a map join assembles. A retry stays in its
// occurrence and replaces its own entry, so "newest" only matters if a branch
// ever ran the stage in more than one occurrence within a visit.
func (s *fanOutResults) branchResult(fanOut string, visit uint64, branch int, stage string) (fanOutResult, bool) {
	if s == nil {
		return fanOutResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var newest fanOutResult
	found := false
	for key, got := range s.results {
		if key.FanOut != fanOut || key.Branch != branch || key.Stage != stage || got.Visit != visit {
			continue
		}
		if !found || got.order > newest.order {
			newest, found = got, true
		}
	}
	return copyFanOutResult(newest), found
}

func copyFanOutResult(in fanOutResult) fanOutResult {
	out := in
	if in.Outputs != nil {
		out.Outputs = make(map[string]any, len(in.Outputs))
		for k, v := range in.Outputs {
			out.Outputs[k] = v
		}
	}
	out.Pointers = append([]apiv1.ContextPointer(nil), in.Pointers...)
	return out
}

type branchStage struct {
	branch int
	stage  string
}

// reconstructFanOutResults rebuilds the mapped result store from the journal
// so a resumed run reads the same per-occurrence results a live run recorded.
// Only stages finished inside a map visit's item branches are mapped results;
// each is keyed by the occurrence of the stage.started that opened it.
func reconstructFanOutResults(runID string, events []journal.Event, machine *workflow.Machine) *fanOutResults {
	store := newFanOutResults()
	var open string
	var visit uint64
	occurrences := map[branchStage]string{}
	for _, e := range events {
		switch e.Type {
		case journal.EventMapStarted:
			open, visit, occurrences = e.Parallel, e.Seq, map[branchStage]string{}
		case journal.EventMapFinished:
			if e.Parallel == open {
				open = ""
			}
		case journal.EventRunResumed:
			if e.Target != workflow.TargetJoin {
				open = ""
			}
		case journal.EventStageStarted:
			if open != "" && e.Branch > 0 {
				key := branchStage{e.Branch, e.Stage}
				occurrences[key] = journal.StageOccurrence(runID, e, occurrences[key])
			}
		case journal.EventStageFinished:
			if open == "" || e.Branch < 1 || isInterruptedAttemptMarker(e) {
				continue
			}
			occurrence := occurrences[branchStage{e.Branch, e.Stage}]
			var task apiv1.Task
			if machine != nil {
				task, _ = machine.Task(e.Stage)
			}
			store.record(
				fanOutResultKey{FanOut: open, Branch: e.Branch, Stage: e.Stage, Occurrence: occurrence}, visit, task,
				apiv1.ResultEnvelope{Status: apiv1.ResultStatus(e.Status), Outputs: e.Outputs, Integrity: e.Integrity},
				contextPointersFor(e.Stage, artifactPointersFrom(e.Artifacts)),
			)
		}
	}
	return store
}
