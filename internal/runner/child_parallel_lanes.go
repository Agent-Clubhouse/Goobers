package runner

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// Declared goroutines are bounded by the DSL's 128 branches; lane ownership,
// rather than goroutine lifetime, bounds simultaneous executable branch work.
// A parked branch owns no lane and cannot prevent an unstarted sibling running.
type parallelChildRuntime struct {
	capacity *parallelChildCapacity
	lanes    chan struct{}
	branches map[int]*parallelChildLane
}

type parallelChildLane struct {
	runtime *parallelChildRuntime
	branch  int
	held    bool // accessed only by this branch's execution goroutine
	parked  bool
}

func (r *Runner) newParallelChildRuntime(in StartInput, p apiv1.Parallel, par *parallelExec, events []journal.Event) (*parallelChildRuntime, error) {
	if r.cfg.ChildWorkflowAdmission == nil || !workflowHasChildren(in) {
		return nil, nil
	}
	if err := parentParallelForkBudget(in.RunID, len(p.Branches), events); err != nil {
		return nil, err
	}
	projection, err := journal.ProjectChildWaits(events)
	if err != nil {
		return nil, err
	}
	branches, parked, finished := []int{}, []int{}, []int{}
	for i := range p.Branches {
		branch := par.branchSnapshot(i)
		branches = append(branches, branch.id)
		if branch.settled {
			finished = append(finished, branch.id)
		}
		if _, ok := projection.Waits[branch.id]; ok {
			parked = append(parked, branch.id)
		}
	}
	capacity, err := newParallelChildCapacity(in.RunID, r.cfg.ChildParentCapacity, branches, parked, finished)
	if err != nil {
		return nil, err
	}
	limit := max(1, min(int(p.MaxConcurrentBranches), len(branches)))
	runtime := &parallelChildRuntime{capacity: capacity, lanes: make(chan struct{}, limit), branches: map[int]*parallelChildLane{}}
	for _, branch := range branches {
		_, waiting := projection.Waits[branch]
		runtime.branches[branch] = &parallelChildLane{runtime: runtime, branch: branch, parked: waiting}
	}
	return runtime, nil
}

func workflowHasChildren(in StartInput) bool {
	if in.Machine == nil {
		return false
	}
	for _, task := range in.Machine.Def.Spec.Tasks {
		if task.ChildWorkflows != nil {
			return true
		}
	}
	return false
}

func (l *parallelChildLane) acquire(ctx context.Context) error {
	if l.held {
		return nil
	}
	select {
	case l.runtime.lanes <- struct{}{}:
		l.held = true
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (l *parallelChildLane) release() {
	if l.held {
		<-l.runtime.lanes
		l.held = false
	}
}
func (l *parallelChildLane) begin(ctx context.Context) error {
	if l.parked {
		return nil
	}
	return l.acquire(ctx)
}
func (l *parallelChildLane) park(ctx context.Context, publish func() error) error {
	if _, err := l.runtime.capacity.Park(ctx, l.branch, publish); err != nil {
		return err
	}
	l.parked = true
	l.release()
	return nil
}
func (l *parallelChildLane) resume(ctx context.Context, publish func() error) error {
	if err := l.acquire(ctx); err != nil {
		return err
	}
	if err := l.runtime.capacity.Resume(ctx, l.branch, publish); err != nil {
		l.release()
		return err
	}
	l.parked = false
	return nil
}

type parallelChildContinuation struct {
	lane    *parallelChildLane
	publish func() error
}

func (s parallelChildContinuation) Resume(ctx context.Context) error {
	return s.lane.resume(ctx, s.publish)
}

func publishChildWait(ctx context.Context, tf *taskFrame, event journal.Event) error {
	publish := func() error { return tf.jr.Append(event) }
	if tf.in.parallelChild != nil {
		return tf.in.parallelChild.park(ctx, publish)
	}
	return publish()
}

func (r *Runner) continueParallelChildWait(ctx context.Context, tf *taskFrame, attempt int, class journal.AttemptClass, record childWaitRecord, workspace *stageWorkspace) error {
	lane := tf.in.parallelChild
	if lane == nil {
		return errors.New("runner: parallel child lane is missing")
	}
	// Recovery starts with durable parked state but no in-memory scheduler
	// suspension handle. Repeating Park obtains the same whole-run lease.
	if err := lane.park(ctx, nil); err != nil {
		return err
	}
	issue, err := r.yieldChildCustody(ctx, tf, attempt, class, record.Request, ChildWorkspaceCustody{Path: workspace.path, RepoRef: tf.in.RepoRef})
	if err != nil {
		return err
	}
	completion, err := r.cfg.ChildHandoff.Wait(ctx, record.Request)
	if err != nil {
		return err
	}
	completion.DispositionIssue = issue
	pointer, err := recordChildCompletion(tf, attempt, class, record, completion)
	if err != nil {
		return err
	}
	resumed := parallelChildContinuation{lane: lane, publish: func() error {
		return tf.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: tf.t.Name, Attempt: attempt, AttemptClass: class, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": record.Request.RequestID, "context": pointer}})
	}}
	if err := resumeChildCapacity(ctx, resumed, tf, attempt, class); err != nil {
		return err
	}
	return restoreChildContext(tf, record, pointer, workspace)
}

func (r *Runner) parallelBranchExpired(ctx context.Context, reader *journal.Reader, branch branchState, seconds int32, now time.Time) (bool, error) {
	if seconds <= 0 || branch.startedAt.IsZero() {
		return false, nil
	}
	events, err := reader.Events()
	if err != nil {
		return false, err
	}
	// Branch IDs are reused by later parallel blocks. Only the current visit's
	// intervals belong to this branch execution clock.
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventBranchStarted && events[i].Branch == branch.id {
			events = events[i:]
			break
		}
	}
	elapsed, err := journal.ChildExecutionElapsed(events, branch.startedAt, now, &branch.id)
	if err != nil {
		return false, fmt.Errorf("parallel branch execution clock: %w", err)
	}
	return elapsed >= time.Duration(seconds)*time.Second, ctx.Err()
}

func startConcurrentBranch(jr *journal.Run, par *parallelExec, p apiv1.Parallel, index int) (branchState, error) {
	branch := par.branchSnapshot(index)
	if branch.started {
		return branch, nil
	}
	var cursors []journal.BranchCursor
	branch, cursors = par.startBranch(index)
	jr.SetBranchCursors(cursors)
	err := jr.Append(journal.Event{Type: journal.EventBranchStarted, Branch: branch.id, Parallel: p.Name, BranchName: branch.name, Stage: branch.start})
	return branch, err
}

func (p *parallelChildRuntime) runBranch(ctx context.Context, jr *journal.Run, par *parallelExec, spec apiv1.Parallel, index int, run func(branchState, *parallelChildLane) parallelBranchResult) parallelBranchResult {
	branch := par.branchSnapshot(index)
	lane := p.branches[branch.id]
	defer lane.release()
	if err := lane.begin(ctx); err != nil {
		paused := parallelDrainCancellation(ctx)
		if paused {
			err = nil
		}
		return parallelBranchResult{index: index, status: journal.BranchCancelled, paused: paused, err: err}
	}
	branch, err := startConcurrentBranch(jr, par, spec, index)
	if err != nil {
		return parallelBranchResult{index: index, status: journal.BranchFailed, err: err}
	}
	return run(branch, lane)
}
func (p *parallelChildRuntime) branchParked(branch int) bool {
	if p == nil {
		return false
	}
	return p.branches[branch].parked
}
func canLaunchParallel(next, total, running, limit int, child *parallelChildRuntime) bool {
	return next < total && (child != nil || running < limit)
}
func finishConcurrentBranch(ctx context.Context, child *parallelChildRuntime, jr *journal.Run, event journal.Event) error {
	publish := func() error { return jr.Append(event) }
	if child == nil {
		return publish()
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return child.capacity.Finish(finishCtx, event.Branch, publish)
}

func parallelChildTaskResume(in StartInput, history []journal.Event, state string, start *int32, class *journal.AttemptClass, accounting **resumeRetryAccounting) (*resumeContext, error) {
	if in.parallelChild == nil {
		return nil, nil
	}
	restored, ok := recoverChildTaskContext(history, state)
	if !ok {
		return nil, nil
	}
	if restored.childWaitErr != nil {
		return nil, restored.childWaitErr
	}
	if restored.mutated {
		return nil, errors.New("runner: interrupted child continuation touched an external mutation")
	}
	*start, *class = int32(restored.attempt)+1, restored.class
	*accounting = &resumeRetryAccounting{policyAttempts: restored.childWait.PolicyAttempts, infrastructureFailures: restored.childWait.InfrastructureFailures, replacementConsumesPolicy: *class != journal.AttemptInfra}
	return restored, nil
}
func parallelRecoveryBoundary(history []journal.Event, restored *resumeContext) (journal.Event, bool) {
	if restored != nil && !restored.childWaitRunning {
		return journal.Event{}, false
	}
	return lastParallelBoundary(history)
}
func withParallelChildResume(frame taskFrame, restored *resumeContext) taskFrame {
	if restored != nil {
		frame.childWaitResume, frame.childWaitAttempt, frame.childWaitClass = restored.childWait, restored.attempt, restored.class
		frame.childWaitCompletion = restored.childWaitCompletion
	}
	return frame
}

func (r *Runner) checkParallelBranchBoundary(ctx context.Context, jr *journal.Run, in StartInput, branch branchState, seconds int32, steps *atomic.Int64, restored **resumeContext) (journal.BranchStatus, error) {
	// Restoring a parked/continued stage is not a new branch boundary. Its
	// child result must settle before the next-stage deadline can terminate it.
	if *restored != nil {
		return "", nil
	}
	expired := !branch.deadline(seconds).IsZero() && !time.Now().Before(branch.deadline(seconds))
	if in.parallelChild != nil && seconds > 0 {
		reader, err := journal.OpenReadOnly(jr.Dir())
		if err != nil {
			return journal.BranchFailed, err
		}
		expired, err = r.parallelBranchExpired(ctx, reader, branch, seconds, time.Now())
		if err != nil {
			return journal.BranchFailed, err
		}
	}
	if expired {
		return journal.BranchTimedOut, nil
	}
	if steps.Add(1) > int64(r.maxSteps) {
		return journal.BranchFailed, fmt.Errorf("runner: run %q exceeded max steps (%d): possible loop", in.RunID, r.maxSteps)
	}
	return "", nil
}

// A one-lane child-enabled parallel still needs branch scheduling: its first
// parked owner must allow the next declared sibling to become runnable.
func (r *Runner) concurrentChildBranches(machine *workflow.Machine, p apiv1.Parallel) bool {
	return p.MaxConcurrentBranches > 1 || r.cfg.ChildWorkflowAdmission != nil && workflowHasChildren(StartInput{Machine: machine})
}
