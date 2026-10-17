# #5890: an unanswered review finding blocks healers, publication and merge

Growth: about +400 non-test lines and no new files in `cmd/goobers`.

## Why the growth belongs in the command package

The rule itself lives in the new `internal/findingledger` package. It decides
which findings still stand on a head after several verdicts, and how a new
verdict carries that ledger. It is pure and provider-neutral and has its own
tests. What `cmd/goobers` gains is the stage-level wiring that reads and
writes the stages' own records:

- **`verdictstate.go`.** This file now:
  - collects a pull request's verdict records in publication order (marked
    status comments, plus the scheduler-state record this file already
    owns) and fails closed on a read error;
  - folds the ledger into every publication path: GitHub/Gitea, advisory,
    and the Azure DevOps pass and non-pass paths;
  - reads the records under merge-pr's lock;
  - renders standing findings and dispositions for humans;
  - journals every block as a stage annotation.

  Each piece uses helpers that only exist here: comment trust and markers
  (`mergeReviewStatusComments`), payload parsing (`parseVerdictComment`), the
  scheduler-state record, the stage annotator, ADO identity attribution, and
  the remediation severity policy (`substantiveFindingAppliesToPR`).
- **`gatherprcontext.go`.** `gatherPRVerdict` now returns the folded record,
  and a checked variant reports ledger read failures. Its callers
  (`update-behind-pr`, `gather-pr-context`, `rebase-pr`,
  `gather-sibling-context`) therefore share one answer.
- **`updatebehindpr.go`, `prsiblingcontext.go`, `mergepr.go`,
  `applyverdict.go`.** One call or a few lines each. `runApplyVerdict`,
  `runMergePR` and `runGatherSiblingContextCore` are at their frozen
  complexity and body-length baselines and gained no lines or branches. The
  wiring replaces an existing line or moves an existing block into a helper.
- **Help text.** The four stages document the new behavior.

## Could any of it live elsewhere?

The decision logic already lives elsewhere (`internal/findingledger`). The
rest reads and writes this package's own record formats and stage seams.
Moving it would mean exporting the comment-trust, payload and scheduler-state
helpers from `cmd/goobers` first, which is a larger refactor than this fix.
