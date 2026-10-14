# #6134: merge-review refuses to land over open, unanswered human feedback

This PR is stacked on #6494 (`6494-rebase-pr-shared-identity.md`). Its own
growth is about +375 non-test lines and +2 files in `cmd/goobers`.

## Why the growth belongs in the command package

- **`mergefeedbackgate.go` (new).** This is merge-pr's new conjunct:
  - its inputs (`blockUnansweredFeedback`, plus the shared identity settings);
  - the typed refusals `unanswered-human-feedback` and
    `human-feedback-unverifiable`, which are merge-pr's own routing vocabulary;
  - the per-provider binding of the feedback read.
- **`humanfeedback.go` (new).** It defines the "open feedback" set that merge-pr
  refuses on and rebase-pr remediates on: general comments, review bodies,
  unresolved threads, and Goobers answers kept from resolved threads. Both
  stages must use one definition, or a refused PR is cleared and handed back
  every cycle.
- **`recordmergerefusal.go`.** It routes the new refusal to remediation instead
  of demotion, on the stage that already owns merge-refusal bookkeeping.
- **`mergepr.go`, `rebasepr.go`, `mergereviewado.go`.** These are one-line
  wirings, help text, and removing the ADO `unsupported` override.

## Could any of it live elsewhere?

The provider-shaped part already lives in `providers`:
`ADOProvider.ListPullRequestFeedbackThreads`, which returns threads with their
resolution status. The flattening and classification depend on the comment
classifier in `cmd/goobers/commentorigin.go`. They could move together into a
small `internal/` feedback package if a consumer outside the stage commands
appears. The cross-stage settings check stays in `api/validate` (WF029).
