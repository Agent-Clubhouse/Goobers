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
// Worktree.Remove. A small on-disk marker (owning PID, created-at, status)
// travels alongside each worktree so that Manager.Reap can find and remove
// worktrees left behind by a process that died mid-run (e.g. kill -9),
// converging disk state on daemon restart without operator intervention.
//
// This package has no dependency on the run journal, workflow DSL, or stage
// contract packages, and does not itself push branches — branch creation
// (goobers/<workflow>/<run-id>) is supported via CreateOptions.Branch, but
// pushing that branch through the credential seam (#14) is the caller's
// responsibility once that seam exists.
package worktree
