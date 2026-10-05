# haw-child-handoff-adapter: command composition review

The host adapter needs existing daemon credential/config lookup, run registry, scheduler capacity and worktree manager ownership. It connects runner.ChildHandoff to internal/childworkflow custody APIs and derives configured credential-path exclusions from host configuration. Snapshot, application and authority algorithms remain in reusable packages; the adapter carries no second implementation of them.

Stable HAW design tasks are the planning references until numbered backlog items
are created after review. This declaration applies to this PR against its stacked
base; the shared baseline is deliberately unchanged.
