# #5957: gaggle backlog settings for stage pods, fail closed

Growth: +1 file in `cmd/goobers` (`stagebacklog.go`) and a small non-test line
increase.

## Why the growth belongs in the command package

- **`stagebacklog.go`** resolves a stage's gaggle backlog settings (the ADO
  backlog project, the claim-scope labels and `doneStates`). It covers both
  sides:
  - in the worker, it encodes each gaggle's settings for the dispatcher to stamp
    (`workerdispatch.go`);
  - in the stage, it decodes the dispatcher's stamp or reads the instance
    config, and refuses when neither is available.

  Its consumers are all stage-command helpers in this package:
  - `backlogRepoRefForStage`, `applyGaggleDoneStates` and `stageADOBacklogScope`;
  - the ADO stage provider factory (`stageprovider.go`), which is the
    fail-closed gate.

  It mirrors the bot-login stamp resolution (`stageProviderConfiguredLogin`),
  which lives here for the same reason.
- `adoprovider.go` loses the per-setting "default is used" warnings and the
  `GOOBERS_BACKLOG_PROJECT` fallback that the stamp supersedes.

## Could any of it live elsewhere?

The stamping itself is in `internal/dispatcher` (`GaggleBacklogEnv`,
`Config.GaggleBacklogs`), and the settings are applied through the existing
`internal/providerconfig` functions. The command package only gains the
resolution, which reads `GOOBERS_GAGGLE`, the instance layout and the stamp in
the same way the other stage-side resolvers here do.
