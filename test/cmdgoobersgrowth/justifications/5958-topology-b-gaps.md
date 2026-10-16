# #5958: topology (b) gaps

Growth: about +170 non-test lines and +0 files in `cmd/goobers`. Most of it is
doc comments. The remaining new code is wiring between functions that already
live in this package.

## Why the growth belongs in the command package

- **`goobers run continue` revalidation** (`run_continue.go`, about +45 net).
  The work-item and pull-request claim checks were one long loop body. They
  are now `validateContinuationWorkItemClaim` and
  `validateContinuationPullRequestClaim`, plus `continuationWorkItemProvider`,
  which opens the backlog provider in topology (b). The split keeps each
  function short now that the two claim kinds are re-read from different
  providers. The provider is opened through `newProviderForStage`,
  the stage-provider seam in this package, so this code cannot move out
  without moving that seam too.
- **CFG015 and RNR007** (`validatecrossprovidercredential.go`, +104;
  `validatereality.go`, +3). Both are `goobers validate` findings next to
  CFG012, and they reuse functions defined only in this package:
  - `configuredCredentialGrants`, the credential preflight's view of which
    capabilities a gaggle's credentials back. CFG015 reports exactly what
    that function says is missing, so validate and the runtime cannot
    disagree.
  - `appendPlacementFindings`'s per-stage solve and `placementIncludesSelf`,
    the same eligibility sets that RNR005 reads.
  - `isBacklogRoleCapability` and the validate report plumbing.
  The only provider rule involved, `providerconfig.CrossProviderBacklog`,
  already lives in `internal/providerconfig`.
- **local-ci output in the ADO PR description** (`openprbody.go`,
  `crossproviderrefs.go`, +2 net lines of comment). This is a one-call change
  to the existing `crossProviderIssueText`.

## Could any of it live elsewhere?

The validate findings could move to a validate-checks package if the
credential-grant computation (`configuredCredentialGrants`,
`buildRoleCredentials`) and the placement solve move out of `cmd/goobers`
first. That extraction belongs to the broader `cmd/goobers` split, not to
this fix.
