# #5404: retract stale status blockers

Growth: +5 non-test lines and +0 files in `cmd/goobers`.

Nearly all of the change lives outside the command package:

- the blocker model, the clear-path registry and the text rendering
  (`OperatorRunSummary.BlockerLine`) in `internal/readservice`
- the observed-at facts in `internal/readmodel`
- the observation hook and the inventory/run failure split in
  `internal/runner`
- the clear recorder in the new `internal/blockerclear`

## What stays in `cmd/goobers`

- **`recoveryevents.go`** (+3: one import, two comment lines).
  `recoveryRunEvents` is the command package's composition of the recovery
  inventory reader. It is the only place that knows which of its failures
  came from reading the shared inventory (`observeRecoveryInventory`) and
  which came from the run's own records. It therefore marks the inventory
  failure with `runner.RecoveryInventoryError`, which decides the journaled
  code and how the blocker clears.
- **`runnerwiring.go`** (+2: one import, one config field). The runner is
  composed here, so the `RecoveryObserved` hook is wired here too, the same
  way `RecoveryEvents` already is.
- **`status.go`** (net 0). The run table calls `BlockerLine` instead of
  joining `potentialBlockers`.

## Could any of it live elsewhere?

No. The remaining lines are wiring and the one classification only the
reader's composition can make. Moving `recoveryRunEvents` itself out of the
command package belongs to the broader recovery-wiring extraction, not to
this fix.
