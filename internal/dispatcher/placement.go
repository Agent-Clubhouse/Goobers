package dispatcher

import (
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// placement.go carries the pinned-placement contract: the run-start solve's
// per-stage answer, snapshotted into the run's input (WF-016) and read as
// pure data by whoever drives the run.
//
// It lives HERE rather than in internal/engine (decision 003 ruling 2, plan
// step 3) because the engine is no longer its only consumer. The daemon's
// runner pins the same list into run.yaml and hands one entry to the engine's
// dispatch activity over Temporal; internal/engine keeps a type alias so
// every existing reference — and, critically, every recorded workflow history
// and persisted RunInput — stays byte-identical. internal/engine already
// imports this package for RunnerSpec/Attempt/Report, so the type sits beside
// the eligible-runner set it carries and the dependency arrow does not
// reverse.

// PinnedPlacement is one task's resolved execution placement, pinned into the
// run's input at run start. The zero value never occurs in a pinned list; an
// absent entry (or an empty list — every zero-declaration and local-mode
// instance) leaves the stage on the legacy self path, byte for byte.
//
// The JSON tags are a WIRE CONTRACT: they appear in recorded Temporal
// histories and in persisted run inputs, so an existing history must decode
// into this type unchanged.
type PinnedPlacement struct {
	// Stage is the task name the placement binds to.
	Stage string `json:"stage"`
	// Self marks a placement that resolved to the daemon/worker host: the
	// stage executes through the existing InvokeGoober / RunDeterministic
	// arms, exactly as before this field existed. The dispatcher models the
	// same outcome as Local=true/no-pod, so self stays a first-class
	// placement rather than a special case.
	Self bool `json:"self,omitempty"`
	// Queue is the per-(gaggle × runner-type) task queue the dispatch
	// activity is routed onto (QueueName of the runner SelectRunner picks —
	// D9). Empty inherits the workflow's queue.
	Queue string `json:"queue,omitempty"`
	// Eligible is the solver's eligible runner set for this stage, in
	// inventory order — dispatch consumes eligibility, it never re-derives it
	// (goobernetes-dispatcher.md §2).
	Eligible []RunnerSpec `json:"eligible,omitempty"`
	// LedgerTouching, CPU, Memory, Disk, and Restrictions are the Attempt
	// requirement facts, carried from the run-start solve
	// (runnersolve.StageRequirement) because the workflow cannot recompute
	// them mid-run.
	LedgerTouching bool     `json:"ledgerTouching,omitempty"`
	CPU            string   `json:"cpu,omitempty"`
	Memory         string   `json:"memory,omitempty"`
	Disk           string   `json:"disk,omitempty"`
	Restrictions   []string `json:"restrictions,omitempty"`
}

// Journal is the run.yaml spelling of this placement (journal.PinnedPlacement,
// placementpin.go): what the daemon's runner pins at Start and hands back to
// the seam at dispatch. Field for field; the journal cannot import this
// package, so the conversion lives on this side.
func (p PinnedPlacement) Journal() journal.PinnedPlacement {
	var eligible []journal.PinnedRunner
	for _, e := range p.Eligible {
		eligible = append(eligible, journal.PinnedRunner{
			Name: e.Name, OS: e.OS, HostKind: string(e.HostKind), Host: e.Host,
			CPU: e.CPU, Memory: e.Memory, Disk: e.Disk, Restrictions: e.Restrictions,
		})
	}
	return journal.PinnedPlacement{
		Stage: p.Stage, Self: p.Self, Queue: p.Queue, Eligible: eligible,
		LedgerTouching: p.LedgerTouching, CPU: p.CPU, Memory: p.Memory, Disk: p.Disk,
		Restrictions: p.Restrictions,
	}
}

// PinnedPlacementFromJournal is Journal's inverse: the seam's daemon-side
// implementation rebuilds the dispatch wire type from what run.yaml carries.
func PinnedPlacementFromJournal(p journal.PinnedPlacement) PinnedPlacement {
	var eligible []RunnerSpec
	for _, e := range p.Eligible {
		eligible = append(eligible, RunnerSpec{
			Name: e.Name, OS: e.OS, HostKind: instance.RunnerHostKind(e.HostKind), Host: e.Host,
			CPU: e.CPU, Memory: e.Memory, Disk: e.Disk, Restrictions: e.Restrictions,
		})
	}
	return PinnedPlacement{
		Stage: p.Stage, Self: p.Self, Queue: p.Queue, Eligible: eligible,
		LedgerTouching: p.LedgerTouching, CPU: p.CPU, Memory: p.Memory, Disk: p.Disk,
		Restrictions: p.Restrictions,
	}
}

// PinnedPlacementsJournal converts a whole pinned list; nil in, nil out, so an
// unplaced run's run.yaml keeps its exact bytes.
func PinnedPlacementsJournal(placements []PinnedPlacement) []journal.PinnedPlacement {
	if len(placements) == 0 {
		return nil
	}
	out := make([]journal.PinnedPlacement, 0, len(placements))
	for _, p := range placements {
		out = append(out, p.Journal())
	}
	return out
}
