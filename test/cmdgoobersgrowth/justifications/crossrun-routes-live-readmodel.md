# Conflict-touches and unpushed-work read the daemon's live read model

Growth: about +23 non-test lines and +0 files in `cmd/goobers`
(`journalreadplane.go`).

## Why the growth belongs in the command package

- **Routing the two handlers through the live reader.** `ConflictTouches` and
  `UnpushedWork` used to call `FileCrossRun`, which lists every run directory in
  the gaggle (about 20,000 on production) per request. The daemon-side handlers
  now hand `journalclient` the daemon's own read-model-backed reader (`s.reads`,
  wired by `goobers up` since #6896), so only runs with activity in the request
  window have their journals opened.
- **`requireWindowedLiveReads`.** Refuses a request with no window, or with no
  live reader attached, instead of silently falling back to the offline
  directory scan. The HTTP layer already rejects a zero `since`; this keeps any
  other caller of the service from reintroducing the scan.

## Could any of it live elsewhere?

No more of it than already does. The candidate narrowing and both scan bodies
live in `internal/journalclient` (`ConflictTouchesFromReads`,
`UnpushedWorkFromReads`), shared with the file-backed path. `RunPhase` and
`BranchOwnership` needed no change: each resolves one run by id.
