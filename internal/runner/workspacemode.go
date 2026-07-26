package runner

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// taskWorkspaceMode resolves the effective workspace for a stage.
//
// Run.Workspace stays authoritative for a deterministic task, so every existing
// definition behaves byte-identically. Task.Workspace is the seam an AGENTIC
// task uses, which previously had no way to express this at all and was
// hardcoded to the writable repo worktree — the reason a fan-out of agentic
// research stages could not be given a non-colliding workspace
// (docs/design/static-fan-out-fan-in.md §6.5).
//
// Unset still means the writable repo worktree, preserving the historical
// default for every stage that does not opt in.
func taskWorkspaceMode(t apiv1.Task) apiv1.WorkspaceMode {
	if t.Run != nil && t.Run.Workspace != "" {
		return t.Run.Workspace
	}
	if t.Workspace != "" {
		return t.Workspace
	}
	return apiv1.WorkspaceRepo
}

// gateWorkspaceMode resolves the effective workspace for a gate evaluation.
func gateWorkspaceMode(g apiv1.Gate) apiv1.WorkspaceMode {
	if g.Agentic != nil && g.Agentic.Workspace != "" {
		return g.Agentic.Workspace
	}
	return apiv1.WorkspaceRepo
}
