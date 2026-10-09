# #7062: PR lanes honour the live run that opened the PR

Growth: +56 non-test lines and +0 files in `cmd/goobers`. Most of it is doc comment.

## Why the growth belongs in the command package

- **`pullRequestClaimStatusFor`** (`cmd/goobers/prselectfairness.go`). This wraps the
  existing `pullRequestClaimStatus`, which already lives here next to the two
  selection paths that consult it: merge-review fairness (`observePRSelectEligibility`)
  and the pr-remediation claim filter (`filterClaimAvailablePullRequests` in
  `remediationcounter.go`, which is changed by one line). The new rule is a
  selection policy: a PR is claimed while the run that opened it still holds a live
  claim. It belongs beside the policy it extends, and it reads the same
  `claimsclient.Listing` those callers already hold under the ledger lock.
- **`runBranchOriginRunID`** (same file). It parses the run ID out of a run-scoped
  head branch, `<namespace><workflow>/<runID>`.

## Could any of it live elsewhere?

`runBranchOriginRunID` is the inverse of `providers.BranchNameIn`, and it could move
to `providers` as `RunIDFromBranchName` so the two stay together. That would make it
a `providers` API with one caller. It's worth doing when a second caller appears
(for example, operator tooling that maps a PR back to its run). The claim-policy
wrapper should stay with the selection code.
