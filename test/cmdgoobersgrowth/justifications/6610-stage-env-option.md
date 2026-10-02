# #6610: per-test stage credential environment

The command package gains about 40 non-test lines and no files. The lookup
itself (`stageenv.Lookup`, with the process environment as its nil default)
lives in the new `internal/stageenv` package.

What stays in `cmd/goobers` is wiring around types that only exist there:
- a `stageProviderConfig` field and its `withStageProviderEnv` option;
- explicit-environment variants of `providerToken`, `stageRefreshingToken`,
  `stageADOCredentialSource` and `adoRemediationGitAuthEnvironment`.

The original names are kept as one-line wrappers over the process
environment. That way the 46 `providerToken` call sites in 28 command files,
and the other stage callers, stay untouched. Most of the growth is those
wrappers plus their doc comments. No runtime behaviour changes.
