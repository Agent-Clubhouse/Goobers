# #5312: recovery-resume reports the source run's terminal journal evidence

Growth: +13 non-test lines and +0 files in `cmd/goobers`.

- `recoveryresume.go`: the stage result embeds `recovery.SourceRun` (with its
  comment), a small `recoveryResumeSource` pairs the consumed record with that
  evidence so outcomes for fresh runs and legacy daemons report none, and the
  help text names the new outputs.
- `recoveryrestore.go`/`recoverydelivery.go`: the record consumer receives the
  evidence, and both idle-journal reads use `recovery.ReadTerminalSourceRun`
  instead of a bare phase check.

## Could any of it live elsewhere?

The evidence type, its journal derivation, header encoding/validation and the
first-byte reporting writer live in `internal/recovery` (`sourcerun.go`).
What remains is the stage's result wiring and help text, which only exist in
this package.