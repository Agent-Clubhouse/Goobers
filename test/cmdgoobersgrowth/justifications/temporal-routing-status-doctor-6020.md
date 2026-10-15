# #6020: Temporal worker routing in `goobers status` and `goobers doctor`

Growth: about +92 non-test lines and +1 file in `cmd/goobers`.

## What lands in the command package

- **`statusenginerouting.go`** (new, about 80 lines, roughly half doc
  comments). This is status-board wiring only. It gates the probe on an
  engine-configured instance and builds the probe target from the instance's
  engine config. It caches the verdict for a 30-second window so `--watch`
  does not dial Temporal on every redraw, and it attaches the verdict to the
  text board and `--json` through `statusRunLoader`. Those loader hooks keep
  `runRunTable` from growing past its complexity baseline.
- **Help text** in `doctor.go` and `status.go` (about 9 lines). These document
  the new check row and the status line. They are rendered into the man pages,
  `docs/cli`, and the help golden.
- **One `statusRunLoader` field** (`statusprojection.go`).

## What lives elsewhere

All the logic lives in the new `internal/workerrouting` package:

- the routing rule (moved out of `internal/workerhost`, so the worker log,
  status and doctor share one implementation);
- poller classification and the fleet verdict;
- the remediation commands;
- the text line and the JSON shape;
- the Temporal dial.

The doctor check sits in `internal/k8spreflight` beside `temporal-namespace`.
The command package keeps only CLI composition: deciding when status probes,
and caching for `--watch`.
