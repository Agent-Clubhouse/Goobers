# #6134 follow-up: disarm an armed landing on fresh human feedback

This PR is stacked on #6494 and on the #6134 gate. Its own growth in
`cmd/goobers` is small: about +120 net non-test lines and no new files.

## Why the growth belongs in the command package

- **`prcommentwatch.go` and `prcommentwatch_forge.go`.** The watcher no longer
  hard-excludes `goobers:merge-ready` PRs. On fresh human feedback it disarms
  the landing before routing the PR: a new `disarm` method on the per-provider
  forge arm, plus a `Disarmed` field in the result. That is stage policy:
  decide that a landing must stop, and stop it before routing.
- **`mergequeuepoll.go`.** queue-watch withdraws a queued PR that gains
  `goobers:needs-remediation`, using the dequeue it already performs for
  opt-outs.

## Could any of it live elsewhere?

The provider operation, `ADOProvider.CancelAutoComplete`, lives in
`providers`. What remains in `cmd/goobers` decides when a stage calls it.
