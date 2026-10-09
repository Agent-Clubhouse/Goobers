package runner

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
)

var (
	errParallelFailFast = errors.New("parallel fail-fast cancellation")
	errParallelTerminal = errors.New("parallel branch selected a run terminal")
)

func reposEqual(a, b *apiv1.RepoRef) bool {
	return a.Provider == b.Provider &&
		a.BaseURL == b.BaseURL &&
		a.Owner == b.Owner &&
		a.Project == b.Project &&
		a.Name == b.Name
}

type branchJournal struct {
	run             *journal.Run
	branch          int
	setMachineState func(string)
}

func (j *branchJournal) Append(ev journal.Event) error {
	if ev.Branch == 0 {
		ev.Branch = j.branch
	}
	return j.run.Append(ev)
}

func (j *branchJournal) AppendChildStageStarted(ev journal.Event, continuation bool) (uint64, *apiv1.ChildWorkflowOrigin, error) {
	if ev.Branch == 0 {
		ev.Branch = j.branch
	}
	return j.run.AppendChildStageStarted(ev, continuation)
}

func (j *branchJournal) AppendIfAbsent(ev journal.Event, match func(journal.Event) bool) (bool, error) {
	if ev.Branch == 0 {
		ev.Branch = j.branch
	}
	return j.run.AppendIfAbsent(ev, func(existing journal.Event) bool {
		return existing.Branch == j.branch && match(existing)
	})
}

func (j *branchJournal) AppendBatchIfAbsent(ctx context.Context, events []journal.Event, key func(journal.Event) string) (int, error) {
	batch := append([]journal.Event(nil), events...)
	for i := range batch {
		if batch[i].Branch == 0 {
			batch[i].Branch = j.branch
		}
	}
	return j.run.AppendBatchIfAbsent(ctx, batch, func(event journal.Event) string {
		if event.Branch != j.branch {
			return ""
		}
		return key(event)
	})
}

func (j *branchJournal) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	return j.run.RecordBranchArtifact(j.branch, name, data)
}

func (j *branchJournal) RecordArtifactWithIntegrity(name string, data []byte, integrity apiv1.Integrity) (journal.Ref, error) {
	return j.run.RecordBranchArtifactWithIntegrity(j.branch, name, data, integrity)
}

func (j *branchJournal) RecordArtifactBounded(name string, data []byte, maxBytes int) (journal.Ref, error) {
	return j.run.RecordBranchArtifactBounded(j.branch, name, data, maxBytes)
}

func (j *branchJournal) RecordArtifactBoundedWithIntegrity(name string, data []byte, integrity apiv1.Integrity, maxBytes int) (journal.Ref, error) {
	return j.run.RecordBranchArtifactBoundedWithIntegrity(j.branch, name, data, integrity, maxBytes)
}

func (j *branchJournal) RecordStageArtifact(stage string, attempt int, class journal.AttemptClass, name string, data []byte) (journal.Ref, error) {
	return j.run.RecordBranchStageArtifact(j.branch, stage, attempt, class, name, data)
}

func (j *branchJournal) RecordStageArtifactWithIntegrity(stage string, attempt int, class journal.AttemptClass, name string, data []byte, integrity apiv1.Integrity) (journal.Ref, error) {
	return j.run.RecordBranchStageArtifactWithIntegrity(j.branch, stage, attempt, class, name, data, integrity)
}

func (j *branchJournal) ExportOutbox(stage string, attempt int, class journal.AttemptClass, files []journal.OutboxFile) ([]journal.Ref, error) {
	return j.run.ExportBranchOutbox(j.branch, stage, attempt, class, files)
}

func (j *branchJournal) RecordSpanWithSchema(stage, name, dataSchema string, data []byte) (journal.Ref, error) {
	return j.run.RecordBranchSpanWithSchema(j.branch, stage, name, dataSchema, data)
}

func (j *branchJournal) ObserveActivity()            { j.run.ObserveActivity() }
func (j *branchJournal) RepairAppendBoundary() error { return j.run.RepairAppendBoundary() }
func (j *branchJournal) Dir() string                 { return j.run.Dir() }
func (j *branchJournal) Seq() uint64                 { return j.run.Seq() }
func (j *branchJournal) AcceptOperatorMessage(request apiv1.OperatorMessageRequest) (apiv1.OperatorMessageRecord, bool, error) {
	return j.run.AcceptOperatorMessage(request)
}
func (j *branchJournal) AcknowledgeOperatorMessage(ack apiv1.OperatorMessageAcknowledgement) (apiv1.OperatorMessageRecord, error) {
	return j.run.AcknowledgeOperatorMessage(ack)
}
func (j *branchJournal) CompleteOperatorMessage(outcome apiv1.OperatorMessageOutcome) (apiv1.OperatorMessageRecord, error) {
	return j.run.CompleteOperatorMessage(outcome)
}
func (j *branchJournal) SetMachineState(state string) {
	j.setMachineState(state)
}

// appendInterruptedAttemptClosure journals the terminal closure of a branch
// attempt the crash caught in flight, then — only when a redispatch would
// actually follow (checkMutation, true unless an agentic budget-exceeded
// synthetic result already took its place) — refuses to continue if that
// attempt already touched an external mutation (ref.touched) before the
// crash cut off its own stage.finished write. Redispatching such an attempt
// would re-run a non-idempotent side effect (e.g. PR creation) (#3637), so
// this fails closed instead of the caller falling through to a fresh
// dispatch.
func appendInterruptedAttemptClosure(branchJournal *branchJournal, history []journal.Event, state string, attempt int, errorDetail *journal.ErrorDetail, runnerDetail map[string]any, checkMutation bool) error {
	if err := branchJournal.Append(journal.Event{
		Type:         journal.EventStageFinished,
		Stage:        state,
		Attempt:      attempt,
		AttemptClass: journal.AttemptInfra,
		Status:       string(apiv1.ResultFailure),
		Error:        errorDetail,
		Runner:       runnerDetail,
	}); err != nil {
		return err
	}
	if checkMutation {
		return refuseInterruptedMutation(history, state, attempt)
	}
	return nil
}

func refuseInterruptedMutation(history []journal.Event, state string, attempt int) error {
	if interruptedAttemptMutated(history, state, attempt) {
		return fmt.Errorf(
			"runner: refusing to resume stage %q: attempt %d already touched an external mutation before the runner was interrupted; redispatching would duplicate it — reconcile manually, then rerun",
			state, attempt,
		)
	}
	return nil
}

type parallelBranchResult struct {
	slot              *parallelBranchSlot
	index             int
	status            journal.BranchStatus
	lastStage         string
	lastResult        apiv1.ResultEnvelope
	pointers          []apiv1.ContextPointer
	completed         stageOutputs
	artifacts         int
	produced          bool
	failed            bool
	noOutput          bool
	workspaceRevision *apiv1.WorkspaceRevision
	repoRef           *apiv1.RepoRef
	terminalTarget    string
	terminalTask      *parallelTaskTerminal
	terminalGate      *parallelGateTerminal
	paused            bool
	err               error
}

type parallelTaskTerminal struct {
	task   apiv1.Task
	result apiv1.ResultEnvelope
}

type parallelGateTerminal struct {
	result     gate.Result
	lastStage  string
	lastResult apiv1.ResultEnvelope
}

type concurrentParallelResult struct {
	target            string
	runJoin           bool
	lastStage         string
	lastResult        apiv1.ResultEnvelope
	pointers          []apiv1.ContextPointer
	completed         stageOutputs
	parallel          *parallelExec
	terminalTask      *parallelTaskTerminal
	terminalGate      *parallelGateTerminal
	workspaceRevision *apiv1.WorkspaceRevision
	repoRef           *apiv1.RepoRef
	paused            bool
}

func validateConcurrentParallelWorkspaces(machine *workflow.Machine, p apiv1.Parallel) error {
	for _, branch := range p.Branches {
		seen := map[string]bool{}
		queue := []string{branch.Start}
		for len(queue) > 0 {
			state := queue[0]
			queue = queue[1:]
			if state == "" || workflow.IsReservedAnyTarget(state) || seen[state] {
				continue
			}
			seen[state] = true

			if task, ok := machine.Task(state); ok {
				mode := taskWorkspaceMode(task)
				if !parallelWorkspaceAllowed(machine, p, mode) {
					return fmt.Errorf("parallel %q: maxConcurrentBranches %d requires every branch stage to use scratch or repo-readonly; branch %q task %q resolves to workspace %q",
						p.Name, p.MaxConcurrentBranches, branch.Name, task.Name, mode)
				}
				if task.Run != nil && task.Run.SyncBase && !parallelHasChildStage(machine, p) {
					return fmt.Errorf("parallel %q: branch %q task %q requests syncBase, which requires a writable repo workspace",
						p.Name, branch.Name, task.Name)
				}
			} else if g, ok := machine.Gate(state); ok {
				if g.Evaluator == apiv1.EvaluatorHuman {
					return fmt.Errorf("parallel %q: branch %q contains human gate %q, which cannot execute concurrently", p.Name, branch.Name, g.Name)
				}
				if g.Evaluator == apiv1.EvaluatorAgentic {
					mode := gateWorkspaceMode(g)
					if !parallelWorkspaceAllowed(machine, p, mode) {
						return fmt.Errorf("parallel %q: maxConcurrentBranches %d requires every branch stage to use scratch or repo-readonly; branch %q gate %q resolves to workspace %q",
							p.Name, p.MaxConcurrentBranches, branch.Name, g.Name, mode)
					}
				}
			} else if _, ok := machine.Parallel(state); ok {
				return fmt.Errorf("parallel %q: branch %q contains nested parallel %q, which cannot execute concurrently", p.Name, branch.Name, state)
			} else {
				return fmt.Errorf("parallel %q: branch %q reaches unknown state %q", p.Name, branch.Name, state)
			}

			queue = append(queue, machine.Outgoing(state)...)
		}
	}
	return nil
}

func (r *Runner) runConcurrentParallel(
	ctx context.Context,
	jr *journal.Run,
	in StartInput,
	p apiv1.Parallel,
	par *parallelExec,
	basePointers []apiv1.ContextPointer,
	baseLastStage string,
	baseLastResult apiv1.ResultEnvelope,
	baseCompleted stageOutputs,
	workspaceBranch string,
	reg SecretRegistrar,
	stepBudget *atomic.Int64,
) (concurrentParallelResult, error) {
	// Concurrent workers always use explicit branch journals. Keeping the run
	// default at root prevents manager and terminal events from inheriting the
	// last active branch after a resume.
	jr.SetBranch(0)
	jr.SetMachineState(p.Name)
	if par == nil {
		par = newParallelExec(p)
		jr.SetBranchCursors(par.cursors())
		if err := jr.Append(journal.Event{
			Type:         journal.EventParallelStarted,
			Parallel:     p.Name,
			Completeness: par.completeness(),
		}); err != nil {
			return concurrentParallelResult{}, err
		}
	}

	rd, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return concurrentParallelResult{}, err
	}
	events, err := rd.Events()
	if err != nil {
		return concurrentParallelResult{}, err
	}
	if rootEvents, ok := parallelRootEvents(events, p.Name); ok {
		basePointers = reconstructPointers(rootEvents, in.Machine)
		baseCompleted = reconstructStageOutputs(rootEvents, in.Machine)
		baseLastStage, baseLastResult, _ = lastFinishedSubject(rootEvents)
		workspaceBranch = lastWorkspaceBranch(rootEvents, in.Machine, r.branchNamespaceFor(in.Gaggle))
	}
	branchEvents := newParallelBranchEventIndex(events, p.Name)
	runtime, err := r.prepareParallelRuntime(ctx, jr, in, par, workspaceBranch, events)
	if err != nil {
		return concurrentParallelResult{}, err
	}

	limit := int(p.MaxConcurrentBranches)
	if limit > len(p.Branches) {
		limit = len(p.Branches)
	}
	branchCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	results := make(chan parallelBranchResult, len(p.Branches))
	outcomes := make([]*parallelBranchResult, len(p.Branches))
	queue := make([]int, 0, len(p.Branches))
	terminalTriggered := false
	for i := range p.Branches {
		branch := par.branchSnapshot(i)
		history := branchEvents.events(branch.id)
		if branch.settled {
			var err error
			var terminalTarget string
			outcomes[i], terminalTarget, _, _, err = r.settledParallelBranchResult(ctx, branch, history, baseCompleted, in, i)
			if err != nil {
				return concurrentParallelResult{}, err
			}
			terminalTriggered = terminalTriggered || terminalTarget != ""
			continue
		}
		queue = append(queue, i)
	}
	if ctx.Err() != nil {
		return concurrentParallelResult{parallel: par, paused: true}, nil
	}

	slots := newParallelBranchSlots(limit)
	dispatch := &parallelDispatch{released: slots.changed, queue: queue, outcomes: outcomes, results: results, cancel: cancel, failurePolicy: p.FailurePolicy, terminalTriggered: terminalTriggered}
	dispatch.settle = func(result parallelBranchResult) error {
		return r.settleParallelRuntimeBranch(ctx, jr, in, par, runtime, result)
	}
	dispatch.cancelQueued = func() error {
		return cancelQueuedParallelBranches(par, queue, &dispatch.next, outcomes, baseCompleted, branchEvents, in, dispatch.settle)
	}
	dispatch.launch = func(index int) (bool, error) {
		slot, available := slots.tryAcquire()
		if !available {
			return false, nil
		}
		branch := par.branchSnapshot(index)
		if !branch.started {
			var cursors []journal.BranchCursor
			branch, cursors = par.startBranch(index)
			jr.SetBranchCursors(cursors)
			if err := jr.Append(journal.Event{
				Type:       journal.EventBranchStarted,
				Branch:     branch.id,
				Parallel:   p.Name,
				BranchName: branch.name,
				Stage:      branch.start,
			}); err != nil {
				slot.release()
				return false, err
			}
		}
		branchInput := parallelBranchInput(in, runtime, slot, branch.id, workspaceBranch)
		go func() {
			result := r.runParallelBranch(
				branchCtx, jr, par, branchInput, branch, basePointers, baseLastStage,
				baseLastResult, baseCompleted, branchInput.WorkspaceBranch, reg,
				branchEvents.events(branch.id), stepBudget,
			)
			result.slot = slot
			results <- result
		}()
		return true, nil
	}

	if err := dispatch.run(); err != nil {
		return concurrentParallelResult{}, err
	}
	if dispatch.draining {
		return concurrentParallelResult{parallel: par, paused: true}, nil
	}

	if err := r.verifyParallelForkResults(ctx, jr, in, p, runtime, outcomes); err != nil {
		return concurrentParallelResult{}, err
	}
	mergedCompleted := cloneStageOutputs(baseCompleted)
	lastStage, lastResult := baseLastStage, baseLastResult
	workspaceRevision := in.workspaceRevision.DeepCopy()
	var repoRef *apiv1.RepoRef
	for _, outcome := range outcomes {
		if outcome == nil {
			continue
		}
		if outcome.workspaceRevision != nil {
			var err error
			workspaceRevision, err = workspacerevision.Accept(workspaceRevision, outcome.workspaceRevision)
			if err != nil {
				return concurrentParallelResult{}, fmt.Errorf("runner: reconcile parallel workspace revision: %w", err)
			}
		}
		if outcome.repoRef != nil {
			if repoRef == nil {
				repoRef = outcome.repoRef
			} else if !reposEqual(repoRef, outcome.repoRef) {
				return concurrentParallelResult{}, fmt.Errorf("runner: reconcile parallel repository: conflicting configured repositories selected by parallel branches")
			}
		}
		for stage, outputs := range outcome.completed {
			mergedCompleted.put(stage, outputs)
		}
		if outcome.lastStage != "" {
			lastStage, lastResult = outcome.lastStage, outcome.lastResult
		}
	}

	terminalTarget, terminalTask, terminalGate := parallelTerminalOutcome(outcomes)
	target, runJoin := par.route()
	if terminalTarget != "" {
		target, runJoin = terminalTarget, false
	}
	if err := r.joinParallelForks(ctx, jr, in, p, runtime, outcomes, runJoin); err != nil {
		return concurrentParallelResult{}, err
	}
	jr.SetBranchCursors(nil)
	if err := jr.Append(journal.Event{
		Type:         journal.EventParallelFinished,
		Parallel:     p.Name,
		Completeness: par.completeness(),
		Target:       target,
	}); err != nil {
		return concurrentParallelResult{}, err
	}
	mergedPointers := append([]apiv1.ContextPointer(nil), basePointers...)
	if runJoin {
		mergedPointers = par.joinPointers(basePointers)
	}
	return concurrentParallelResult{
		target:            target,
		runJoin:           runJoin,
		lastStage:         lastStage,
		lastResult:        lastResult,
		pointers:          mergedPointers,
		completed:         mergedCompleted,
		parallel:          par,
		terminalTask:      terminalTask,
		terminalGate:      terminalGate,
		workspaceRevision: workspaceRevision,
		repoRef:           repoRef,
	}, nil
}

func newParallelGateEvaluator(r *Runner, in StartInput, branchJournal gate.Journal, history []journal.Event, visitedStages map[string]bool) *gate.Evaluator {
	return &gate.Evaluator{
		Automated:   r.cfg.Automated,
		Journal:     branchJournal,
		MaxRepasses: int(in.RunControls.MaxRepasses),
		Attempts:    gateRepassSeed(history),
		IsNeedsHumanTarget: func(target string) bool {
			task, ok := in.Machine.Task(target)
			return ok && task.Inputs["status"] == "needs-human"
		},
		RepassAttempts:               targetRepassSeed(history),
		InfrastructureAttempts:       gateInfrastructureSeed(history),
		InfrastructureRepassAttempts: infrastructureTargetRepassSeed(history),
		PollAttempts:                 pollingTargetSeed(history),
		IsReentry: func(target string) bool {
			return visitedStages[target]
		},
		LastDiffDigest: gateDiffSeed(history),
	}
}

func (r *Runner) runParallelBranch(
	ctx context.Context,
	jr *journal.Run,
	par *parallelExec,
	in StartInput,
	branch branchState,
	basePointers []apiv1.ContextPointer,
	baseLastStage string,
	baseLastResult apiv1.ResultEnvelope,
	baseCompleted stageOutputs,
	workspaceBranch string,
	reg SecretRegistrar,
	history []journal.Event,
	stepBudget *atomic.Int64,
) (result parallelBranchResult) {
	initialRepoRef := in.RepoRef
	defer func() {
		captureParallelBranchBinding(&result, &initialRepoRef, &in.RepoRef, &in.workspaceRevision)
	}()
	result = initialParallelBranchResult(branch, baseLastStage, baseLastResult, baseCompleted, in, history)
	in, result.err = r.restoreWorkspaceRevision(ctx, in, history)
	if result.err != nil {
		result.status = journal.BranchFailed
		return result
	}
	branchJournal := &branchJournal{
		run:    jr,
		branch: branch.id,
		setMachineState: func(state string) {
			jr.SetBranchCursors(par.moveBranch(branch.id, state))
		},
	}
	ex := newExecutors(r.cfg, branchJournal, reg)
	visitedStages := stageVisitSeed(history)
	gateEval := newParallelGateEvaluator(r, in, branchJournal, history, visitedStages)
	state := branch.machine
	if state == "" {
		state = branch.start
	}
	branchRecorded, reboundRecorded := true, ""
	if lastStage, lastResult, ok := lastFinishedSubject(history); ok {
		result.lastStage, result.lastResult = lastStage, lastResult
	}
	if rebound := lastWorkspaceBranch(history, in.Machine, r.branchNamespaceFor(in.Gaggle)); rebound != "" {
		workspaceBranch = rebound
	}
	if retryTarget, pending := pendingRetryTarget(history, in.Machine, result.lastStage, result.lastResult); pending {
		state = retryTarget
	}

	restored, err := recoverParallelStage(branchJournal, in.Machine, state, result.lastResult, history)
	if err != nil {
		result.status, result.err = journal.BranchFailed, err
		return result
	}
	replayTask, replayGate, replayGateEvent := restored.task, restored.gate, restored.gateEvent
	startAttempt, firstClass := restored.attempt, restored.class
	committedWorkOnInfra, resumeAccounting := restored.committed, restored.accounting
	var retryInstructionAddendum string

	for {
		if r.stopParallelBranchAtBoundary(ctx, jr, in, par, branch, stepBudget, &result, restored) {
			return result
		}
		branchJournal.SetMachineState(state)

		if task, ok := in.Machine.Task(state); ok {
			var stageResult apiv1.ResultEnvelope
			var produced []apiv1.ContextPointer
			var err error
			replayed := replayTask != nil
			if replayed {
				stageResult = *replayTask
				replayTask, restored.child, restored.parent = nil, nil, false
			} else {
				attemptAddendum := retryInstructionAddendum
				retryInstructionAddendum = ""
				frame := taskFrame{
					jr: branchJournal, in: in, ex: ex, t: task,
					upstream:        branchContextPointers(basePointers, result.pointers),
					upstreamResult:  result.lastResult,
					completed:       result.completed,
					workspaceBranch: workspaceBranch, branchRecorded: &branchRecorded, reboundRecorded: &reboundRecorded,
					workspaceRevision: &in.workspaceRevision,
					repoRef:           &in.RepoRef,
				}
				applyChildTaskResume(&frame, restored.child)
				restored.child, restored.parent = nil, false
				stageResult, produced, err = r.runTask(
					ctx, frame,
					branch.id, startAttempt, firstClass, attemptAddendum,
					nil, committedWorkOnInfra, resumeAccounting,
				)
				startAttempt = 1
				firstClass = ""
				resumeAccounting = nil
			}
			if errors.Is(err, errChildWaitDrain) {
				result.status, result.paused = journal.BranchCancelled, true
				return result
			}
			if err = taskDispatchError(task.Name, stageResult, err); err != nil {
				result.status, result.err = journal.BranchFailed, err
				return result
			}
			if !replayed {
				result.pointers = append(result.pointers, produced...)
				result.artifacts += artifactPointerCount(produced)
				outputs := stageResult.Outputs
				if stageResult.Status == apiv1.ResultFailure && task.ContinueOnError {
					outputs = nil
				}
				if len(outputs) > 0 || len(produced) > 0 {
					result.produced = true
				}
			}
			result.lastStage, result.lastResult = task.Name, stageResult
			visitedStages[task.Name] = true
			if stageResult.Status == apiv1.ResultFailure && task.ContinueOnError {
				result.completed.clear(task.Name)
			} else {
				result.completed.record(task.Name, stageResult.Outputs, stageResult.Integrity)
			}
			workspaceBranch = r.parallelWorkspaceBranchAfterTask(in.Gaggle, task, stageResult, workspaceBranch)
			handoffScope := parallelHandoffScope{branchJournal, gateEval, visitedStages, in.Machine, branch.start, basePointers}
			switch post := r.parallelPostTaskTransition(ctx, handoffScope, task, stageResult, &result); post.kind {
			case parallelPostReturn:
				return result
			case parallelPostRetry:
				retryInstructionAddendum = post.addendum
				state = post.target
				continue
			}

			switch stageResult.Status {
			case apiv1.ResultNoWork:
				result.noOutput = true
				result.status = journal.BranchNoOutput
				return result
			case apiv1.ResultBlocked:
				result.failed = true
				result.status = journal.BranchFailed
				result.terminalTarget = workflow.TargetEscalate
				result.terminalTask = &parallelTaskTerminal{task: task, result: stageResult}
				return result
			case apiv1.ResultFailure:
				if task.ContinueOnError {
					if err := journalToleratedFailure(branchJournal, task.Name); err != nil {
						result.status, result.err = journal.BranchFailed, err
						return result
					}
					result.lastResult.Outputs = nil
				} else if _, isGate := in.Machine.Gate(task.Next); !isGate {
					result.failed = true
					result.status = journal.BranchFailed
					return result
				} else if isNonRetryableEscalation(stageResult.Error) {
					target := taskEscalationTarget(in.Machine, task)
					switch target {
					case workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete:
						result.failed = true
						result.status = journal.BranchFailed
						result.terminalTarget = target
						if target == workflow.TerminalComplete {
							result.terminalTarget = workflow.TargetEscalate
						}
						result.terminalTask = &parallelTaskTerminal{task: task, result: stageResult}
						return result
					default:
						state = target
						continue
					}
				}
			}
			switch task.Next {
			case workflow.TargetJoin:
				result.status = parallelBranchStatus(result)
				return result
			case workflow.TargetAbort, workflow.TargetEscalate:
				result.failed = true
				result.status, result.terminalTarget = journal.BranchFailed, task.Next
				result.terminalTask = &parallelTaskTerminal{task: task, result: stageResult}
				return result
			case workflow.TerminalComplete:
				result.status = journal.BranchFailed
				result.err = fmt.Errorf("runner: parallel %q branch %q task %q completed the run instead of routing to %q", par.spec.Name, branch.name, task.Name, workflow.TargetJoin)
				return result
			default:
				state = task.Next
				continue
			}
		}

		if g, ok := in.Machine.Gate(state); ok {
			if g.Evaluator == apiv1.EvaluatorHuman {
				result.status = journal.BranchFailed
				result.err = fmt.Errorf("runner: parallel %q branch %q reached human gate %q", par.spec.Name, branch.name, g.Name)
				return result
			}
			_, knownOutcome, _ := retryFailureClass(g, result.lastResult)
			replayed := replayGate != nil
			var gr gate.Result
			var err, removeErr error
			if replayed {
				gr = *replayGate
				replayGate = nil
			} else {
				if err := branchJournal.Append(journal.Event{Type: journal.EventGatePaused, Gate: g.Name}); err != nil {
					result.status, result.err = journal.BranchFailed, err
					return result
				}
				gr, err, removeErr = r.evaluateGate(
					ctx, branchJournal, gateEval, ex, in, g, result.lastStage,
					result.lastResult, branchContextPointers(basePointers, result.pointers),
					nil, "", workspaceBranch, knownOutcome,
				)
			}
			if removeErr != nil {
				if appendErr := branchJournal.Append(journal.Event{
					Type:  journal.EventError,
					Gate:  g.Name,
					Error: workspaceCleanupErrorDetail(removeErr),
				}); appendErr != nil {
					result.status, result.err = journal.BranchFailed, appendErr
					return result
				}
			}
			if err != nil {
				result.status, result.err = journal.BranchFailed, err
				return result
			}
			retryClass, _, retryable := retryFailureClassForGateResult(g, result.lastResult, gr.Outcome)
			var retryTarget string
			var retry bool
			if replayed && replayGateEvent != nil && hasRetryDecisionAfter(history, *replayGateEvent) {
				retryTarget, retry = completedGateRetry(gr, retryable)
			} else {
				retryTarget, retry, err = routeRetryDecision(branchJournal, gr, result.lastStage, result.lastResult, retryClass, retryable)
			}
			replayGateEvent = nil
			if err != nil {
				result.status, result.err = journal.BranchFailed, err
				return result
			}
			// #3932: this branch's gate arm is PRODUCED by the same helper the
			// sequential walk uses (recordGateBranchInjection), on both the
			// retry route and the advance route.
			//
			// runBranch used to carry a hand-copied half of it — the verdict
			// pointer without the learning episode — which made
			// maxConcurrentBranches, a scheduling bound, decide whether a
			// repass received its correction. Both routes go through the
			// helper because #3943 established that a stage-re-entering branch
			// arrives by EITHER: an agentic reviewer's needs-changes is a true
			// repass the retry classifier declines, so it advances rather than
			// retries. Wiring only the retry route would have rebuilt the same
			// divergence for the branch that matters most.
			//
			// Only the ROUTING is local: pointers land in the branch's own
			// accumulator, and a recorded artifact is charged to the branch's
			// artifact/produced accounting, which is what a join's
			// completeness record reports.
			injectTarget := gr.Target
			if retry {
				injectTarget = retryTarget
			}
			injection, err := recordGateBranchInjection(
				branchJournal, in, g.Name, injectTarget, gr, result.lastStage, result.lastResult, replayed,
			)
			if err != nil {
				result.status, result.err = journal.BranchFailed, err
				return result
			}
			for _, pointer := range injection.pointers() {
				result.pointers = append(result.pointers, pointer)
				if pointer.Artifact != nil {
					result.artifacts++
				}
				result.produced = true
			}
			if retry {
				state = retryTarget
				continue
			}
			if ctx.Err() != nil {
				result.status = journal.BranchCancelled
				result.paused = parallelDrainCancellation(ctx)
				return result
			}
			switch gr.Target {
			case workflow.TargetJoin:
				if result.lastResult.Status == apiv1.ResultFailure && !gateClearsFailure(gr, g) {
					result.failed = true
				}
				result.status = parallelBranchStatus(result)
				return result
			case workflow.TargetAbort, workflow.TargetEscalate:
				result.failed = true
				result.status, result.terminalTarget = journal.BranchFailed, gr.Target
				result.terminalGate = &parallelGateTerminal{
					result: gr, lastStage: result.lastStage, lastResult: result.lastResult,
				}
				return result
			case workflow.TerminalComplete:
				result.status = journal.BranchFailed
				result.err = fmt.Errorf("runner: parallel %q branch %q gate %q completed the run instead of routing to %q", par.spec.Name, branch.name, g.Name, workflow.TargetJoin)
				return result
			default:
				if gr.Escalated {
					if reason, notify := terminalGateNotificationReason(in.Machine, gr); notify {
						if err := r.notifyTerminalGate(stalledAttemptContext(ctx), jr, in.RunID, in.RepoRef, in.Item, gr, reason); err != nil {
							result.status, result.err = journal.BranchFailed, err
							return result
						}
					}
				}
				state = gr.Target
				continue
			}
		}

		result.status = journal.BranchFailed
		result.err = fmt.Errorf("runner: parallel %q branch %q reached unknown state %q", par.spec.Name, branch.name, state)
		return result
	}
}

func parallelBranchStatus(result parallelBranchResult) journal.BranchStatus {
	return branchStatus(&branchState{
		status:    result.status,
		artifacts: result.artifacts,
		produced:  result.produced,
		failed:    result.failed,
		noOutput:  result.noOutput,
	})
}

func artifactPointerCount(pointers []apiv1.ContextPointer) int {
	count := 0
	for _, pointer := range pointers {
		if pointer.Artifact != nil {
			count++
		}
	}
	return count
}

func (r *Runner) parallelWorkspaceBranchAfterTask(gaggle string, task apiv1.Task, result apiv1.ResultEnvelope, current string) string {
	if result.Status == apiv1.ResultFailure && task.ContinueOnError {
		return current
	}
	if rebound := rebindWorkspaceBranch(task, result, r.branchNamespaceFor(gaggle)); rebound != "" {
		return rebound
	}
	return current
}

type parallelPostTaskKind int

const (
	parallelPostContinue parallelPostTaskKind = iota
	parallelPostReturn
	parallelPostRetry
)

type parallelPostTaskResult struct {
	kind     parallelPostTaskKind
	target   string
	addendum string
}

// parallelHandoffScope is the concurrent branch context an invalid handoff
// reroute needs: where to journal, which budget to charge, and which producers
// the branch may legitimately re-execute.
type parallelHandoffScope struct {
	jr            executionJournal
	eval          *gate.Evaluator
	visitedStages map[string]bool
	machine       *workflow.Machine
	branchStart   string
	inherited     []apiv1.ContextPointer
}

func (r *Runner) parallelPostTaskTransition(
	ctx context.Context,
	scope parallelHandoffScope,
	consumer apiv1.Task,
	stageResult apiv1.ResultEnvelope,
	result *parallelBranchResult,
) parallelPostTaskResult {
	if ctx.Err() != nil {
		result.status = journal.BranchCancelled
		result.paused = parallelDrainCancellation(ctx)
		return parallelPostTaskResult{kind: parallelPostReturn}
	}
	retry, ok := invalidHandoffRetryFromResult(stageResult)
	if !ok {
		return parallelPostTaskResult{}
	}
	target, addendum, terminal := parallelInvalidHandoffOutcome(scope, consumer, retry, result)
	if terminal {
		return parallelPostTaskResult{kind: parallelPostReturn}
	}
	return parallelPostTaskResult{kind: parallelPostRetry, target: target, addendum: addendum}
}

func parallelInvalidHandoffOutcome(
	scope parallelHandoffScope,
	consumer apiv1.Task,
	retry invalidHandoffRetry,
	result *parallelBranchResult,
) (target, addendum string, terminal bool) {
	unsupported := invalidHandoffRerouteUnsupported(scope.machine, scope.branchStart, scope.inherited, retry.Producer)
	budget := evaluatorRepassBudget(scope.eval)
	decision := decideInvalidHandoff(&budget, consumer.Name, retry, unsupported, scope.visitedStages[retry.Producer], scope.eval.MaxRepasses)
	applyEvaluatorRepassBudget(scope.eval, budget)
	if err := scope.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: consumer.Name, Runner: decision.fields}); err != nil {
		result.status, result.err = journal.BranchFailed, fmt.Errorf("runner: journal invalid handoff reroute for %q: %w", consumer.Name, err)
		return "", "", true
	}
	if decision.escalate {
		// Escalate the whole parallel directly: replaying the consumer's
		// invalid-handoff result through the root walk would reroute it there.
		result.failed = true
		result.status = journal.BranchFailed
		result.terminalTarget = workflow.TargetEscalate
		return "", "", true
	}
	result.pointers = removeStageArtifactPointers(result.pointers, retry.Producer)
	result.artifacts = artifactPointerCount(result.pointers)
	return retry.Producer, decision.addendum, false
}

func completedGateRetry(result gate.Result, retryable bool) (string, bool) {
	if !retryable || result.Outcome == gate.OutcomePass || result.Escalated {
		return "", false
	}
	switch result.Target {
	case workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete:
		return "", false
	default:
		return result.Target, true
	}
}

func parallelBranchTerminal(history []journal.Event, machine *workflow.Machine) (string, *parallelTaskTerminal, *parallelGateTerminal) {
	for i := len(history) - 1; i >= 0; i-- {
		source := history[i]
		if isInvalidHandoffEscalation(source) {
			return workflow.TargetEscalate, nil, nil
		}
		if source.Type == journal.EventGateEvaluated {
			if source.Target != workflow.TargetAbort && source.Target != workflow.TargetEscalate {
				continue
			}
			result := gateResultFromEvent(source)
			lastStage, lastResult, _ := lastFinishedSubject(history[:i])
			return source.Target, nil, &parallelGateTerminal{
				result: result, lastStage: lastStage, lastResult: lastResult,
			}
		}
		if source.Type != journal.EventStageFinished || isInterruptedAttemptMarker(source) {
			continue
		}
		task, ok := machine.Task(source.Stage)
		if !ok {
			continue
		}
		_, result, ok := lastFinishedSubject([]journal.Event{source})
		if !ok {
			continue
		}
		target := ""
		switch result.Status {
		case apiv1.ResultBlocked:
			target = workflow.TargetEscalate
		case apiv1.ResultFailure:
			if task.ContinueOnError {
				target = task.Next
			} else if _, nextIsGate := machine.Gate(task.Next); nextIsGate && isNonRetryableEscalation(result.Error) {
				target = taskEscalationTarget(machine, task)
				if target == workflow.TerminalComplete {
					target = workflow.TargetEscalate
				}
			}
		case apiv1.ResultSuccess:
			target = task.Next
		}
		if target == workflow.TargetAbort || target == workflow.TargetEscalate {
			return target, &parallelTaskTerminal{task: task, result: result}, nil
		}
	}
	return "", nil, nil
}

func parallelRootEvents(events []journal.Event, parallel string) ([]journal.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventParallelStarted && events[i].Parallel == parallel {
			return events[:i], true
		}
	}
	return nil, false
}

type parallelBranchEventIndex struct {
	byBranch map[int][]journal.Event
}

func newParallelBranchEventIndex(events []journal.Event, parallel string) parallelBranchEventIndex {
	index := parallelBranchEventIndex{byBranch: make(map[int][]journal.Event)}
	for _, event := range events {
		if event.Type == journal.EventParallelStarted && event.Parallel == parallel {
			index.byBranch = make(map[int][]journal.Event)
			continue
		}
		index.byBranch[event.Branch] = append(index.byBranch[event.Branch], event)
	}
	return index
}

func (i parallelBranchEventIndex) events(branch int) []journal.Event {
	return i.byBranch[branch]
}

func lastParallelBoundary(events []journal.Event) (journal.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case journal.EventBranchStarted, journal.EventStageStarted, journal.EventStageFinished,
			journal.EventGatePaused, journal.EventGateStarted, journal.EventGateEvaluated:
			return events[i], true
		}
	}
	return journal.Event{}, false
}

func branchStageOutputs(base stageOutputs, history []journal.Event, machine *workflow.Machine) stageOutputs {
	out := cloneStageOutputs(base)
	for stage, produced := range reconstructStageOutputs(history, machine) {
		out.put(stage, produced)
	}
	return out
}

func cloneStageOutputs(in stageOutputs) stageOutputs {
	out := stageOutputs{}
	for stage, produced := range in {
		out.put(stage, produced)
	}
	return out
}

func branchContextPointers(base, produced []apiv1.ContextPointer) []apiv1.ContextPointer {
	out := make([]apiv1.ContextPointer, 0, len(base)+len(produced))
	out = append(out, base...)
	out = append(out, produced...)
	return out
}

func parallelDrainCancellation(ctx context.Context) bool {
	return !errors.Is(context.Cause(ctx), errParallelFailFast) &&
		!errors.Is(context.Cause(ctx), errParallelTerminal)
}
