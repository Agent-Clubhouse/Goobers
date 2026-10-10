package runner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
)

// ParentForkResultFunc captures/imports a stopped branch through its host owner.
type ParentForkResultFunc func(context.Context, OwnedJournalRecorder, spec.ResultRequest, *spec.Source) (spec.Source, error)

// ParentForkResultKind marks an immutable host-captured branch result.
const ParentForkResultKind = "isolated.parent.fork.result"

// ParentForkResult is immutable output, separate from the mutable checkout and
// its later terminal archive. Failed branches retain results for diagnosis too.
type ParentForkResult struct {
	Sequence uint64               `json:"sequence"`
	Plan     journal.Ref          `json:"plan"`
	Branch   int                  `json:"branch"`
	Status   journal.BranchStatus `json:"status"`
	Source   spec.Source          `json:"source"`
}

// ParentForkResults validates host receipts against the exact original plan.
func ParentForkResults(reader *journal.Reader, plan ParentForkPlan, reference journal.Ref) ([]ParentForkResult, error) {
	events, err := reader.Events()
	if err != nil {
		return nil, err
	}
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return nil, err
	}
	state := states[plan.Sequence]
	if state == nil || state.reference != reference {
		return nil, errors.New("parallel result plan unavailable")
	}
	results := []ParentForkResult{}
	seen := map[int]bool{}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ParentForkResultKind {
			continue
		}
		var value ParentForkResult
		data, err := json.Marshal(event.Runner["result"])
		if err != nil || len(data) > 8192 || json.Unmarshal(data, &value) != nil {
			return nil, errors.New("invalid parallel result receipt")
		}
		if value.Sequence != plan.Sequence {
			continue
		}
		if !validParentForkResult(value, event, state) || seen[value.Branch] {
			return nil, errors.New("parallel result receipt changed ownership")
		}
		if _, err := reader.ArtifactBytesBounded(value.Source.Metadata, maxParentForkPlanBytes); err != nil {
			return nil, err
		}
		if _, err := reader.ArtifactBytesBounded(value.Source.Bundle, maxParentForkBundleBytes); err != nil {
			return nil, err
		}
		seen[value.Branch] = true
		results = append(results, value)
	}
	return results, nil
}

func validForkResultStatus(status journal.BranchStatus) bool {
	switch status {
	case journal.BranchSucceeded, journal.BranchFailed, journal.BranchTimedOut, journal.BranchCancelled, journal.BranchNoOutput:
		return true
	}
	return false
}

// Root dispatcher calls this only after the branch goroutine has returned.
// Custody projections must confirm no live parent or unresolved child writer.
func (r *Runner) captureParallelForkResult(ctx context.Context, run *journal.Run, in StartInput, parallel apiv1.Parallel, result parallelBranchResult, requirePrevious bool) error {
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		return err
	}
	request, previous, err := parallelForkResultRequest(reader, in, parallel, result)
	if err != nil {
		return err
	}
	if requirePrevious && previous == nil {
		return errors.New("settled parallel branch lacks recorded workspace result")
	}
	if r.cfg.PrepareParentForkResult == nil {
		return errors.New("parallel result service unavailable")
	}
	captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	source, err := r.cfg.PrepareParentForkResult(captureCtx, run, request, previous)
	if err != nil {
		return err
	}
	if previous != nil {
		if source != *previous {
			return errors.New("parallel result changed on replay")
		}
		return nil
	}
	value := ParentForkResult{Sequence: request.Sequence, Plan: request.Plan, Branch: request.Branch, Status: result.status, Source: source}
	return run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Parallel: parallel.Name, Branch: request.Branch, Runner: map[string]any{"kind": ParentForkResultKind, "result": value}})
}

func parallelForkResultRequest(reader *journal.Reader, in StartInput, parallel apiv1.Parallel, result parallelBranchResult) (spec.ResultRequest, *spec.Source, error) {
	var empty spec.ResultRequest
	events, err := reader.Events()
	if err != nil {
		return empty, nil, err
	}
	started, err := parallelForkBoundary(events, parallel)
	if err != nil {
		return empty, nil, err
	}
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return empty, nil, err
	}
	state := states[started.Seq]
	branch := result.index + 1
	if state == nil || branch <= 0 || branch > len(state.plan.Workspaces) || !state.ready[branch] || !validForkResultStatus(result.status) {
		return empty, nil, errors.New("invalid parallel result branch")
	}
	owner := state.plan.Workspaces[branch-1]
	if _, err := readParentWorkspaceState(events, in.RunID, owner.Branch); err != nil {
		return empty, nil, err
	}
	projection, err := journal.ProjectChildWaits(events)
	if err != nil {
		return empty, nil, err
	}
	if _, waiting := projection.Waits[branch]; waiting {
		return empty, nil, errors.New("parallel result has unresolved child custody")
	}
	values, err := ParentForkResults(reader, state.plan, state.reference)
	if err != nil {
		return empty, nil, err
	}
	request := spec.ResultRequest{Request: forkSourceRequest(in, started), Plan: state.reference, Seed: state.plan.Source, Branch: branch, Status: result.status, Custody: owner}
	for _, value := range values {
		if value.Branch != branch {
			continue
		}
		if value.Status != result.status {
			return empty, nil, errors.New("parallel result status changed on replay")
		}
		saved := value.Source
		return request, &saved, nil
	}
	return request, nil, nil
}

func (r *Runner) settleParallelRuntimeBranch(ctx context.Context, run *journal.Run, in StartInput, par *parallelExec, runtime parallelRuntime, result parallelBranchResult) error {
	if len(runtime.forks) != 0 {
		if err := r.captureParallelForkResult(ctx, run, in, par.spec, result, false); err != nil {
			return err
		}
	}
	return settleParallelChildBranch(ctx, run, par, runtime.capacity, result)
}

func (r *Runner) verifyParallelForkResults(ctx context.Context, run *journal.Run, in StartInput, parallel apiv1.Parallel, runtime parallelRuntime, outcomes []*parallelBranchResult) error {
	if len(runtime.forks) == 0 {
		return nil
	}
	for _, outcome := range outcomes {
		if outcome == nil {
			return errors.New("parallel fork missing branch outcome")
		}
		if err := r.captureParallelForkResult(ctx, run, in, parallel, *outcome, true); err != nil {
			return err
		}
	}
	return nil
}

func validParentForkResult(value ParentForkResult, event journal.Event, state *parentForkState) bool {
	return value.Plan == state.reference && value.Branch > 0 && value.Branch <= len(state.plan.Workspaces) && event.Branch == value.Branch && event.Parallel == state.plan.Parallel && event.Seq > state.plannedAt && state.ready[value.Branch] && validForkResultStatus(value.Status) && value.Source.Metadata.Integrity == apiv1.IntegrityTrusted && value.Source.Bundle.Integrity == apiv1.IntegrityTrusted
}
