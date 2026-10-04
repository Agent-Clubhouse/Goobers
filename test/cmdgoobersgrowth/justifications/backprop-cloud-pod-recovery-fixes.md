# backprop/cloud-pod-recovery-fixes: #6306, #6568, #6569

Growth: +6 non-test lines and +0 files in `cmd/goobers`, all in
`cmd/goobers/recoverypublication.go`. The recovery logic itself lives in
`internal/recovery`; the command package only wires it to the instance.

## Why the growth belongs in the command package

- **`EnsureBase` wiring** (+3 lines). Host archive intake needs a way to fetch
  a base commit that is missing from the host's recovery mirror (#6306).
  `internal/recovery` exposes the hook. Only the command layer knows the
  repository manager and remote URL that satisfy it, so the closure is built
  here, next to the other per-instance options in the same struct.
- **Tolerant inventory read in the publication acknowledgement** (+3 net
  lines, two of them a comment). Switching to `recovery.ReadInventoryTolerant`
  stops one crashed reservation from failing every later acknowledgement on
  the instance (#6569). The call site belongs to the acknowledgement, which is
  already in this file.

## Could any of it live elsewhere?

The `EnsureBase` closure could move into a recovery-delivery constructor if
more callers ever need the same wiring. With one caller today, that would add
indirection without shrinking `cmd/goobers` in any meaningful way.