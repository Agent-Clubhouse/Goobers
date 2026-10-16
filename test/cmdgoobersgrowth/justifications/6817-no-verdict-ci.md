# #6817: CI that finished without a verdict

Growth: about +90 non-test lines and +1 file in `cmd/goobers`
(`cinoverdictnotice.go`).

## Why the growth belongs in the command package

- **`mergepr.go`** gains `ciNotReadyReason`. It decides the refusal text for
  merge-pr's live CI conjunct: a head whose checks finished without a verdict
  (cancelled, never started, stale) is refused with a `ci-no-verdict:` reason,
  not with `CI is "pending"`. This is an edit to an existing stage handler's
  refusal vocabulary, which lives beside its other reason tokens.
- **`recordmergerefusal.go`** and **`cinoverdictnotice.go`**: record-merge-refusal
  must not count such a refusal toward merge demotion, because a demotion
  clears only on a head change and a CI rerun could not lift it. Instead it
  posts one sticky notice per head asking for a rerun. The notice uses the
  stage's provider, comment list and the existing `postOrUpdateStickyComment`
  helper, all of which are command-package stage glue.
- **`dispatchcipoll.go`** gains the pod-path summary for the new
  `ci_no_verdict` code, so the pod path reports exactly what the in-process
  path does.

## Could any of it live elsewhere?

The classification itself does live elsewhere: `providers.OnlyNoVerdictPending`
and `providers.NoVerdictCheckNames` are shared with ci-poll in
`internal/executor`. What remains here is the stage's refusal text and its
sticky-comment bookkeeping, whose only callers are these stage handlers.
