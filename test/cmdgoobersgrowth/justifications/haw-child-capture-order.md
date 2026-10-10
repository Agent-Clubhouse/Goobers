# Child launch waits for the durable parent capture

Growth: +9 non-test lines and +0 non-test files in `cmd/goobers`, in
`childlauncher.go`.

## Why this belongs in the command package

The daemon launcher must confirm that the parent snapshot receipt is durable
before it reserves a worker or publishes launch intent. Scratch children need
this ordering too: spare worker capacity must not let a child enter Running
before the parent can retain its snapshot. The existing snapshot state guard
continues to reject snapshots supplied after launch.

## Why no new package

This is one readiness check at the existing daemon launch boundary. The journal
already owns the snapshot state and its validation; moving the check into a new
package would add an adapter without moving domain behavior. Focused tests cover
pending capture, retained capture, and invalid or missing capture evidence.
