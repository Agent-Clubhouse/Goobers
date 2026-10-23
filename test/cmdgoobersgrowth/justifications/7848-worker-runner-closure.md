# Separate cloud executor construction from the local runner (#7848)

The local command gains an explicit adapter from shared executor configuration
into `runner.Config`, plus the callback wiring that keeps daemon-only lifecycle
services local. This adds 47 non-test lines and one file to `cmd/goobers`.
`cmd/goobers-cloud` adds two import lines while replacing runner types with the
shared contracts. No workflow or executor behavior moves into either command.

The adapter belongs at this composition boundary: putting it in runtimewiring
would reintroduce the local runner into the cloud binary, and putting the local
lifecycle callbacks in the shared configuration would expose unused runner
capabilities to the remote worker. Shared construction and cleanup validation
remain in internal/runtimewiring and internal/workspaceauthority.
