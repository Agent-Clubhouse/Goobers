// Package worktree manages the on-disk isolation contract for stage
// execution: one managed working copy per target repo, and one disposable
// git worktree per run branched off it (ARCHITECTURE.md §5, §6; DEP-004,
// DEP-026, DEP-007).
//
// A Manager owns exactly one managed working copy per distinct repo URL, kept
// as a mirror clone under Root so it is never itself checked out to a mutable
// branch. Clone/fetch for a given repo is serialized per repo so concurrent
// runs share one up-to-date copy without contending on the network or racing
// git's ref locks; runs against different repos proceed independently.
//
// Each run gets its own worktree via Manager.Create and tears it down via
// Worktree.Remove. A small on-disk marker (writer identity, owning PID,
// created-at, status) travels alongside each worktree so that Manager.Reap can
// find and remove worktrees left behind by a process that died mid-run (e.g.
// kill -9), converging disk state on daemon restart without operator
// intervention. PID liveness is local to the manager's ownership domain;
// distributed workers must use pod-private roots.
//
// This package has no dependency on the run journal or workflow DSL, and does
// not itself push branches. Branch creation (goobers/<workflow>/<run-id>) is
// supported via CreateOptions.Branch, but
// pushing that branch through the credential seam (#14) is the caller's
// responsibility once that seam exists.
//
// # Exact-revision read-only workspaces
//
// CreateOptions.ExpectedSHA implements selected-revision repo-readonly stages.
// The caller first authorizes the selected repository against configuration,
// then passes its configured URL and full lowercase object ID:
//
//	CreateOptions{
//	    RepoURL: authorizedURL, RunID: stageID,
//	    BaseRef: selectedSHA, ExpectedSHA: selectedSHA,
//	    Sparse: configuredCones,
//	}
//
// Acquisition always contacts that source for the exact object, including on
// retries when a matching object is cached. It rejects missing objects, trees,
// blobs and annotated tag objects (rather than accepting tag peeling), verifies
// <sha>^{commit}, and verifies the final detached HEAD. It never substitutes a
// branch tip or another repository. Different stage IDs receive independent
// worktrees, including parallel stages selecting the same commit.
//
// Repository changes NEVER cross read-only stage boundaries: tracked edits,
// local commits, untracked files and ignored files are discarded at teardown,
// including before recovery/preservation handoff and when a checkout is kept
// for debugging. Run-journal resume is the caller's responsibility: reconstruct
// the identical authorized repository and SHA, rather than resolving a moving
// ref or polling a provider again.
//
// Pinned callers hold the whole-run lease and serialize PreparePinnedRevision
// before each stage and ResetPinnedRevision afterward, including failure paths.
// Both reset and clean (-ffdx) the persistent checkout before detaching at the
// selected SHA; post-stage reset does not fetch. Durable discard-only custody
// also prevents interrupted stages from publishing source changes during the
// next pinned handoff. Source acquisition/hydration uses source authorization,
// not base-repository promisor fallback; the configured origin is not rerouted.
//
// ExpectedSHA requires BaseRef == ExpectedSHA and forbids Branch, SyncBase,
// RequireExistingBranch and AcquireRemoteBranch. The workflow-facing equivalents
// workspaceBranch and syncBase, and workspace-delta import/export, must also be
// rejected by the caller. An inspection/review stage uses exact-SHA repo-readonly;
// an implementation stage whose committed work must reach later stages instead
// uses writable repo and the workflow-owned branch continuity path.
//
// Partial clone and sparse cones remain configured materialization policy, not
// revision authority. Undeclared submodule recursion, LFS expansion and custom
// content filters remain disabled during exact-revision materialization/reset.
//
// Cross-platform note (#643): every managed mirror is pinned to a deterministic
// git config (core.autocrlf=false, core.longpaths=true — see managedGitConfig),
// chosen to be behavior-identical on darwin/linux and to make a Windows checkout
// deterministic rather than dependent on the host's ambient git config. Symlinks
// a symlink-less platform (Windows without Developer Mode) flattens to plain
// files are detected and surfaced as Worktree.Warnings rather than failing the
// run or passing silently. The full audit, path-length budget, and Windows
// prerequisites are in docs/guides/windows-worktree-notes.md.
package worktree
