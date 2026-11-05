package runsemantics

// WorkspaceBranchOutput is the well-known stage output that REBINDS the branch
// every subsequent stage's worktree checks out, for the rest of the run
// (issue #392).
//
// By default a run's stages share one branch — providers.BranchName,
// "goobers/<workflow>/<run-id>" — created off RepoRef.Branch by the first
// stage and checked out as-is, carrying prior stages' commits, by every stage
// after it (internal/worktree.Manager.Create's own doc comment, #133). That
// shared branch, not a shared directory, is what makes local-ci and the
// reviewer gate evaluate the run's actual diff.
//
// A workflow that re-enters on work that ALREADY has a branch needs that same
// continuity mechanism pointed somewhere else. pr-remediation is the case this
// exists for (docs/design/v0/pr-lifecycle-loop.md §5): it rebases and reworks
// an EXISTING PR, so its implement/review/local-ci stages must operate on the
// PR's own head branch, not a fresh one cut from main. Its gather-pr-context
// entrypoint emits the selected PR's head branch under this key, and every
// stage from there on — including agentic stages and gate evaluators, which
// have no way to re-checkout anything for themselves — is provisioned against
// it with no per-stage cooperation at all.
//
// The rebinding is sticky for the remainder of the run and survives a crash:
// stage outputs are journaled on stage.finished, so Resume recovers the most
// recent binding (lastWorkspaceBranch) into walkState rather than silently
// reverting a resumed run to the default branch mid-chain.
//
// A rebound branch must already exist (worktree.CreateOptions.RequireExistingBranch
// below turns a missing one into a loud failure rather than a silent empty branch
// cut from base), and must live in the run-branch namespace the worktree manager
// protects from its prune-fetch (internal/worktree/manager.go's
// runBranchNamespace) — otherwise the next stage's WorkingCopy refresh would
// delete it. Emitting an empty value is a no-op, not a reset.
//
// ONLY A DETERMINISTIC STAGE MAY REBIND. An agentic stage's Outputs are
// authored by the model itself (internal/harness's result-shape hint invites a
// free-form `"outputs": {...}` map and internal/harness.Executor passes it
// through verbatim), so honoring this key from one would let any goober, in any
// workflow, silently move every subsequent stage — including `push-branch` —
// onto a branch of its choosing. `implementation` needs no diff at all to be
// affected by that; it just needs an implementer that decides to report a
// branch name. Rebinding is a runner control-plane decision, so it is sourced
// only from stages whose output the runner itself produced.
const WorkspaceBranchOutput = "workspaceBranch"
