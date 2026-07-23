package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// RerunStageInput identifies one agentic task or reviewer gate in an escalated
// run to execute again with an operator-supplied instruction addendum.
type RerunStageInput struct {
	RunID               string
	Machine             *workflow.Machine
	RepoRef             apiv1.RepoRef
	Stage               string
	Actor               string
	InstructionAddendum string
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

	isGate, err := validateRerunTarget(in.Machine, in.Stage)
	if err != nil {
		return Result{}, err
	}

	dir := filepath.Join(r.cfg.RunsDir, in.RunID)
	registrar, scrubber := journal.DefaultScrubber()
	jr, _, err := journal.Recover(dir, journal.WithScrubber(scrubber))
	if err != nil {
		return Result{}, fmt.Errorf("runner: recover run %q for stage rerun: %w", in.RunID, err)
	}
	defer func() { _ = jr.Close() }()

	return r.withActiveRun(ctx, in.RunID, jr, func(ctx context.Context) (Result, error) {
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
		if id.GooberDigest != "" && id.GooberDigest != in.Machine.GooberDigest() {
			return Result{}, fmt.Errorf("runner: run %q is pinned to goober digest %q, cannot rerun against %q (WF-016)", in.RunID, id.GooberDigest, in.Machine.GooberDigest())
		}
		events, err := rd.Events()
		if err != nil {
			return Result{}, fmt.Errorf("runner: read events for run %q: %w", in.RunID, err)
		}
		seedEvents, err := rerunSeedEvents(events, in.Stage, isGate)
		if err != nil {
			return Result{}, err
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
			RunID:   in.RunID,
			Machine: in.Machine,
			Gaggle:  id.Gaggle,
			Trigger: id.Trigger,
			RepoRef: in.RepoRef,
			Item:    item,
		}
		seed := walkSeed{pointers: reconstructPointers(seedEvents)}
		seed.lastStage, seed.lastResult, _ = lastFinishedSubject(seedEvents)
		seed.workspaceBranch = lastWorkspaceBranch(seedEvents, in.Machine, r.branchNamespaceFor(id.Gaggle))
		gateAttempts, gateDiffDigests := gateRepassSeed(seedEvents), gateDiffSeed(seedEvents)
		gateAttempts = resetRerunGateSeeds(in.Machine, rerun, gateAttempts, gateDiffDigests)

		ctx, span := r.startRunSpan(ctx, startIn)
		defer span.End()
		setStalledAttemptContext(ctx)

		result, err := r.walk(ctx, jr, startIn, in.Stage, nil, rerun, gateAttempts, gateDiffDigests, registrar, seed)
		if err != nil {
			span.Fail(err)
			return result, err
		}
		completeRunSpan(span, result)
		return result, nil
	})
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
