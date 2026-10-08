# Issue #5810: pull request title predicates

Growth: +15 net non-test lines and +0 files in `cmd/goobers`, all in
`cmd/goobers/prselect.go`.

## Why the growth belongs in the command package

- **Reading the `titlePredicate` input** (+5 lines). `runPRSelectCore` already
  reads the stage's other opt-in filter inputs (`author`, `assignee`,
  `requestedReviewer`) and fails the stage on invalid input. The title
  predicate is compiled once at the same point so a bad expression fails before
  any provider call.
- **Handling the filter error at the two existing filter sites** (+8 net
  lines). `pullRequestsForSelectionCommon` already applied the identity filters
  to the targeted webhook PR and to each candidate. Both sites now call
  `ListPullRequestsRequest.MatchesSummary`, which can return an error, so each
  needs an error check.
- **Carrying the ADO poll title onto the selection candidate** (+1 line) in
  `adoSelectionCandidate`, the stage's own ADO projection.
- **One import** (+1 line).

## Could any of it live elsewhere?

The matching logic already does. The grammar lives in
`internal/fieldpredicate` (`CompileTitlePredicate`) and the combined identity
and title check lives in `providers` (`ListPullRequestsRequest.MatchesSummary`).
What remains is input plumbing and error propagation for this stage only.
