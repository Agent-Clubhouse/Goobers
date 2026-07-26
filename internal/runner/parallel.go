package runner

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// branchState is one branch's live execution state inside a parallel.
type branchState struct {
	id        int
	name      string
	start     string
	machine   string // the branch's current cursor; empty once settled
	status    journal.BranchStatus
	artifacts int
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

// supportedFailurePolicy reports whether this slice can execute the declared
// policy. FO-5 executes continue_on_error only; the cancelling policies need
// the cooperative-cancellation machinery that lands in FO-6 (#1564), so they
// fail CLOSED at runtime rather than silently behaving like continue_on_error
// — which would let a workflow believe it had fail-fast semantics it does not.
func supportedFailurePolicy(policy apiv1.BranchFailurePolicy) error {
	switch policy {
	case apiv1.BranchContinueOnError:
		return nil
	case apiv1.BranchFailFast, apiv1.BranchAllOrNothing:
		return fmt.Errorf("failurePolicy %q is declared but not yet implemented; only %q executes today",
			policy, apiv1.BranchContinueOnError)
	default:
		return fmt.Errorf("unknown failurePolicy %q", policy)
	}
}

// branchStatusFor maps a settling branch's last stage result to its terminal
// status. A branch-scoped no-work is a SUCCESSFUL settle that produced
// nothing — deliberately distinct from succeeded, which the join needs in
// order to tell "ran and found nothing" from "ran and produced findings".
func branchStatusFor(result apiv1.ResultEnvelope, artifacts int) journal.BranchStatus {
	switch result.Status {
	case apiv1.ResultNoWork:
		return journal.BranchNoOutput
	case apiv1.ResultFailure:
		return journal.BranchFailed
	}
	if artifacts == 0 && len(result.Outputs) == 0 {
		return journal.BranchNoOutput
	}
	return journal.BranchSucceeded
}
