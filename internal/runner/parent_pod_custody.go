package runner

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/worktree"
)

// ContainedParentWorkspaceKind records the managed checkout held before its
// isolated worker can start. The pod never receives this host custody marker.
const ContainedParentWorkspaceKind = "isolated.parent.workspace"

// ContainedParentWorkspaceCustody is a host-only recovery receipt. It contains
// no filesystem path, clone URL, credentials or permission grant.
type ContainedParentWorkspaceCustody struct {
	Version   int                        `json:"version"`
	Origin    *apiv1.ChildWorkflowOrigin `json:"origin"`
	Workspace worktree.StageCustody      `json:"workspace"`
}
