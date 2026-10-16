# #5959: `goobers connect` keeps the existing ADO credential source; guided merge-review on GitHub

Growth: about +121 non-test lines and +0 files in `cmd/goobers`.

- `connectado.go` (+~80): `connectADOTarget` chooses the credential source
  connect writes. It keeps the existing source unless `--token-env` is
  explicit (the PO decision for #5959). That source can be azure-cli,
  workload-identity or managed-identity, or a PAT's own token reference.
  Supporting helpers, with their doc comments:
  - `connectADONonPATAuthKind` and `connectADOAuthLabel`.
  - `connectADOSameCredential`, which compares by value so a repeat connect
    is idempotent.
  - `connectKeptADOCredential`, which points the later steps at the kept
    PAT variable or the kept source.
- `connect.go` (+~30):
  - Help text for the new behaviour.
  - The `connectOptions` fields (`explicitTokenEnv`, `keptADOAuth`, `stores`).
  - Resolving secret stores for a kept source.
  - Skipping the token preflight when the kept source names no token variable.
  - The `kept <kind> authentication` output line.
- `connectadoseed.go` (+~22): `connectADOSeedClient`. With a kept source,
  `--seed` files through that source, using the existing `newGuidedADOClient`,
  rather than through a token variable the instance does not name.
- `init.go` (+~2): help text. Merge-review is offered on GitHub and uses the
  `--pr-token-env` token.

## Could any of it live elsewhere?

The guided-scaffold grant change, which is the reusable part, lives in
`internal/instance` (`guidedMergeReviewCapabilities`). The rest is behaviour of
the `connect` and `init` commands: their flags, help text, preflight and seeding
steps, all of which exist only in this package. The seeding client reuses the
guided getting-started client rather than adding a new one.
