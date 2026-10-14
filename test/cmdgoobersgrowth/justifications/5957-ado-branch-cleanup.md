# #5957: ADO terminal branch cleanup and the post-merge source-branch backstop

Growth: about +150 non-test lines in `cmd/goobers` and no new files. All of it
is wiring.

## What moved out

The cleanup logic itself lives in `providers` (`providers/ado_branchcleanup.go`):
- `ADOProvider.CleanupMergedSourceBranch`, the post-merge backstop. It confirms
  the branch is gone, keeps a moved or stacked branch, and re-reads after a lost
  lease.
- `ADOProvider.DeleteUnusedBranch`, the terminal delete. It keeps a branch that
  an open pull request uses as its base or source.
- `NewADOTokenSourceCredentialSource`.

## What stays, and why it must

- **`terminalbranch.go` / `terminalprovider.go`** gain the ADO arm of the
  existing terminal-provider dispatch, next to the GitHub and Gitea arms. The
  arm builds the provider from the token the daemon's credential injector
  materialized, picks the credential kind from the instance config, and maps
  the provider's in-use refusal to the journal's skip reasons. The injector, the
  instance config and the journal annotator are all composition-root concerns of
  this package.
- **`mergepr.go`** gains `stageADOSourceBranchBackstop`, about 20 lines that
  build the stage provider through the stage provider seam
  (`newProviderForStageAs`, `github:branch:delete`, `kind="branch"` recording)
  and translate the provider's outcome into the `mergeBranchCleanup` result
  shape. The ADO reporting arm is split out of `reportMergedBranchCleanup`, with
  one shared print helper.
- **`mergequeuepoll.go`** gains the ADO transport's call site and keeps the
  merged read's head, so the backstop needs no second provider read.
