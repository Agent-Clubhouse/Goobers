# backprop/fix-6567: configurable pod recovery custody budget

Growth: +69 non-test lines and +0 files in `cmd/goobers`, in `dispatchexec.go`,
`recoverypod.go` and `workerdispatch.go`.

## Why the growth belongs in the command package

- **Stamped custody budget** (`dispatchexec.go`, `workerdispatch.go`). The
  hard-coded 90s pod recovery deadline is replaced by
  `GOOBERS_RECOVERY_CUSTODY_TIMEOUT`, which the worker stamps on the pod from
  the new `runner.recoveryCustodyTimeout` setting (default 10m). Large
  repositories could not finish recovery in 90s. The worker wiring and the
  stage-side read are both command-layer code that already handles the other
  stamped pod settings.
- **Per-phase recovery tracing** (`recoverypod.go`, most of the growth). Each
  phase of pod recovery (claim lookup, base-ref resolution, artifact exclusion,
  workspace inspection, inventory preparation, archive retention) is wrapped
  so its duration and any error reach the stage log. Before this, a budget
  overrun gave no indication of which phase was slow. The phases are the
  command's own sequence; the work they call stays in `internal/recovery`.

## Could any of it live elsewhere?

The phase-timing wrapper could move into `internal/recovery` if more callers
ever run pod recovery. Today `dispatch-exec` is the only caller, and the
config default lives in `internal/dispatcher` and `internal/instance`.