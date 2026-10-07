# Escalation candidates read the daemon's live read model

Growth: about +8 non-test lines and +0 files in `cmd/goobers`
(`journalreadplane.go` and `up.go`).

## Why the growth belongs in the command package

- **The `reads` field and its wiring** (`journalreadplane.go`, `up.go`). The
  daemon-side journal-plane handler has to be handed the daemon's own live,
  read-model-backed `readservice.Local`, which only `goobers up` constructs. The
  route used to build an offline `FileCrossRun` that scanned every run journal
  on disk, which cannot fit the request budget on a gaggle with tens of
  thousands of runs.
- **The nil guard.** Without a live reader the route refuses rather than
  silently falling back to the offline scan.

## Could any of it live elsewhere?

No more of it than already does. The selection logic stays in `decomposition`,
and the reader-to-wire conversion and gaggle scoping live in `journalclient`
(`EscalationCandidatesFromReads`). Only the handler's dependency and its
wiring are daemon-specific.
