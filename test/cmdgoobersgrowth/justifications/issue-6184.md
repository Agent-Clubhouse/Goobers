# #6184: advisory PR review stage commands

Growth: +742 non-test lines and +1 file in `cmd/goobers`.

- `advisorypr.go` (new, 713 lines) holds three new commands. Two are
  workflow stages, `advisory-pr-select` and `advisory-pr-publish`. The third
  is an operator command, `advisory-pr-reset`. Like the existing merge-review
  stages next to it (`pr-select`, `apply-verdict`), these commands are built
  from the stage plumbing that only exists in this package: `providerInput`,
  `providerRepo`, `newProviderForStageAs`, `failProviderStage`,
  `openStageClaimLedger`, `openStageStateStore` and
  `readDecompositionInput`. Moving them into another package would mean
  exporting that plumbing first. That refactor touches every stage command,
  so it belongs in its own PR.
- The pieces that can be reused already live outside this package. The
  GitHub calls (PR listing, files, compare, contents, comments) are in
  `providers/`. The atomic lease is in `claimsclient`, and the durable
  disposition uses `stateclient` (one new key helper,
  `stateclient.AdvisoryPRKey`).
- About 20 lines are the CLI registry entries every command needs:
  `runtime_capabilities.go`, `clisynopsis.go` and `completionmodel.go`.
