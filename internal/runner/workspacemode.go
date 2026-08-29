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
//
// The precedence itself is apiv1.Task.EffectiveWorkspace — shared with the
// engine's continuity selector, pod dispatch and activities — so the local
// runner and the engine cannot read one declaration two ways; only the
// self-arm default for "" is applied here.
func taskWorkspaceMode(t apiv1.Task) apiv1.WorkspaceMode {
	if mode := t.EffectiveWorkspace(); mode != "" {
		return mode
	}
	return apiv1.WorkspaceRepo
}

// gateWorkspaceMode resolves the effective workspace for a gate evaluation.
func gateWorkspaceMode(g apiv1.Gate) apiv1.WorkspaceMode {
	if mode := g.EffectiveWorkspace(); mode != "" {
		return mode
	}
	return apiv1.WorkspaceRepo
}
