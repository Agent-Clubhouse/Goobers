# Worktree and local-branch retention

The top-level `retention` section in `instance.yaml` controls automatic cleanup
of retained terminal-failure worktrees and eligible local run branches. It is
separate from `telemetry.retention`, which controls run journals and telemetry
rollups, and from `retention.recovery`, which controls recovery snapshots.

Retention defaults to enabled. An omitted `retainedWorktreeMaxAge` uses `168h`
(seven days), and the default first-enable behavior reports candidates for seven
days before deleting them. Set `retention.dryRun: true` to keep reporting without
deletion, or `retention.enabled: false` to disable this cleanup.

## Recovery handoff

Before removing a worktree that needs recovery, Goobers must durably capture its
tracked files and selected non-ignored untracked files. An empty untracked-file
selection means no additional files are added; it must never expand into a
forced add of the whole worktree, including ignored dependencies or build output.
Tracked edits and deletions are still captured when that selection is empty.

A failed handoff preserves the worktree. Do not delete it or broaden capture to
include ignored files to bypass the failure.

## Local branches

A local run branch can qualify either because its tip is a Git ancestor of
another local branch, or because its owning run completed, failed, or was
aborted at least `retention.terminalBranchMaxAge` ago. The latter defaults to
`720h` (30 days), allowing old unmerged branches to expire, including branches
left behind by squash merges. Set it to `"0s"` to disable the branch-age rule,
or another nonnegative Go duration to change the age floor.

The age comes from the durable root `run.finished` event, never a commit date
or file modification time. Missing, inconsistent, or future timestamps do not
authorize age-based cleanup. Missing journals and legacy branches whose item
ownership cannot be established remain on disk and are reported as skipped.

Both rules protect active and parked runs, including escalated runs and runs
waiting at a gate. Current provider item or pull-request state is checked using
the repository recorded when the item was selected, even after its claim has
been released. Needs-human, escalated, blocked, paused, and parked items protect
their branches. A provider read failure preserves the branch. Branches referenced
by another protected run are also preserved.

Candidates use the existing dry-run and first-enable grace window. Immediately
before deletion, the sweep rechecks the branch tip, run state, item state, and
age or ancestry authority. Deletion failures are reported. No remote branch or
provider item is modified.

Git ancestry alone still does not prove a squash or merge-queue landing. The age
rule supplies separate terminal-run authority; it does not infer landing from a
commit message, patch similarity, or the absence of an open pull request. Broader
landing proof is tracked in [issue #4861](https://github.com/Agent-Clubhouse/Goobers/issues/4861).
