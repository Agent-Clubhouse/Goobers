# Worktree and local-branch retention

The top-level `retention` section in `instance.yaml` controls automatic cleanup
of retained terminal-failure worktrees and eligible local run branches. It is
separate from `telemetry.retention`, which controls run journals and telemetry
rollups, and from `retention.recovery`, which controls recovery snapshots.

Retention defaults to enabled. An omitted `retainedWorktreeMaxAge` uses `168h`
(seven days), and the default first-enable behavior reports candidates for seven
days before deleting them. Set `retention.dryRun: true` to keep reporting without
deletion, or `retention.enabled: false` to disable this cleanup.

## Local-branch limitation

Local run-branch cleanup currently proves that a branch landed only when its tip
is a Git ancestor of another local branch. This is valid for fast-forward and
ordinary merge-commit histories. A squash merge creates a different commit, so
the original run-branch tip is not an ancestor of the target branch. Merge-queue
and legacy landing paths likewise do not currently provide alternate authority
to this retention rule.

Consequently, local branches from squash, merge-queue, or legacy landings may
remain after a retention sweep. The retained-worktree age and byte rules still
apply to retained failure worktrees, but operators must not treat the local
run-branch rule as a complete disk bound for those landing modes.

The daemon fails closed: a terminal run and an unprotected branch are necessary
but not sufficient for deletion. It does not infer landing from a matching
commit message, a similar patch, or the absence of an open pull request.

This limitation is tracked by [issue #4861](https://github.com/Agent-Clubhouse/Goobers/issues/4861).
Choosing an additional source of landing authority is a separate policy change.
