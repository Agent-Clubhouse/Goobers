package runner

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

var (
	errParallelFailFast = errors.New("parallel fail-fast cancellation")
	errParallelTerminal = errors.New("parallel branch selected a run terminal")
)

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
func (j *branchJournal) SetMachineState(state string) {
	j.setMachineState(state)
}

type parallelBranchResult struct {
	index          int
	status         journal.BranchStatus
	lastStage      string
	lastResult     apiv1.ResultEnvelope
	pointers       []apiv1.ContextPointer
	completed      stageOutputs
	artifacts      int
	produced       bool
	failed         bool
	noOutput       bool
	terminalTarget string
	terminalTask   *parallelTaskTerminal
	terminalGate   *parallelGateTerminal
	paused         bool
	err            error
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
	target       string
	runJoin      bool
	lastStage    string
	lastResult   apiv1.ResultEnvelope
	pointers     []apiv1.ContextPointer
	completed    stageOutputs
	parallel     *parallelExec
	terminalTask *parallelTaskTerminal
	terminalGate *parallelGateTerminal
	paused       bool
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
				if mode != apiv1.WorkspaceScratch && mode != apiv1.WorkspaceRepoReadOnly {
					return fmt.Errorf("parallel %q: maxConcurrentBranches %d requires every branch stage to use scratch or repo-readonly; branch %q task %q resolves to workspace %q",
						p.Name, p.MaxConcurrentBranches, branch.Name, task.Name, mode)
				}
				if task.Run != nil && task.Run.SyncBase {
					return fmt.Errorf("parallel %q: branch %q task %q requests syncBase, which requires a writable repo workspace",
						p.Name, branch.Name, task.Name)
				}
			} else if g, ok := machine.Gate(state); ok {
				if g.Evaluator == apiv1.EvaluatorHuman {
					return fmt.Errorf("parallel %q: branch %q contains human gate %q, which cannot execute concurrently", p.Name, branch.Name, g.Name)
				}
				if g.Evaluator == apiv1.EvaluatorAgentic {
					mode := gateWorkspaceMode(g)
					if mode != apiv1.WorkspaceScratch && mode != apiv1.WorkspaceRepoReadOnly {
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
		history := parallelBranchEvents(events, p.Name, branch.id)
		if branch.settled {
			lastStage, lastResult, _ := lastFinishedSubject(history)
			terminalTarget, terminalTask, terminalGate := parallelBranchTerminal(history, in.Machine)
			outcomes[i] = &parallelBranchResult{
				index:          i,
				status:         branch.status,
				lastStage:      lastStage,
				lastResult:     lastResult,
				pointers:       branch.pointers,
				completed:      branchStageOutputs(baseCompleted, history, in.Machine),
				artifacts:      branch.artifacts,
				produced:       branch.produced,
				failed:         branch.failed,
				noOutput:       branch.noOutput,
				terminalTarget: terminalTarget,
				terminalTask:   terminalTask,
				terminalGate:   terminalGate,
			}
			terminalTriggered = terminalTriggered || terminalTarget != ""
			continue
		}
		queue = append(queue, i)
	}
	if ctx.Err() != nil {
		return concurrentParallelResult{parallel: par, paused: true}, nil
	}

	next, running := 0, 0
	var firstErr error
	failFast := false
	draining := false

	launch := func(index int) error {
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
				return err
			}
		}
		running++
		go func() {
			results <- r.runParallelBranch(
				branchCtx, jr, par, in, branch, basePointers, baseLastStage,
				baseLastResult, baseCompleted, workspaceBranch, reg,
				parallelBranchEvents(events, p.Name, branch.id), stepBudget,
			)
		}()
		return nil
	}

	cancelQueued := func() error {
		for next < len(queue) {
			index := queue[next]
			branch := par.branchSnapshot(index)
			cursors := par.settleBranch(
				branch.id, journal.BranchCancelled, branch.artifacts, branch.pointers,
				branch.produced, branch.failed, branch.noOutput,
			)
			jr.SetBranchCursors(cursors)
			if err := jr.Append(journal.Event{
				Type:         journal.EventBranchFinished,
				Branch:       branch.id,
				Parallel:     p.Name,
				BranchName:   branch.name,
				BranchStatus: journal.BranchCancelled,
			}); err != nil {
				return err
			}
			outcomes[index] = &parallelBranchResult{
				index:     index,
				status:    journal.BranchCancelled,
				pointers:  branch.pointers,
				completed: branchStageOutputs(baseCompleted, parallelBranchEvents(events, p.Name, branch.id), in.Machine),
				artifacts: branch.artifacts,
				produced:  branch.produced,
				failed:    branch.failed,
				noOutput:  branch.noOutput,
			}
			next++
		}
		return nil
	}

	if terminalTriggered {
		if err := cancelQueued(); err != nil {
			return concurrentParallelResult{}, err
		}
	}
	for next < len(queue) && running < limit {
		if err := launch(queue[next]); err != nil {
			firstErr = err
			cancel(err)
			break
		}
		next++
	}

	for running > 0 {
		result := <-results
		running--
		outcomes[result.index] = &result
		if result.paused {
			draining = true
		} else {
			branch := par.branchSnapshot(result.index)
			cursors := par.settleBranch(
				branch.id, result.status, result.artifacts, result.pointers,
				result.produced, result.failed, result.noOutput,
			)
			jr.SetBranchCursors(cursors)
			if err := jr.Append(journal.Event{
				Type:         journal.EventBranchFinished,
				Branch:       branch.id,
				Parallel:     p.Name,
				BranchName:   branch.name,
				BranchStatus: result.status,
			}); err != nil && firstErr == nil {
				firstErr = err
				cancel(err)
			}
		}
		if result.err != nil && firstErr == nil {
			firstErr = result.err
			cancel(result.err)
		}
		if (result.terminalTask != nil || result.terminalGate != nil) && !terminalTriggered {
			terminalTriggered = true
			cancel(errParallelTerminal)
		}
		if (result.status == journal.BranchFailed || result.status == journal.BranchTimedOut) && p.FailurePolicy == apiv1.BranchFailFast && !failFast {
			failFast = true
			cancel(errParallelFailFast)
		}
		if (firstErr != nil || terminalTriggered || failFast) && next < len(queue) {
			if err := cancelQueued(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		for firstErr == nil && !terminalTriggered && !failFast && !draining && next < len(queue) && running < limit {
			if err := launch(queue[next]); err != nil {
				firstErr = err
				cancel(err)
				break
			}
			next++
		}
	}

	if firstErr != nil {
		return concurrentParallelResult{}, firstErr
	}
	if draining {
		return concurrentParallelResult{parallel: par, paused: true}, nil
	}

	mergedCompleted := cloneStageOutputs(baseCompleted)
	lastStage, lastResult := baseLastStage, baseLastResult
	for _, outcome := range outcomes {
		if outcome == nil {
			continue
		}
		for stage, outputs := range outcome.completed {
			mergedCompleted.put(stage, outputs)
		}
		if outcome.lastStage != "" {
			lastStage, lastResult = outcome.lastStage, outcome.lastResult
		}
	}

	var terminalTarget string
	var terminalTask *parallelTaskTerminal
	var terminalGate *parallelGateTerminal
	for _, outcome := range outcomes {
		if outcome != nil && outcome.terminalTarget != "" {
			terminalTarget = outcome.terminalTarget
			terminalTask = outcome.terminalTask
			terminalGate = outcome.terminalGate
			break
		}
	}
	target, runJoin := par.route()
	if terminalTarget != "" {
		target, runJoin = terminalTarget, false
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
		target:       target,
		runJoin:      runJoin,
		lastStage:    lastStage,
		lastResult:   lastResult,
		pointers:     mergedPointers,
		completed:    mergedCompleted,
		parallel:     par,
		terminalTask: terminalTask,
		terminalGate: terminalGate,
	}, nil
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
) parallelBranchResult {
	result := parallelBranchResult{
		index:      branch.id - 1,
		lastStage:  baseLastStage,
		lastResult: baseLastResult,
		completed:  branchStageOutputs(baseCompleted, history, in.Machine),
		pointers:   append([]apiv1.ContextPointer(nil), branch.pointers...),
		artifacts:  branch.artifacts,
		produced:   branch.produced,
		failed:     branch.failed,
		noOutput:   branch.noOutput,
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
	gateEval := &gate.Evaluator{
		Automated:      r.cfg.Automated,
		Journal:        branchJournal,
		MaxRepasses:    int(in.RunControls.MaxRepasses),
		Attempts:       gateRepassSeed(history),
		RepassAttempts: targetRepassSeed(history),
		IsReentry: func(target string) bool {
			return visitedStages[target]
		},
		LastDiffDigest: gateDiffSeed(history),
	}
	state := branch.machine
	if state == "" {
		state = branch.start
	}
	branchRecorded := true
	if lastStage, lastResult, ok := lastFinishedSubject(history); ok {
		result.lastStage, result.lastResult = lastStage, lastResult
	}
	if rebound := lastWorkspaceBranch(history, in.Machine, r.branchNamespaceFor(in.Gaggle)); rebound != "" {
		workspaceBranch = rebound
	}
	if retryTarget, pending := pendingRetryTarget(history, in.Machine, result.lastStage, result.lastResult); pending {
		state = retryTarget
	}

	var replayTask *apiv1.ResultEnvelope
	var replayGate *gate.Result
	var replayGateEvent *journal.Event
	startAttempt := int32(1)
	var firstClass journal.AttemptClass
	var committedWorkOnInfra bool
	var resumeAccounting *resumeRetryAccounting
	if boundary, ok := lastParallelBoundary(history); ok {
		if task, isTask := in.Machine.Task(state); isTask {
			switch {
			case boundary.Type == journal.EventStageFinished && boundary.Stage == state && !isInterruptedAttemptMarker(boundary):
				replayed := result.lastResult
				replayTask = &replayed
			case boundary.Type == journal.EventStageStarted && boundary.Stage == state:
				attempt := boundary.Attempt
				if attempt == 0 {
					attempt = 1
				}
				errorDetail := &journal.ErrorDetail{Code: interruptedAttemptErrorCode, Message: "attempt was in flight when the runner was interrupted"}
				runnerDetail := map[string]any{interruptedAttemptMarkerKey: true}
				if task.Type == apiv1.TaskAgentic {
					limits, err := workflow.TaskLimits(in.Machine, task)
					if err != nil {
						result.status, result.err = journal.BranchFailed, err
						return result
					}
					if usageBudgetConfigured(limits) {
						interrupted := interruptedStageBudgetFailure(limits)
						replayTask = &interrupted
						errorDetail = errorDetailFrom(interrupted)
						runnerDetail = nil
					}
				}
				if err := branchJournal.Append(journal.Event{
					Type:         journal.EventStageFinished,
					Stage:        state,
					Attempt:      attempt,
					AttemptClass: journal.AttemptInfra,
					Status:       string(apiv1.ResultFailure),
					Error:        errorDetail,
					Runner:       runnerDetail,
				}); err != nil {
					result.status, result.err = journal.BranchFailed, err
					return result
				}
				if replayTask == nil {
					startAttempt = int32(attempt) + 1
					firstClass = journal.AttemptInfra
					committedWorkOnInfra = infraFailedAttemptCommittedWork(history, state, attempt)
					resumeAccounting = &resumeRetryAccounting{
						policyAttempts:            policyAttemptsBefore(history, state, attempt),
						infrastructureFailures:    infrastructureFailuresBefore(history, state, attempt),
						replacementConsumesPolicy: boundary.AttemptClass != journal.AttemptInfra,
					}
				}
			}
		} else if _, isGate := in.Machine.Gate(state); isGate &&
			boundary.Type == journal.EventGateEvaluated && boundary.Gate == state {
			gr := gateResultFromEvent(boundary)
			replayGate = &gr
			event := boundary
			replayGateEvent = &event
		}
	}

	for {
		if ctx.Err() != nil {
			result.status = journal.BranchCancelled
			result.paused = parallelDrainCancellation(ctx)
			return result
		}
		// Exceeding branchTimeoutSeconds terminates at the next stage
		// boundary (never mid-stage — see the field's doc comment), so this
		// is a plain check here, not a context deadline: a stage that is
		// already running finishes on its own, exactly like the sequential
		// path (run.go).
		if deadline := branch.deadline(par.spec.BranchTimeoutSeconds); !deadline.IsZero() && !time.Now().Before(deadline) {
			result.status = journal.BranchTimedOut
			result.failed = true
			return result
		}
		if stepBudget.Add(1) > int64(r.maxSteps) {
			result.status = journal.BranchFailed
			result.err = fmt.Errorf("runner: run %q exceeded max steps (%d): possible loop", in.RunID, r.maxSteps)
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
				replayTask = nil
			} else {
				stageResult, produced, err = r.runTask(
					ctx, branchJournal, in, ex, task, branch.id,
					branchContextPointers(basePointers, result.pointers),
					result.lastResult, result.completed, nil, startAttempt, firstClass,
					"", workspaceBranch, nil, &branchRecorded, committedWorkOnInfra, resumeAccounting,
				)
				startAttempt = 1
				firstClass = ""
				resumeAccounting = nil
			}
			if err != nil {
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
			if stageResult.Status != apiv1.ResultFailure || !task.ContinueOnError {
				if rebound := rebindWorkspaceBranch(task, stageResult, r.branchNamespaceFor(in.Gaggle)); rebound != "" {
					workspaceBranch = rebound
				}
			}
			if ctx.Err() != nil {
				result.status = journal.BranchCancelled
				result.paused = parallelDrainCancellation(ctx)
				return result
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
					Error: &journal.ErrorDetail{Code: "worktree_remove_failed", Message: removeErr.Error()},
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
			if retry {
				if !replayed && gr.VerdictArtifact != nil {
					result.pointers = append(result.pointers, apiv1.ContextPointer{
						Name: g.Name + ".verdict", Integrity: gr.VerdictArtifact.Integrity, Artifact: gr.VerdictArtifact,
					})
					result.artifacts++
					result.produced = true
				}
				state = retryTarget
				continue
			}
			if !replayed && gr.VerdictArtifact != nil {
				result.pointers = append(result.pointers, apiv1.ContextPointer{
					Name: g.Name + ".verdict", Integrity: gr.VerdictArtifact.Integrity, Artifact: gr.VerdictArtifact,
				})
				result.artifacts++
				result.produced = true
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

func parallelBranchEvents(events []journal.Event, parallel string, branch int) []journal.Event {
	start := 0
	for i, event := range events {
		if event.Type == journal.EventParallelStarted && event.Parallel == parallel {
			start = i + 1
		}
	}
	out := make([]journal.Event, 0)
	for _, event := range events[start:] {
		if event.Branch == branch {
			out = append(out, event)
		}
	}
	return out
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
