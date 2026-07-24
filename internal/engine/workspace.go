package engine

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// WorkspaceRequest describes the working copy one stage attempt needs. It is
// the engine-side analogue of the local runner's createStageWorkspace inputs
// (internal/runner/run.go): the same identity (run + stage), the same target
// repository, and the same workspace mode vocabulary.
type WorkspaceRequest struct {
	// RunID is the run this stage attempt belongs to.
	RunID string
	// Stage is the stage (task or gate) name within the run.
	Stage string
	// Gaggle is the gaggle the run belongs to.
	Gaggle string
	// Workflow is the workflow definition name — a repo-mode provisioner
	// derives the run branch from it (providers.BranchNameIn), exactly as the
	// local runner's createStageWorkspace does.
	Workflow string
	// BranchNamespace is the run's pinned branch-namespace root; empty means
	// the default namespace.
	BranchNamespace string
	// RepoRef is the repository a repo-mode workspace is provisioned from.
	RepoRef apiv1.RepoRef
	// Mode selects the workspace kind. Empty or apiv1.WorkspaceRepo provisions
	// a repository working copy; apiv1.WorkspaceScratch an empty disposable
	// directory — the same vocabulary as DeterministicRun.Workspace.
	Mode apiv1.WorkspaceMode
}

// Workspace is one provisioned stage-attempt working copy.
type Workspace interface {
	// Path is the absolute path of the working copy. Never empty for a
	// successfully provisioned workspace — the closed invocation schema
	// requires the envelope's workspace field.
	Path() string
	// Remove tears the working copy down after the attempt.
	Remove(ctx context.Context) error
}

// WorkspaceProvisioner provisions the fresh, isolated, disposable working copy
// each stage attempt runs in (ARCHITECTURE.md §5). The engine's activities
// provision one per attempt and stamp its path into the invocation envelope's
// required workspace field before the stage executes; construction fails
// closed — no provisioner, or a provision failure, means the stage errors
// rather than dispatching a partial envelope (#621/#156). The worker host
// (#632) supplies the real, worktree-backed implementation; engine tests
// supply fakes.
type WorkspaceProvisioner interface {
	Provision(ctx context.Context, req WorkspaceRequest) (Workspace, error)
}
