# Contained parent contribution custody

The command adapter records a host receipt after the stopped worker's returned
workspace is verified and imported. Generic receipt validation, linear handoff,
archive verification and checkout retirement live in internal/runner and
internal/worktree. Command wiring only joins those primitives with daemon
family settlement and journal pruning, including parents that never accepted a
child and therefore have no queue family. Existing explicit linear repoFrom
stages are admitted; parallel workspace shapes remain gated pending fan-in.
