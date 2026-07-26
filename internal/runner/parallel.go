package runner

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// BranchCompletenessInput is the join invocation input containing one terminal
// outcome for every declared branch, in declaration order.
const BranchCompletenessInput = "branchCompleteness"

// branchState is one branch's live execution state inside a parallel.
type branchState struct {
	id        int
	name      string
	start     string
	machine   string // the branch's current cursor; empty once settled
	status    journal.BranchStatus
	artifacts int
	pointers  []apiv1.ContextPointer
	produced  bool
	failed    bool
	noOutput  bool
	settled   bool
}

// parallelExec is the runner's live state for one parallel. It is deliberately
// a plain value threaded through walk rather than a goroutine pool: at
// maxConcurrentBranches=1 (the default) branches run SEQUENTIALLY, which is
// what makes this slice's journal trivially deterministic and sidesteps the
// worktree collision entirely (docs/design/static-fan-out-fan-in.md §6.3, §6.5).
type parallelExec struct {
	spec     apiv1.Parallel
	branches []*branchState
	active   int
}

// newParallelExec assigns branch ids by DECLARATION ORDER, from 1. Branch 0 is
// the run's root, so an id is deterministic and reproducible across runs and
// runners — which is what lets conformance compare per branch.
func newParallelExec(spec apiv1.Parallel) *parallelExec {
	branches := make([]*branchState, 0, len(spec.Branches))
	for i, branch := range spec.Branches {
		branches = append(branches, &branchState{
			id:      i + 1,
			name:    branch.Name,
			start:   branch.Start,
			machine: branch.Start,
		})
	}
	return &parallelExec{spec: spec, branches: branches, active: 0}
}

func (p *parallelExec) current() *branchState {
	if p.active < 0 || p.active >= len(p.branches) {
		return nil
	}
	return p.branches[p.active]
}

func (p *parallelExec) branch(name string) *branchState {
	for _, branch := range p.branches {
		if branch.name == name {
			return branch
		}
	}
	return nil
}

func (p *parallelExec) currentPointers(root []apiv1.ContextPointer) []apiv1.ContextPointer {
	current := p.current()
	size := len(root)
	if current != nil {
		size += len(current.pointers)
	}
	out := make([]apiv1.ContextPointer, 0, size)
	out = append(out, root...)
	if current != nil {
		out = append(out, current.pointers...)
	}
	return out
}

func (p *parallelExec) recordCurrent(outputs map[string]any, pointers []apiv1.ContextPointer) {
	current := p.current()
	if current == nil {
		return
	}
	current.pointers = append(current.pointers, pointers...)
	for _, pointer := range pointers {
		if pointer.Artifact != nil {
			current.artifacts++
		}
	}
	if len(outputs) > 0 || len(pointers) > 0 {
		current.produced = true
	}
}

func (p *parallelExec) recordCurrentPointer(pointer apiv1.ContextPointer) {
	p.recordCurrent(nil, []apiv1.ContextPointer{pointer})
}

func (p *parallelExec) markCurrentFailed() {
	if current := p.current(); current != nil {
		current.failed = true
	}
}

func (p *parallelExec) markCurrentNoOutput() {
	if current := p.current(); current != nil {
		current.noOutput = true
	}
}

func (p *parallelExec) currentStatus() journal.BranchStatus {
	current := p.current()
	if current == nil {
		return journal.BranchCancelled
	}
	if current.failed {
		return journal.BranchFailed
	}
	if current.noOutput {
		return journal.BranchNoOutput
	}
	if current.produced {
		return journal.BranchSucceeded
	}
	return journal.BranchNoOutput
}

func (p *parallelExec) joinPointers(root []apiv1.ContextPointer) []apiv1.ContextPointer {
	size := len(root)
	for _, branch := range p.branches {
		size += len(branch.pointers)
	}
	out := make([]apiv1.ContextPointer, 0, size)
	out = append(out, root...)
	for _, branch := range p.branches {
		for _, pointer := range branch.pointers {
			pointer.Branch = branch.id
			pointer.BranchName = branch.name
			out = append(out, pointer)
		}
	}
	return out
}

// advance settles the active branch and moves to the next unsettled one.
// It reports whether another branch is now running.
func (p *parallelExec) advance(status journal.BranchStatus) (*branchState, bool) {
	if cur := p.current(); cur != nil {
		cur.settled = true
		cur.status = status
		cur.machine = ""
	}
	for p.active++; p.active < len(p.branches); p.active++ {
		if !p.branches[p.active].settled {
			return p.branches[p.active], true
		}
	}
	return nil, false
}

// cursors projects the live branch positions for the run checkpoint.
func (p *parallelExec) cursors() []journal.BranchCursor {
	out := make([]journal.BranchCursor, 0, len(p.branches))
	for _, b := range p.branches {
		out = append(out, journal.BranchCursor{
			Branch:       b.id,
			Name:         b.name,
			Parallel:     p.spec.Name,
			MachineState: b.machine,
			Status:       b.status,
		})
	}
	return out
}

// completeness builds the branch completeness record in DECLARATION order —
// the order that assigns ids, and therefore normative. It is what makes "did
// every branch report?" answerable from the journal alone.
func (p *parallelExec) completeness() []journal.BranchOutcome {
	out := make([]journal.BranchOutcome, 0, len(p.branches))
	for _, b := range p.branches {
		status := b.status
		if status == "" {
			// A branch that never ran (cancelled before it started) still gets
			// an entry — the record covers every DECLARED branch, so a missing
			// branch is visible rather than absent.
			status = journal.BranchCancelled
		}
		out = append(out, journal.BranchOutcome{
			Branch: b.id, Name: b.name, Status: status, Artifacts: b.artifacts,
		})
	}
	return out
}

// supportedFailurePolicy reports whether the declared policy is one the runner
// implements. Every policy in the DSL executes as of FO-6; an unrecognised one
// still fails CLOSED rather than defaulting to permissive behaviour.
func supportedFailurePolicy(policy apiv1.BranchFailurePolicy) error {
	switch policy {
	case apiv1.BranchContinueOnError, apiv1.BranchFailFast, apiv1.BranchAllOrNothing:
		return nil
	default:
		return fmt.Errorf("unknown failurePolicy %q", policy)
	}
}

// anyFailed reports whether any branch settled unsuccessfully. no-output is a
// SUCCESSFUL settle: the branch ran and legitimately produced nothing, which is
// exactly what a research lens finding no issues looks like.
func (p *parallelExec) anyFailed() bool {
	for _, b := range p.branches {
		switch b.status {
		case journal.BranchFailed, journal.BranchTimedOut, journal.BranchCancelled:
			return true
		}
	}
	return false
}

// cancelRemaining settles every branch that has not run yet as cancelled. Under
// fail_fast this is what "cancel the siblings" means at maxConcurrentBranches=1:
// the remaining branches have not started, so cancelling them is abandoning
// them rather than interrupting anything mid-flight.
func (p *parallelExec) cancelRemaining() []*branchState {
	var cancelled []*branchState
	for _, b := range p.branches {
		if !b.settled {
			b.settled = true
			b.status = journal.BranchCancelled
			b.machine = ""
			cancelled = append(cancelled, b)
		}
	}
	p.active = len(p.branches)
	return cancelled
}

// route decides what happens once every branch has settled: whether the join
// runs, and which state the run continues at.
//
// When no branch failed, all three policies behave identically — the join runs.
// They differ only on failure, and fail_fast and all_or_nothing differ from
// each other only in how much work happened first.
func (p *parallelExec) route() (target string, runJoin bool) {
	if !p.anyFailed() {
		return p.spec.Join, true
	}
	switch p.spec.FailurePolicy {
	case apiv1.BranchContinueOnError:
		// The join owns the decision, via the completeness record.
		return p.spec.Join, true
	default:
		return p.spec.OnFailure, false
	}
}
