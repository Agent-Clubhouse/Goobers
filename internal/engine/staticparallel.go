package engine

import (
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runsemantics"
	wf "github.com/goobers/goobers/internal/workflow"
)

// staticWalkSession belongs to one versioned Run controller. Admission still
// refuses unqualified features. New histories retain each committed visit
// identity; legacy scheduler histories keep their single-visit refusal.
type staticWalkSession struct {
	currentVisits bool
	seen          map[string]bool
	join          string
	completeness  []journal.BranchOutcome
	parallel      string
	branches      map[string]runsemantics.BranchInputSource
	mapJoin       *staticMapJoin
}

type staticMapJoin struct {
	completeness     []journal.BranchOutcome
	results          []runsemantics.MapItemResult
	rosterIntegrity  apiv1.Integrity
	resultsIntegrity apiv1.Integrity
}

type staticBranchOutcome struct {
	index int
	frame *branchWalkFrame
	err   error
}

func (r *runJournal) fanOutVisit() uint64 {
	if r.branchWalk == nil {
		return 0
	}
	return r.branchWalk.started.Seq
}

// Only the root coroutine owns this queue and settles outcomes. Branch bodies
// publish exactly once to a buffered workflow channel, including on cancel.
type staticParallelWalk struct {
	root         *runJournal
	in           RunInput
	machine      *wf.Machine
	spec         fanOutPlan
	started      livejournal.AppendReceipt
	seed         *branchWalkFrame
	outcomes     []*staticBranchOutcome
	results      workflow.Channel
	ctx, drain   workflow.Context
	cancel       workflow.CancelFunc
	next, active int
	stopped      bool
	err          error
}

func (r *runJournal) staticParallel(ctx workflow.Context, in RunInput, m *wf.Machine, state string, seed *branchWalkFrame) (string, bool, error) {
	if r.staticWalk == nil {
		return "", false, nil
	}
	if mapped, ok := m.Map(state); ok {
		return r.staticMap(ctx, in, m, mapped, seed)
	}
	p, ok := m.Parallel(state)
	if !ok {
		return "", false, nil
	}
	repeatable := workflow.GetVersion(ctx, staticParallelVisitsVersion, workflow.DefaultVersion, 1) != workflow.DefaultVersion
	if r.staticWalk.seen[state] && !repeatable {
		return "", true, fmt.Errorf("engine: repeated static parallel visit %q is not supported", state)
	}
	if r.owner == nil || r.stepBudget == nil {
		return "", true, fmt.Errorf("engine: static parallel requires commit owner and shared budget")
	}
	if p.Selection != nil &&
		workflow.GetVersion(ctx, staticSelectionVersion, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return "", true, fmt.Errorf("engine: selective parallel execution is refused")
	}
	branchInputs := workflow.GetVersion(ctx, staticBranchInputsVersion, workflow.DefaultVersion, 1) != workflow.DefaultVersion
	if !branchInputs {
		if err := refuseStaticJoinBindings(in.Spec, p.Name); err != nil {
			return "", true, err
		}
	}
	if r.staticWalk.seen == nil {
		r.staticWalk.seen = map[string]bool{}
	}
	r.staticWalk.join, r.staticWalk.completeness, r.staticWalk.mapJoin = "", nil, nil
	r.staticWalk.parallel, r.staticWalk.branches = "", nil
	r.staticWalk.seen[state] = true
	plan, err := r.selectiveFanOutPlan(ctx, in, m, p)
	if err != nil {
		r.append(ctx, journal.Event{
			Type: journal.EventError, Parallel: p.Name,
			Error: journal.ErrorDetailFor(runsemantics.FanOutSourceInvalidCode, err),
		})
		if emitErr := r.emitPending(ctx); emitErr != nil {
			return "", true, emitErr
		}
		target := p.OnFailure
		if target == "" {
			target = wf.TargetAbort
		}
		return target, true, nil
	}
	pwalk, result, err := r.fanOut(ctx, in, m, plan, seed)
	if err != nil {
		return "", true, err
	}
	r.staticWalk.join, r.staticWalk.completeness = p.Join, pwalk.completeness()
	if branchInputs {
		r.staticWalk.parallel = p.Name
		r.staticWalk.branches = pwalk.joinSources()
	}
	r.staticWalk.currentVisits = repeatable
	if repeatable {
		pwalk.mergeVisit()
	} else {
		pwalk.merge()
	}
	return result.Target, true, nil
}

func (p *staticParallelWalk) run(ctx workflow.Context) {
	limit := min(max(1, int(p.spec.MaxConcurrentBranches)), len(p.spec.Branches))
	for p.active > 0 || p.next < len(p.spec.Branches) {
		p.fail(ctx.Err())
		if p.next < len(p.spec.Branches) && p.outcomes[p.next] != nil {
			p.next++
			continue
		}
		var out staticBranchOutcome
		if p.results.ReceiveAsync(&out) {
			p.accept(out)
			continue
		}
		if p.stopped {
			p.cancelQueued()
		} else if p.next < len(p.spec.Branches) && p.active < limit {
			p.launch()
			continue
		}
		if p.active > 0 {
			p.results.Receive(p.drain, &out)
			p.accept(out)
		}
	}
	p.fail(ctx.Err())
}

func (p *staticParallelWalk) launch() {
	i := p.next
	branch := p.spec.Branches[i]
	p.root.append(p.ctx, journal.Event{Type: journal.EventBranchStarted, Branch: i + 1, Parallel: p.spec.Name, BranchName: branch.Name, Stage: branch.Start, MapItemIdentity: branch.ItemIdentity, MapItemDigest: branch.ItemDigest})
	if err := p.root.emitPending(p.ctx); err != nil {
		p.fail(err)
		return
	}
	frame := *p.seed // walk clones every mutable seed collection before use.
	frame.start = branch.Start
	frame.bind = branch.Bind
	frame.started = p.started
	frame.results = nil
	frame.visited = nil
	if p.spec.BranchTimeoutSeconds > 0 {
		frame.deadline = workflow.Now(p.ctx).Add(time.Duration(p.spec.BranchTimeoutSeconds) * time.Second)
	}
	rec := &runJournal{owner: p.root.owner, branch: i + 1, branchWalk: &frame, stepBudget: p.root.stepBudget,
		taskDispatches: p.root.taskDispatches, machine: p.machine, live: true, proj: p.root.proj, branchRef: p.root.branchRef}
	rec.proj.Ops = nil
	p.next++
	p.active++
	workflow.Go(p.ctx, func(ctx workflow.Context) {
		_, err := walk(ctx, p.in, p.machine, rec, nil)
		// Capacity covers every started body, so completion never blocks and
		// never disappears because its execution context was cancelled.
		p.results.SendAsync(staticBranchOutcome{index: i, frame: &frame, err: err})
	})
}

func (p *staticParallelWalk) accept(out staticBranchOutcome) {
	p.active--
	p.outcomes[out.index] = &out
	p.settle(out.index)
	status := out.frame.status
	// Boundary timeouts are branch outcomes. Cancellation caused by our own
	// fail-fast/terminal decision must not replace that decision with an error.
	if status != journal.BranchTimedOut && status != journal.BranchCancelled {
		p.fail(out.err)
	}
	failed := status == journal.BranchFailed || status == journal.BranchTimedOut
	if out.frame.terminalTarget != "" || (failed && p.spec.FailurePolicy == apiv1.BranchFailFast) {
		p.stop()
	}
}

func (p *staticParallelWalk) stop() {
	p.stopped = true
	p.cancel()
}

func (p *staticParallelWalk) fail(err error) {
	if err != nil && p.err == nil {
		p.err = err
		p.stop()
	}
}

func (p *staticParallelWalk) settle(i int) {
	p.root.append(p.drain, journal.Event{Type: journal.EventBranchFinished, Branch: i + 1, Parallel: p.spec.Name, BranchName: p.spec.Branches[i].Name, BranchStatus: p.outcomes[i].frame.status, Reason: p.spec.Branches[i].Reason, MapItemIdentity: p.spec.Branches[i].ItemIdentity, MapItemDigest: p.spec.Branches[i].ItemDigest})
	p.fail(p.root.emitPending(p.drain))
}

func (p *staticParallelWalk) cancelQueued() {
	for p.next < len(p.spec.Branches) {
		i := p.next
		p.next++
		if p.outcomes[i] != nil {
			continue
		}
		p.outcomes[i] = &staticBranchOutcome{index: i, frame: &branchWalkFrame{status: journal.BranchCancelled}}
		p.settle(i)
	}
}

func (p *staticParallelWalk) completeness() []journal.BranchOutcome {
	out := make([]journal.BranchOutcome, len(p.spec.Branches))
	for i, b := range p.spec.Branches {
		out[i] = journal.BranchOutcome{Branch: i + 1, Name: b.Name, Status: journal.BranchCancelled, Reason: b.Reason, MapItemIdentity: b.ItemIdentity, MapItemDigest: b.ItemDigest}
		if result := p.outcomes[i]; result != nil {
			out[i].Status = result.frame.status
			start := min(len(p.seed.pointers), len(result.frame.pointers))
			for _, pointer := range result.frame.pointers[start:] {
				if pointer.Artifact != nil {
					out[i].Artifacts++
				}
			}
		}
	}
	return out
}

func (p *staticParallelWalk) target() string {
	if p.err != nil {
		return ""
	}
	failed := false
	for _, out := range p.outcomes {
		if out.frame.terminalTarget != "" {
			return out.frame.terminalTarget
		}
		switch out.frame.status {
		case journal.BranchFailed, journal.BranchTimedOut, journal.BranchCancelled:
			failed = true
		}
	}
	if failed && p.spec.FailurePolicy != apiv1.BranchContinueOnError {
		return p.spec.OnFailure
	}
	return p.spec.Join
}

func (r *runJournal) staticJoinInputs(stage string, inputs map[string]any) {
	if s := r.staticWalk; s != nil && s.join == stage {
		if s.mapJoin != nil {
			inputs[runsemantics.MapCompletenessInput] = s.mapJoin.completeness
			inputs[runsemantics.MapResultsInput] = s.mapJoin.results
			return
		}
		inputs[runsemantics.BranchCompletenessInput] = s.completeness
	}
}

func (r *runJournal) staticJoinGrades(stage string, grades map[string]apiv1.Integrity) map[string]apiv1.Integrity {
	s := r.staticWalk
	if s == nil || s.join != stage || s.mapJoin == nil {
		return grades
	}
	if grades == nil {
		grades = make(map[string]apiv1.Integrity, 2)
	}
	grades[runsemantics.MapCompletenessInput] = s.mapJoin.rosterIntegrity
	grades[runsemantics.MapResultsInput] = s.mapJoin.resultsIntegrity
	return grades
}

// The old scalar resolver can treat dotted strings as literal output keys.
// Reject unsupported branch bindings explicitly rather than accidentally
// accepting a root output with the same spelling.
func refuseStaticJoinBindings(spec apiv1.WorkflowSpec, parallel string) error {
	for _, task := range spec.Tasks {
		for _, key := range sortedKeys(task.InputsFrom) {
			if strings.HasPrefix(task.InputsFrom[key], parallel+".") {
				return fmt.Errorf("engine: branch-qualified join input %q on %q is not supported", key, task.Name)
			}
		}
	}
	return nil
}
