# #5638: failure streak honours journaled infrastructure; engine watchdog cancels

Growth: about +139 non-test lines and +1 file in `cmd/goobers`. Roughly half
of it is doc comment.

## What lands in the command package

- **`infrafailurestreakstate.go`** (new, about 78 lines). This is the
  persistence for the separate infrastructure failure streak:
  `escalationNotifyState.LoadInfraFailureStreak` /
  `WriteInfraFailureStreak` and their key.
  - It reuses the work streak's existing scheduler-state record, CAS update
    and audit annotation from `failurestreakstate.go`, all of which live in
    this package. It adds only a distinct key and a load path with no legacy
    migration.
  - `escalationNotifyState` is the command package's implementation of
    `escalationnotify.State`, so the new interface methods must be
    implemented here.
- **`stalledengineruns.go`** (about +67 lines). This makes the stall sweep
  record *why* it cancelled an engine run, and lets the failure streak read
  that back:
  - the `interrupt` field on its `engine_cancel_requested` annotation, written
    before the cancel;
  - per-run interruption and operator-cancel tracking in the sweep's existing
    `engineCancelIndex`;
  - `engineWatchdogInterruption`, which reads the index.

  The sweep, its instance-log record and its incremental index already live
  here, and a second reader of the same annotations elsewhere would
  duplicate the index.
- **`runnerwiring_notifications.go`** (+3 lines). This wires
  `Policy.EngineInterruption` to the sweep's record.

## What lives elsewhere

The decisions are outside the command package:

- the terminal classification (`runner.JournaledTerminalFault`,
  `EndedByCancellation`);
- the two-streak breaker and its charging rules (`internal/escalationnotify`);
- the infrastructure-streak comment (`internal/gate`).

The command package only persists the counter and supplies the sweep's own
evidence.
