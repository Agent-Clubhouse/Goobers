# #6134 follow-up: withdraw an armed landing when human feedback arrives

This PR is stacked on #6494 and on the #6134 merge gate. Its own growth in
`cmd/goobers` is about +280 net non-test lines and +1 file.

## Why the growth belongs in the command package

- **`mergequeuefeedback.go` (new).** Queue-watch runs merge-pr's
  unanswered-feedback gate on every poll of a still-queued PR. If the gate
  refuses, it withdraws the PR and routes it to remediation: on GitHub through
  the issues credential, with a comment that acknowledges no feedback; on
  Azure DevOps through pull-request labels. That is stage policy, deciding
  when a watched landing must stop.
- **`mergequeuepoll.go`.** It wires the per-provider withdrawal into the
  existing poll loop. The ADO transport now cancels the auto-complete where it
  used to report "unsupported". The withdrawal-label rule is also here: a
  route to remediation off landing withdraws the PR, while post-merge's
  overlap fan-out does not.
- **`prcommentwatch.go`, `prcommentwatch_forge.go`.** The opt-in landing scan
  (`blockUnansweredFeedback`, policy action `disarm-landing`): disarm first,
  then route, judged by open feedback on ADO.
- **`mergefeedbackgate.go`.** Splits `assess` (unanswered vs. unreadable) out
  of `evaluate`, so queue-watch can keep a PR queued on a failed read where
  merge-pr refuses.

## Could any of it live elsewhere?

The provider operation lives in `providers`: `ADOProvider.CancelAutoComplete`,
with its confirmed-cancel outcomes. What remains here decides when stages call
it.
