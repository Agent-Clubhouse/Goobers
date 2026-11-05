# Branch recovery-inventory-reclaim-commands

Growth: +199 non-test lines and +1 file in `cmd/goobers`.

- Nearly all of it is the new `cmd/goobers/recoveryinventoryreclaim.go`.
- The rest is a few lines in `recoveryinventoryhealth.go`, which attach the
  guidance to the existing recovery-inventory gate.

When the inventory is filling or full, the Overview card used to link to a
guide. It now shows the commands that free slots.

## Why the growth belongs in the command package

- **Candidate selection** (`attachRecoveryReclaimGuidance`, the run-phase
  cache).
  - A snapshot is a candidate only when its owning run is terminal, which
    matches what `recovery-abandon` and `recovery-restore` accept.
  - Deciding that needs `runDirFor` and `terminalRunPhase`, both
    `cmd/goobers` policy that the retention pass and the recovery commands
    share.
  - The cache is pruned to the runs that currently hold a slot.
- **Retention hold** (`recoveryReclaimHold`).
  - It says whether a retention pass would actually delete an abandoned
    snapshot.
  - It reuses `readRetentionGraceState` and `retentionPassIsDryRun`, the same
    functions the retention pass uses, so the portal and the pass cannot
    disagree.
- **Command rendering** (`operatorCommand`). It wraps `quoteShellArg`, the
  host-shell quoting that `goobers init` already uses for the commands it
  prints.

## Could any of it live elsewhere?

The data shapes already live outside the command package.

- The read-model types are in `internal/readservice`.
- The abandonment fold is in `internal/instanceannotations`
  (`Fold.RecoveryAbandonments`).

Moving the remaining logic out would mean exporting the run-path, terminal-phase,
retention-grace and shell-quoting helpers as well, or passing each one in as a
callback. Either way that is more surface than the ~200 lines it would move.
