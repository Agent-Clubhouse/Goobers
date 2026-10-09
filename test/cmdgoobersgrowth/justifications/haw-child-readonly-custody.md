# HAW-CHD read-only child workspace custody

The existing command-owned child pod factory and recovery adapter must select
the same retained workspace for a stage. This slice adds one shared selector to
the existing adapter file: read-only stages adopt their pinned detached view;
writable stages adopt their retained child branch; unsupported modes fail closed.
No new command production file is added.

The selector belongs beside the two daemon composition callers because it binds
their recorded stage and workspace mode to the existing worktree manager. Git
creation, snapshot verification, dirty-view refusal and read-only validation
remain in internal/worktree and internal/runner. The factory integration test
verifies exact view adoption and refusal after that view is modified; runner and
worker tests cover the corresponding execution boundary.
