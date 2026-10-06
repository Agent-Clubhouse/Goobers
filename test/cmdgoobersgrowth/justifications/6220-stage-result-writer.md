# #6220: centralized provider-stage JSON result-file writes

Growth: +1 non-test file in `cmd/goobers` (`stageresult.go`); non-test lines
shrink by about 19.

## Why the growth belongs in the command package

`stageresult.go` holds `writeStageResultJSON`, the one marshal/write/diagnose
helper that ten provider-stage commands (`apply-verdict`, `backlog-assignment`,
`backlog-dedupe`, `backlog-health`, `backlog-query`, the read-only backlog
report, `cancel-pending-ci`, `check-issue-staleness`, `elect-lander`,
`file-issues`) now share instead of each repeating the same
`json.Marshal` + `os.WriteFile` + `pf(stderr, ...)` block. It writes through the
package's `pf` diagnostics helper and returns the stage exit code each caller
already used, so it is stage-command glue, not reusable library code.

## Could any of it live elsewhere?

It could be folded into an existing file, but a dedicated file keeps the helper
next to its characterization test (`stageresult_test.go`). The file count goes
up by one while the package's non-test line count goes down.
