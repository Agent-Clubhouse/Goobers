# #6126 + #6131: PR feedback snapshots and review-thread publication receipts

Growth: +1603 non-test lines and +4 files in `cmd/goobers`.

- `prfeedbacksnapshot.go` (new, 428 lines). It builds the
  `goobers.dev/pr-feedback-snapshot/v1` record and compares a live read
  against it. It has the canonicalization rules (id ordering, body digests,
  marker and bot exclusion) and the structured stale reasons. It reads the
  provider through the review-thread stage surface (`reviewThreadResolver`),
  which lives in this package. The ADO identity attribution of the refreshed
  brief comments reuses `gather-pr-context`'s helpers in
  `gatherreviewthreads.go`.
- `prfeedbackcheck.go` (new, 67 lines) and `prfeedbackrepass.go` (new, 170
  lines) add `pr-claim --verify-feedback` and the repass classification. They
  extend the existing `pr-claim` guard and its lifecycle result type
  (`prRemediationLifecycleResult`, `endClaimedPullRequest`), so they belong
  next to that code.
- `reviewthreadreceipt.go` (new, 389 lines) holds the incremental receipt
  writer and the reconciliation that `resolve-review-threads` runs on retry.
  It loads earlier receipts from the run journal through the stage's own
  journal reader and `GOOBERS_TASK`.
- The rest are edits to the existing stage commands this feature changes:
  `gather-review-threads` (+84 net), `resolve-review-threads` (+286 net),
  `pr-claim` lifecycle (+85 net), `push-remediated` (+34 net),
  `respond-to-findings` and `prrevision.go`. `gather-ci-failures` (+36)
  now reads an older brief version through its own schema and migrates it,
  so a run in flight across the v4 deploy still resumes. About 3 lines are CLI registry
  entries (`clisynopsis.go`, `completionmodel.go`).

## Could any of it live elsewhere?

The persisted contracts are already outside this package:
`apiv1.PRFeedbackSnapshot`, `apiv1.ReviewThreadPublication`, the stale reason
type and both JSON schemas are in `api/`. The provider reads they depend on
are in `providers/`. What is left is stage policy built on stage plumbing that
only exists here: `providerInput`, `failProviderStageWithCode`, the
claim-ledger helpers, and the remediation-brief journal reader.

The snapshot canonicalization and comparison could later move into a small
package of their own (for example `internal/prfeedback`), so that the stages
only call `Build` and `Compare`. That depends on the same stage-plumbing
extraction as the other `cmd/goobers` follow-ups, so it is not part of this
change.
