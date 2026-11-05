package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/goobers/goobers/internal/workspaceauthority"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// ChildWorkspaceInputName uses the shared workspaceauthority.ChildWorkspaceInputName contract.
const ChildWorkspaceInputName = workspaceauthority.ChildWorkspaceInputName

// ChildWorkspaceAdmission uses the shared workspaceauthority.ChildWorkspaceAdmission contract.
type ChildWorkspaceAdmission = workspaceauthority.ChildWorkspaceAdmission

type childRunWorkspace struct {
	worktree *worktree.Worktree
	stage    sync.RWMutex
}

func (r *Runner) prepareChildWorkspaceStart(ctx context.Context, in *StartInput, inputs map[string][]byte, integrity map[string]apiv1.Integrity) error {
	if err := r.validateChildExecution(*in); err != nil {
		return err
	}
	if err := pinChildCredentials(in, inputs, integrity); err != nil {
		return err
	}
	if err := r.validateChildWorkspacePlan(*in); err != nil {
		return err
	}
	if in.ChildWorkspace == nil {
		return nil
	}
	admission := *in.ChildWorkspace
	in.ChildWorkspace = &admission
	if err := r.adoptChildWorkspace(ctx, in); err != nil {
		return err
	}
	data, err := json.Marshal(admission)
	if err != nil {
		return err
	}
	inputs[childWriterPolicyInput] = []byte(childWriterPolicy)
	integrity[childWriterPolicyInput] = apiv1.IntegrityTrusted
	inputs[ChildWorkspaceInputName] = data
	integrity[ChildWorkspaceInputName] = apiv1.IntegrityTrusted
	return nil
}

// Child execution requires the admitted managed fork for every repo-backed
// stage. The compiler validates static branch workspace modes and dependencies.
func (r *Runner) validateChildWorkspacePlan(in StartInput) error {
	if in.Child == nil {
		if in.ChildWorkspace != nil {
			return fmt.Errorf("runner: child workspace requires pinned child lineage")
		}
		return nil
	}
	if in.ChildWorkspace == nil {
		if childMachineUsesRepo(in.Machine) {
			return fmt.Errorf("runner: repository child requires pinned workspace custody")
		}
		return nil
	}
	if err := in.ChildWorkspace.Validate(); err != nil {
		return err
	}
	if r.cfg.PinnedWorkspace {
		return fmt.Errorf("runner: managed child forks cannot use pinned project workspace mode")
	}

	return nil
}

func childMachineUsesRepo(machine *workflow.Machine) bool {
	for _, task := range machine.Def.Spec.Tasks {
		if task.EffectiveWorkspace() != apiv1.WorkspaceScratch {
			return true
		}
	}
	for _, gate := range machine.Def.Spec.Gates {
		if gate.Evaluator == apiv1.EvaluatorAgentic && gate.EffectiveWorkspace() != apiv1.WorkspaceScratch {
			return true
		}
	}
	return false
}

func (r *Runner) childWorkspaceOptions(in StartInput) (worktree.ChildOptions, error) {
	if in.Child == nil || in.ChildWorkspace == nil || r.cfg.Worktrees == nil || r.cfg.RepoCloneURL == nil {
		return worktree.ChildOptions{}, fmt.Errorf("runner: child workspace custody is unavailable")
	}
	if err := in.ChildWorkspace.Validate(); err != nil {
		return worktree.ChildOptions{}, err
	}
	url, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return worktree.ChildOptions{}, err
	}
	if worktree.RepositoryDigest(url) != in.ChildWorkspace.RepositoryDigest {
		return worktree.ChildOptions{}, fmt.Errorf("runner: child workspace repository binding changed")
	}
	return worktree.ChildOptions{RepoURL: url, RunID: in.ChildWorkspace.WorkspaceID, OwnerRunID: in.RunID, Gaggle: in.Gaggle, SnapshotSHA: in.ChildWorkspace.ForkSHA}, nil
}

func (r *Runner) adoptChildWorkspace(ctx context.Context, in *StartInput) error {
	opts, err := r.childWorkspaceOptions(*in)
	if err != nil {
		return err
	}
	adopted, err := r.cfg.Worktrees.AdoptChildFromSnapshot(ctx, opts)
	if err != nil {
		return fmt.Errorf("runner: verify child workspace custody: %w", err)
	}
	if in.WorkspaceBranch != "" && in.WorkspaceBranch != adopted.Branch {
		return fmt.Errorf("runner: child workspace cannot rebind its owned branch")
	}
	in.childWorkspace = &childRunWorkspace{worktree: adopted}
	in.WorkspaceBranch = adopted.Branch
	in.WorkspaceBranchSHA = in.ChildWorkspace.ForkSHA
	return nil
}

// PinnedChildWorkspaceAdmission uses the shared workspaceauthority.PinnedChildWorkspaceAdmission contract.
func PinnedChildWorkspaceAdmission(reader *journal.Reader, id journal.RunIdentity) (*ChildWorkspaceAdmission, error) {
	return workspaceauthority.PinnedChildWorkspaceAdmission(reader, id)
}

func (r *Runner) restoreChildWorkspace(ctx context.Context, reader *journal.Reader, id journal.RunIdentity, in *StartInput) error {
	if err := restoreChildCredentials(reader, id, in); err != nil {
		return err
	}
	admission, err := PinnedChildWorkspaceAdmission(reader, id)
	if err != nil {
		return err
	}
	in.ChildWorkspace = admission
	if err := r.validateChildWorkspacePlan(*in); err != nil {
		return err
	}
	if admission == nil {
		return nil
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	if err := VerifyChildWorkspaceQuiescence(reader, id, events); err != nil {
		return err
	}
	if in.workspaceRevision != nil {
		return fmt.Errorf("runner: child history changed its admitted workspace revision")
	}
	if id.WorkspaceRepository == nil || !reposEqual(&in.RepoRef, id.WorkspaceRepository) {
		return fmt.Errorf("runner: child repository differs from its pinned workspace")
	}
	in.RepoRef = *id.WorkspaceRepository.DeepCopy()
	return r.adoptChildWorkspace(ctx, in)
}

// Child runs recover only their pinned fork. Reject persisted redirection before
// ordinary workspace-revision resolution can contact another repository.
func (r *Runner) restoreExecutionWorkspace(ctx context.Context, reader *journal.Reader, id journal.RunIdentity, in StartInput, events []journal.Event, parallel *parallelExec, start, branch int) (StartInput, error) {
	if err := r.validateChildExecution(in); err != nil {
		return in, err
	}
	if in.Child == nil {
		return r.restoreResumeWorkspaceRevision(ctx, in, events, parallel, start, branch)
	}
	for _, event := range events {
		if event.WorkspaceRevision != nil {
			return in, fmt.Errorf("runner: child history cannot select another workspace revision")
		}
	}
	in.pinConfiguredRepository()
	if err := r.restoreChildWorkspace(ctx, reader, id, &in); err != nil {
		return in, fmt.Errorf("runner: restore child workspace custody: %w", err)
	}
	return in, nil
}

func (r *Runner) createChildStageWorkspace(ctx context.Context, in StartInput, stage string, mode apiv1.WorkspaceMode, syncBase bool, branch string) (*stageWorkspace, error) {
	if mode == apiv1.WorkspaceRepoReadOnly {
		return r.createChildReadOnlyStage(ctx, in, stage, syncBase, branch)
	}

	if in.childWorkspace == nil || mode != apiv1.WorkspaceRepo || in.workspaceRevision != nil {
		return nil, fmt.Errorf("runner: child workspace cannot change its admitted repository, mode or base")
	}
	state := in.childWorkspace
	state.stage.Lock()
	opts, err := r.childWorkspaceOptions(in)
	if err == nil {
		var verified *worktree.Worktree
		verified, err = r.cfg.Worktrees.AdoptChildFromSnapshot(ctx, opts)
		if err == nil && (verified.Path != state.worktree.Path || (branch != "" && branch != verified.Branch)) {
			err = fmt.Errorf("runner: child workspace custody changed before dispatch")
		}
	}
	if err != nil {
		state.stage.Unlock()
		return nil, err
	}
	base := in.RepoRef.Branch
	if base == "" {
		base = "main"
	}
	if err := state.worktree.PrepareChildStage(ctx, opts, base, syncBase); err != nil {
		state.stage.Unlock()
		return nil, err
	}
	additional, err := r.provisionAdditionalCheckouts(ctx, in, opts.RunID)
	if err != nil {
		state.stage.Unlock()
		return nil, err
	}
	verify := func(ctx context.Context) error {
		_, err := r.cfg.Worktrees.AdoptChildFromSnapshot(ctx, opts)
		return err
	}
	return &stageWorkspace{path: state.worktree.Path, worktree: state.worktree, additional: additional, retainedChild: verify, release: state.stage.Unlock}, nil
}

func validateChildWorkspaceResult(in StartInput, result apiv1.ResultEnvelope) error {
	if in.ChildWorkspace == nil {
		return nil
	}
	if result.WorkspaceRevision != nil {
		return fmt.Errorf("runner: child result cannot select another workspace revision")
	}
	if branch, ok := result.Outputs[runsemantics.WorkspaceBranchOutput].(string); ok && branch != "" && branch != in.WorkspaceBranch {
		return fmt.Errorf("runner: child result cannot rebind its owned workspace branch")
	}
	return nil
}
