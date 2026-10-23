# Temporal read-only inspection (#4084, #4085)

This PR adds exactly 14 net non-test Go lines and zero non-test Go files to
`cmd/goobers`, measured against `395cf74f45765173ccb2e092aa11f57a1477c019`:
153,976 → 153,990 lines and 569 → 569 files.

The command package adds seven lines for the `temporalops` import and the two
read-only command registrations in `runtime_capabilities.go`, plus seven lines
for their flags in `completionmodel.go`. Existing Temporal synopsis and help
lines are replaced without increasing the count. These registrations belong in
the central CLI registry and completion model so the commands are reachable and
the existing command/help/flag audits cover them.

The implementation remains outside the command package: `internal/temporalops`
owns parsing, bounded input and output; `internal/workerrouting` owns the bounded
native deployment read; and `internal/workercontract` owns local exact-source
compiler inspection. No rollout mutation or runtime admission is added. The
shared informational growth snapshot remains unchanged.
