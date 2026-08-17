package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/workflow"
)

// RerunStageInput identifies one agentic task or reviewer gate in an escalated
// run to execute again with an operator-supplied instruction addendum.
type RerunStageInput struct {
	RunID               string
	Machine             *workflow.Machine
	GooberDigest        string
	RepoRef             apiv1.RepoRef
	Stage               string
	Actor               string
	InstructionAddendum string
	// ExpectedTerminalSeq binds the request to the escalation that was
	// inspected before the operator action was accepted.
	ExpectedTerminalSeq uint64
}

type rerunContext struct {
	stage                  string
	attempt                int
	requestAttempt         int
	policyAttempts         int32
	infrastructureFailures int32
	gateAttempts           int
	instructionAddendum    string
}

// RerunStage re-enters an escalated run at one agentic task or reviewer gate.
// The workflow definition remains pinned and unchanged; the operator, addendum,
// target, and attempt are recorded before the invocation starts.
func (r *Runner) RerunStage(ctx context.Context, in RerunStageInput) (Result, error) {
	if in.RunID == "" {
		return Result{}, fmt.Errorf("runner: RunID is required")
	}
	if in.Machine == nil {
		return Result{}, fmt.Errorf("runner: Machine is required")
	}
	if in.Stage == "" {
		return Result{}, fmt.Errorf("runner: Stage is required")
	}
	actor := strings.TrimSpace(in.Actor)
	if actor == "" {
		return Result{}, fmt.Errorf("runner: Actor is required")
	}
	addendum := strings.TrimSpace(in.InstructionAddendum)
	if addendum == "" {
		return Result{}, fmt.Errorf("runner: InstructionAddendum is required")
	}
	if in.ExpectedTerminalSeq == 0 {
		return Result{}, fmt.Errorf("runner: expected terminal sequence is required")
	}

	isGate, err := validateRerunTarget(in.Machine, in.Stage)
	if err != nil {
		return Result{}, err
	}

	dir := filepath.Join(r.cfg.RunsDir, in.RunID)
	registrar, scrubber := journal.DefaultScrubber()
	jr, _, err := journal.Recover(dir, journal.WithScrubber(scrubber), journal.WithAppendObserver(r.cfg.JournalAdvanced))
	if err != nil {
		return Result{}, fmt.Errorf("runner: recover run %q for stage rerun: %w", in.RunID, err)
	}
	defer func() { _ = jr.Close() }()

	return r.withActiveRun(ctx, in.RunID, jr, func(ctx context.Context) (result Result, retErr error) {
		rd, err := journal.OpenRead(dir)
		if err != nil {
			return Result{}, fmt.Errorf("runner: open run %q for stage rerun: %w", in.RunID, err)
		}
		id, err := rd.Identity()
		if err != nil {
			return Result{}, fmt.Errorf("runner: read identity for run %q: %w", in.RunID, err)
		}
		phase, err := rd.Phase()
		if err != nil {
			return Result{}, fmt.Errorf("runner: reconstruct phase for run %q: %w", in.RunID, err)
		}
		if phase != journal.PhaseEscalated {
			return Result{}, fmt.Errorf("runner: run %q has phase %s, not escalated", in.RunID, phase)
		}
		if id.WorkflowDigest == "" || id.WorkflowDigest != in.Machine.Digest() {
			return Result{}, fmt.Errorf("runner: run %q is pinned to workflow digest %q, cannot rerun against %q (WF-016)", in.RunID, id.WorkflowDigest, in.Machine.Digest())
		}
		if id.GooberDigest != "" && id.GooberDigest != in.GooberDigest {
			return Result{}, fmt.Errorf("runner: run %q is pinned to goober digest %q, cannot rerun against %q (WF-016)", in.RunID, id.GooberDigest, in.GooberDigest)
		}
		if err := runcontrol.ValidatePinned(id.RunControls); err != nil {
			return Result{}, fmt.Errorf("runner: invalid pinned run controls: %w", err)
		}
		runControls, err := r.resolveRunControls(id.RunControls)
		if err != nil {
			return Result{}, fmt.Errorf("runner: resolve pinned run controls: %w", err)
		}
		events, err := rd.Events()
		if err != nil {
			return Result{}, fmt.Errorf("runner: read events for run %q: %w", in.RunID, err)
		}
		if err := validateTerminalGeneration(in.RunID, events, in.ExpectedTerminalSeq); err != nil {
			return Result{}, err
		}
		seedEvents, err := rerunSeedEvents(events, in.Stage, isGate)
		if err != nil {
			return Result{}, err
		}
		activeParallel, parallelStart := pendingParallel(seedEvents, in.Machine)
		if activeParallel != nil {
			jr.SetBranchCursors(activeParallel.cursors())
			if owner := rerunOwnerBranch(activeParallel, in.Machine, in.Stage); owner != nil {
				jr.SetBranch(owner.id)
			}
		}
		item, err := resumeItem(rd, id)
		if err != nil {
			return Result{}, fmt.Errorf("runner: rerun item snapshot for run %q: %w", in.RunID, err)
		}

		attempt := nextRerunAttempt(events, in.Stage, isGate)
		rerun := &rerunContext{
			stage:               in.Stage,
			attempt:             attempt,
			requestAttempt:      attempt,
			instructionAddendum: addendum,
		}
		if err := jr.Append(journal.Event{
			Type:                journal.EventStageRerunRequested,
			Stage:               in.Stage,
			Attempt:             rerun.attempt,
			AttemptClass:        journal.AttemptHuman,
			Actor:               actor,
			InstructionAddendum: addendum,
		}); err != nil {
			return Result{}, fmt.Errorf("runner: journal stage rerun for %q: %w", in.Stage, err)
		}

		startIn := StartInput{
			RunID:        in.RunID,
			Machine:      in.Machine,
			GooberDigest: in.GooberDigest,
			Gaggle:       id.Gaggle,
			Trigger:      id.Trigger,
			RepoRef:      in.RepoRef,
			Item:         item,
			RunControls:  runControls,
		}
		_, err = r.acquirePinnedWorkspace(ctx, jr, &startIn)
		if err != nil {
			if interrupted, ok, interruptErr := r.finishStalledRequest(ctx, in.RunID, jr, in.Stage, 0); ok {
				return interrupted, interruptErr
			}
			return Result{}, err
		}
		defer func() {
			if retErr != nil || result.Phase != "" && result.Phase != journal.PhaseRunning {
				retErr = errors.Join(retErr, r.releasePinnedWorkspace(in.RunID))
			}
		}()
		pointerEvents := seedEvents
		if activeParallel != nil {
			pointerEvents = seedEvents[:parallelStart]
		}
		ws := newWalkState(jr, startIn, registrar, in.Stage)
		ws.pointers = reconstructPointers(pointerEvents, in.Machine)
		ws.completed = reconstructStageOutputs(seedEvents, in.Machine)
		ws.parallel = activeParallel
		ws.fanIn = rerunFanIn(seedEvents, in.Machine, in.Stage)
		if activeParallel != nil {
			ws.parallelRootPointers = append([]apiv1.ContextPointer(nil), ws.pointers...)
		}
		ws.lastStage, ws.lastResult, _ = lastFinishedSubject(seedEvents)
		ws.lastResult = discardToleratedFailureOutputs(in.Machine, ws.lastStage, ws.lastResult)
		ws.workspaceBranch = lastWorkspaceBranch(seedEvents, in.Machine, r.branchNamespaceFor(id.Gaggle))
		ws.branchRecorded = hasRunBranchRef(events)
		gateAttempts, gateDiffDigests := gateRepassSeed(seedEvents), gateDiffSeed(seedEvents)
		gateAttempts = resetRerunGateSeeds(in.Machine, rerun, gateAttempts, gateDiffDigests)
		ws.gateAttempts, ws.repassAttempts, ws.gateDiffDigests = gateAttempts, targetRepassSeed(seedEvents), gateDiffDigests
		ws.rerun = rerun

		ctx, span := r.startRunSpan(ctx, startIn)
		defer span.End()
		setStalledAttemptContext(ctx)

		result, err = r.walk(ctx, ws)
		if err != nil {
			span.Fail(err)
			return result, err
		}
		completeRunSpan(span, result)
		return result, nil
	})
}

// rerunOwnerBranch resolves which branch a rerun request should be attributed
// to. activeParallel.current() is only "whichever branch's EventBranchStarted
// happened to journal last" (pendingParallel's reconstruction) — in a wide
// (maxConcurrentBranches > 1) parallel that is not necessarily the branch
// containing stage, e.g. a sibling can start after the paused branch already
// began. Resolving the owner explicitly keeps the rerun-request event (and
// jr.SetBranch below) attributed to the branch that actually owns stage,
// falling back to current() only if resolution can't place it at all
// (defensive; should not happen given compile-time rule 1's disjoint branch
// subgraphs).
func rerunOwnerBranch(activeParallel *parallelExec, machine *workflow.Machine, stage string) *branchState {
	owner := activeParallel.current()
	if name := branchOwningState(machine, activeParallel.spec, stage); name != "" {
		if b := activeParallel.branch(name); b != nil {
			owner = b
		}
	}
	return owner
}

// branchOwningState returns the name of the branch within spec whose subgraph
// reaches state, walking each branch's declared states from its Start via
// machine.Outgoing. Compile-time rule 1 (parallel.go's parallelBodyProblems)
// guarantees branch subgraphs are disjoint and none of them include the
// parallel's own Join, so the first branch whose walk reaches state is the
// only one that can. Returns "" if no branch reaches it (state is outside
// this parallel, or the parallel has already fully settled).
func branchOwningState(machine *workflow.Machine, spec apiv1.Parallel, state string) string {
	for _, branch := range spec.Branches {
		seen := make(map[string]bool)
		queue := []string{branch.Start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if cur == "" || cur == spec.Join || seen[cur] {
				continue
			}
			seen[cur] = true
			if cur == state {
				return branch.Name
			}
			if !machine.Has(cur) {
				continue
			}
			queue = append(queue, machine.Outgoing(cur)...)
		}
	}
	return ""
}

func validateRerunTarget(machine *workflow.Machine, stage string) (bool, error) {
	if task, ok := machine.Task(stage); ok {
		if task.Type != apiv1.TaskAgentic {
			return false, fmt.Errorf("runner: stage %q is deterministic; instruction addenda require an agentic stage", stage)
		}
		return false, nil
	}
	if gate, ok := machine.Gate(stage); ok {
		if gate.Evaluator != apiv1.EvaluatorAgentic {
			return false, fmt.Errorf("runner: stage %q is not an agentic reviewer gate", stage)
		}
		return true, nil
	}
	return false, fmt.Errorf("runner: stage %q is not defined by workflow %q", stage, machine.Def.Name)
}

func rerunSeedEvents(events []journal.Event, stage string, isGate bool) ([]journal.Event, error) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if (!isGate && event.Type == journal.EventStageStarted && event.Stage == stage) ||
			(isGate && event.Type == journal.EventGateStarted && event.Gate == stage) {
			return events[:i], nil
		}
	}
	return nil, fmt.Errorf("runner: stage %q has not previously run", stage)
}

func nextRerunAttempt(events []journal.Event, stage string, isGate bool) int {
	attempt := 1
	for _, event := range events {
		if (!isGate && event.Type == journal.EventStageStarted && event.Stage == stage) ||
			(isGate && event.Type == journal.EventGateStarted && event.Gate == stage) {
			attempt++
		}
	}
	return attempt
}

func pendingRerun(events []journal.Event, machine *workflow.Machine) (*rerunContext, []journal.Event, error) {
	for i := len(events) - 1; i >= 0; i-- {
		request := events[i]
		if request.Type != journal.EventStageRerunRequested {
			continue
		}
		if request.Attempt < 1 || request.AttemptClass != journal.AttemptHuman ||
			strings.TrimSpace(request.Actor) == "" || strings.TrimSpace(request.InstructionAddendum) == "" {
			return nil, nil, fmt.Errorf("pending rerun request for %q is incomplete", request.Stage)
		}
		isGate := false
		if task, ok := machine.Task(request.Stage); ok {
			if task.Type != apiv1.TaskAgentic {
				return nil, nil, fmt.Errorf("pending rerun target %q is not agentic", request.Stage)
			}
		} else if gate, ok := machine.Gate(request.Stage); ok {
			if gate.Evaluator != apiv1.EvaluatorAgentic {
				return nil, nil, fmt.Errorf("pending rerun target %q is not an agentic reviewer gate", request.Stage)
			}
			isGate = true
		} else {
			return nil, nil, fmt.Errorf("pending rerun target %q is not defined by workflow %q", request.Stage, machine.Def.Name)
		}
		for _, event := range events[i+1:] {
			if (!isGate && event.Type == journal.EventStageFinished && event.Stage == request.Stage && !isInterruptedAttemptMarker(event)) ||
				(isGate && event.Type == journal.EventGateEvaluated && event.Gate == request.Stage) {
				return nil, nil, nil
			}
		}
		seed, err := rerunSeedEvents(events[:i], request.Stage, isGate)
		if err != nil {
			return nil, nil, err
		}
		policyAttempts, infrastructureFailures := pendingRerunRetryUsage(events[i+1:], request.Stage)
		return &rerunContext{
			stage:                  request.Stage,
			attempt:                pendingRerunAttempt(events[i+1:], request),
			requestAttempt:         request.Attempt,
			policyAttempts:         policyAttempts,
			infrastructureFailures: infrastructureFailures,
			gateAttempts:           pendingRerunGateAttempts(events[i+1:], request.Stage),
			instructionAddendum:    request.InstructionAddendum,
		}, seed, nil
	}
	return nil, nil, nil
}

// pendingRerunRetryUsage restores the counters runTask held in memory. The
// infra-class starts preserve counts from journals written before failures
// carried retryFailureClass; max avoids counting the same failure twice.
func pendingRerunRetryUsage(events []journal.Event, stage string) (int32, int32) {
	var policyAttempts, taggedInfrastructureFailures, infrastructureStarts int32
	for _, event := range events {
		if event.Stage != stage {
			continue
		}
		if event.Type == journal.EventStageStarted {
			switch event.AttemptClass {
			case journal.AttemptHuman, journal.AttemptPolicy:
				policyAttempts++
			case journal.AttemptInfra:
				infrastructureStarts++
			}
		}
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error" &&
			event.Runner[retryFailureClassKey] == string(journal.AttemptInfra) {
			taggedInfrastructureFailures++
		}
	}
	if taggedInfrastructureFailures < infrastructureStarts {
		taggedInfrastructureFailures = infrastructureStarts
	}
	return policyAttempts, taggedInfrastructureFailures
}

func pendingRerunAttempt(events []journal.Event, request journal.Event) int {
	attempt := request.Attempt
	for _, event := range events {
		if event.Stage == request.Stage && isInterruptedAttemptMarker(event) && event.Attempt >= attempt {
			attempt = event.Attempt + 1
		}
	}
	return attempt
}

func pendingRerunGateAttempts(events []journal.Event, gate string) int {
	attempts := 0
	for _, event := range events {
		if event.Type == journal.EventGateStarted && event.Gate == gate {
			attempts++
		}
	}
	return attempts
}

func resetRerunGateSeeds(machine *workflow.Machine, rerun *rerunContext, attempts map[string]int, digests map[string]string) map[string]int {
	if rerun == nil {
		return attempts
	}
	if gate, ok := machine.Gate(rerun.stage); !ok || gate.Evaluator != apiv1.EvaluatorAgentic {
		return attempts
	}
	if attempts == nil && rerun.gateAttempts > 0 {
		attempts = make(map[string]int)
	}
	if attempts != nil {
		attempts[rerun.stage] = rerun.gateAttempts
	}
	if digests != nil {
		delete(digests, rerun.stage)
	}
	return attempts
}
