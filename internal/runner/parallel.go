package runner

import (
	"fmt"
	"sync"
	"time"

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
	timedOut  bool
	started   bool
	settled   bool
	// startedAt is when the branch's first stage began, reconstructed from
	// its EventBranchStarted on resume rather than reset to "now" — a
	// resumed branch gets its REMAINING budget, not a fresh one. Zero when
	// the branch has not started or the parallel declares no
	// branchTimeoutSeconds.
	startedAt time.Time
}

// deadline returns when this branch's declared branchTimeoutSeconds budget
// expires, or the zero Time if unbounded (seconds <= 0) or not yet started.
func (b *branchState) deadline(seconds int32) time.Time {
	if seconds <= 0 || b.startedAt.IsZero() {
		return time.Time{}
	}
	return b.startedAt.Add(time.Duration(seconds) * time.Second)
}

// parallelExec is the runner's live state for one parallel. The default
// maxConcurrentBranches=1 path advances it sequentially; wider read-only
// parallels update it from bounded branch workers.
type parallelExec struct {
	mu       sync.Mutex
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
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentLocked()
}

func (p *parallelExec) currentLocked() *branchState {
	if p.active < 0 || p.active >= len(p.branches) {
		return nil
	}
	return p.branches[p.active]
}

func (p *parallelExec) branch(name string) *branchState {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, branch := range p.branches {
		if branch.name == name {
			return branch
		}
	}
	return nil
}

func (p *parallelExec) currentPointers(root []apiv1.ContextPointer) []apiv1.ContextPointer {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.currentLocked()
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
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.currentLocked()
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if current := p.currentLocked(); current != nil {
		current.failed = true
	}
}

// markCurrentTimedOut records that the active branch exceeded
// branchTimeoutSeconds. Checked at the next stage boundary (never
// mid-stage — see BranchTimeoutSeconds' doc comment), so it is a plain flag
// like markCurrentFailed, not a preemptive cancellation.
func (p *parallelExec) markCurrentTimedOut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if current := p.currentLocked(); current != nil {
		current.timedOut = true
	}
}

// currentDeadline returns the active branch's branchTimeoutSeconds deadline,
// or the zero Time if unbounded or no branch is active.
func (p *parallelExec) currentDeadline() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.currentLocked()
	if current == nil {
		return time.Time{}
	}
	return current.deadline(p.spec.BranchTimeoutSeconds)
}

func (p *parallelExec) markCurrentNoOutput() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if current := p.currentLocked(); current != nil {
		current.noOutput = true
	}
}

func (p *parallelExec) currentStatus() journal.BranchStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.currentLocked()
	if current == nil {
		return journal.BranchCancelled
	}
	return branchStatus(current)
}

func (p *parallelExec) joinPointers(root []apiv1.ContextPointer) []apiv1.ContextPointer {
	p.mu.Lock()
	defer p.mu.Unlock()
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

func branchStatus(branch *branchState) journal.BranchStatus {
	if branch.settled && branch.status != "" {
		return branch.status
	}
	if branch.timedOut {
		return journal.BranchTimedOut
	}
	if branch.failed {
		return journal.BranchFailed
	}
	if branch.noOutput || !branch.produced {
		return journal.BranchNoOutput
	}
	return journal.BranchSucceeded
}

func (p *parallelExec) branchSnapshot(index int) branchState {
	p.mu.Lock()
	defer p.mu.Unlock()
	branch := *p.branches[index]
	branch.pointers = append([]apiv1.ContextPointer(nil), branch.pointers...)
	return branch
}

func (p *parallelExec) startBranch(index int) (branchState, []journal.BranchCursor) {
	p.mu.Lock()
	defer p.mu.Unlock()
	branch := p.branches[index]
	branch.started = true
	if branch.startedAt.IsZero() {
		branch.startedAt = time.Now()
	}
	snapshot := *branch
	snapshot.pointers = append([]apiv1.ContextPointer(nil), branch.pointers...)
	return snapshot, p.cursorsLocked()
}

func (p *parallelExec) moveBranch(branchID int, state string) []journal.BranchCursor {
	p.mu.Lock()
	defer p.mu.Unlock()
	if branchID > 0 && branchID <= len(p.branches) && !p.branches[branchID-1].settled {
		p.branches[branchID-1].machine = state
	}
	return p.cursorsLocked()
}

func (p *parallelExec) settleBranch(branchID int, status journal.BranchStatus, artifacts int, pointers []apiv1.ContextPointer, produced, failed, noOutput bool) []journal.BranchCursor {
	p.mu.Lock()
	defer p.mu.Unlock()
	if branchID <= 0 || branchID > len(p.branches) {
		return p.cursorsLocked()
	}
	branch := p.branches[branchID-1]
	branch.machine = ""
	branch.status = status
	branch.artifacts = artifacts
	branch.pointers = append([]apiv1.ContextPointer(nil), pointers...)
	branch.produced = produced
	branch.failed = failed
	branch.noOutput = noOutput
	branch.settled = true
	return p.cursorsLocked()
}

// advance settles the active branch and moves to the next unsettled one.
// It reports whether another branch is now running.
func (p *parallelExec) advance(status journal.BranchStatus) (*branchState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur := p.currentLocked(); cur != nil {
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
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cursorsLocked()
}

func (p *parallelExec) cursorsLocked() []journal.BranchCursor {
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
	p.mu.Lock()
	defer p.mu.Unlock()
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
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.anyFailedLocked()
}

func (p *parallelExec) anyFailedLocked() bool {
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
	p.mu.Lock()
	defer p.mu.Unlock()
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.anyFailedLocked() {
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
