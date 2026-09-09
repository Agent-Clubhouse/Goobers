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
	// WorkspaceBranch is the run-scoped branch selected by an earlier
	// deterministic stage. Empty means the run's own derived branch. A non-empty
	// branch must already exist; provisioners must never create it from base.
	WorkspaceBranch string
	// RepoRef is the repository a repo-mode workspace is provisioned from.
	RepoRef apiv1.RepoRef
	// Mode selects the workspace kind. Empty or apiv1.WorkspaceRepo provisions
	// a repository working copy; apiv1.WorkspaceScratch an empty disposable
	// directory — the same vocabulary as DeterministicRun.Workspace.
	Mode apiv1.WorkspaceMode
	// SyncBase asks a repo-mode provisioner to merge the freshly fetched base
	// ref into the run branch before handing the workspace over —
	// DeterministicRun.SyncBase (#813), threaded exactly as the local runner's
	// createStageWorkspace threads it into worktree.CreateOptions. Never set
	// for scratch mode (compilation rejects the combination).
	SyncBase bool
	// WorkspaceDelta is the blob digest of the workspace-delta bundle earlier
	// stages of this run published (#3803): what a pod committed, which the
	// provisioner's own branch ref cannot know about. A repo-mode provisioner
	// must land it on the run branch (fast-forward-only) BEFORE handing the
	// workspace over, and must refuse — never silently skip — when it has no
	// way to fetch it: a stage that runs against base while believing it
	// continues from its predecessor is the silent wrong result this exists
	// to remove. Empty means nothing to continue from. Only ever set for a
	// writable repo mode.
	WorkspaceDelta string
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

// WorkspaceDeltaPublication is what a workspace reports after a stage ran in
// it: the bundle digest of base..HEAD when the stage advanced its branch, or
// Unchanged when it did not. Base/Tip are the bundle's two commits, journaled
// beside the digest (runner.workspace.delta).
type WorkspaceDeltaPublication struct {
	Digest    string
	Base      string
	Tip       string
	Unchanged bool
}

// DeltaPublisher is the optional PUBLISH half of continuity a repo-mode
// Workspace may implement (#3803 reverse direction): after a stage succeeds
// on a writable repo workspace, the engine asks the workspace to bundle what
// the stage committed and put it in the blob plane, so the next stage — on
// a pod, or on another worker — can build on it. A workspace that does not
// implement this (scratch, tests) publishes nothing, which is exactly the
// pre-#3803 behaviour for self-placed stages.
type DeltaPublisher interface {
	PublishDelta(ctx context.Context) (WorkspaceDeltaPublication, error)
}

// DiffReader is the optional READ half a repo-mode Workspace may implement so
// the engine can see what a stage actually changed without shelling out to git
// from workflow code (#3882).
//
// Two callers need it, and they need it for different reasons. A reviewer gate
// needs the subject diff to review it at all (#3384) and to decide whether
// there is anything to review — an empty diff fast-fails and an unchanged diff
// is a repeat. A finished agentic attempt needs the diff its workspace is
// about to take to the grave (#3366), which is the only copy of work an agent
// committed but never pushed.
//
// Both are answered by the WORKSPACE rather than by the engine because the
// engine holds no repository: the activity host provisions a working copy per
// attempt and tears it down, so the window in which "what changed" is
// answerable is exactly the workspace's lifetime. A workspace that does not
// implement this reports no diff, and every behaviour keyed on one degrades to
// the pre-#3882 engine rather than failing.
type DiffReader interface {
	// Diff returns the patch between baseRef and the workspace's current HEAD.
	// Empty (not an error) when the branch has not moved.
	Diff(ctx context.Context, baseRef string) ([]byte, error)
	// Head reports the workspace's current branch and the base ref the diff
	// above is taken against, for the sidecar that records provenance.
	Head(ctx context.Context) (branch string, baseRef string, err error)
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

// writableWorkspace reports whether a self-arm workspace mode is one whose
// commits the continuity record carries. Empty is the historical default —
// the writable repo worktree — on every self arm (workerhost's Provision
// treats "" and repo identically), which is why this is not
// WorkspaceMode.IsWritableRepo: that helper is the pod-side reading, where
// "" provisions nothing.
func writableWorkspace(mode apiv1.WorkspaceMode) bool {
	return mode == "" || mode.IsWritableRepo()
}
