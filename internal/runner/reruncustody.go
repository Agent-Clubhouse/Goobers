package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

// ErrRerunCustodyUnavailable refuses a rerun whose preserved workspace cannot be verified.
var ErrRerunCustodyUnavailable = errors.New("runner: rerun custody unavailable")

// PreflightRerunStage checks the retained execution and workspace before the
// daemon durably accepts an operator request or reacquires its claims. The
// runner repeats the custody check while holding the run's journal writer.
func (r *Runner) PreflightRerunStage(ctx context.Context, in RerunStageInput) error {
	if in.Machine == nil {
		return fmt.Errorf("runner: Machine is required")
	}
	isGate, err := validateRerunTarget(in.Machine, in.Stage)
	if err != nil {
		return err
	}
	rd, err := journal.OpenRead(filepath.Join(r.cfg.RunsDir, in.RunID))
	if err != nil {
		return err
	}
	id, err := rd.Identity()
	if err != nil {
		return err
	}
	if id.WorkflowDigest != in.Machine.Digest() || id.GooberDigest != "" && id.GooberDigest != in.GooberDigest {
		return fmt.Errorf("runner: pinned workflow/goober identity mismatch (WF-016)")
	}
	events, err := rd.Events()
	if err != nil {
		return err
	}
	if err := validateTerminalGeneration(in.RunID, events, in.ExpectedTerminalSeq); err != nil {
		return err
	}
	seed, err := rerunSeedEvents(events, in.Stage, isGate)
	if err != nil {
		return err
	}
	if err := verifyRecordedSelections(seed, in.Machine); err != nil {
		return err
	}
	parallel, parallelStart, err := pendingParallel(seed, in.Machine)
	if err != nil {
		return err
	}
	branch := 0
	if parallel != nil {
		owner := rerunOwnerBranch(parallel, in.Machine, in.Stage)
		if err := refuseSkippedBranchRerun(parallel, owner); err != nil {
			return err
		}
		if owner != nil && !parallelUsesDispatcher(in.Machine, parallel.spec) {
			branch = owner.id
		}
	}
	start := StartInput{RunID: in.RunID, Machine: in.Machine, Gaggle: id.Gaggle, Child: id.Child, RepoRef: in.RepoRef}
	// Child custody has its own immutable admission and authorization checks.
	start, err = r.restoreExecutionWorkspace(ctx, rd, id, start, events, parallel, parallelStart, branch)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRerunCustodyUnavailable, err)
	}
	_, err = r.rerunWorkspaceBranch(ctx, start, events, in.Stage)
	return err
}

func rerunRequiresWritableWorkspace(machine *workflow.Machine, events []journal.Event, stage string) bool {
	writable := func(name string) bool {
		if task, ok := machine.Task(name); ok {
			return taskWorkspaceMode(task).IsWritableRepo()
		}
		if gate, ok := machine.Gate(name); ok {
			return gate.Evaluator == apiv1.EvaluatorAgentic && gateWorkspaceMode(gate).IsWritableRepo()
		}
		return false
	}
	if writable(stage) {
		return true
	}
	for _, event := range events {
		if event.Type == journal.EventStageStarted && writable(event.Stage) || event.Type == journal.EventGateStarted && writable(event.Gate) {
			return true
		}
	}
	return false
}

func (r *Runner) rerunWorkspaceBranch(ctx context.Context, in StartInput, events []journal.Event, stage string) (string, error) {
	branch := lastWorkspaceBranch(events, in.Machine, r.branchNamespaceFor(in.Gaggle))
	if in.Child != nil {
		return in.WorkspaceBranch, nil
	}
	if !rerunRequiresWritableWorkspace(in.Machine, events, stage) {
		return branch, nil
	}
	if branch == "" {
		branch = providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID)
	}
	if r.cfg.Worktrees == nil {
		return "", ErrRerunCustodyUnavailable
	}
	url, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err == nil {
		err = r.cfg.Worktrees.VerifyRunBranch(ctx, url, branch, r.cfg.PinnedWorkspace)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRerunCustodyUnavailable, err)
	}
	return branch, nil
}
