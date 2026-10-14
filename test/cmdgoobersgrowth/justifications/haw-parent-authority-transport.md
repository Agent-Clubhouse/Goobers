# Contained parent authority and transport (LAND-C06)

The command package composes the existing daemon credential service, retained
configuration loader, run journal, blob store and execution-claim observer for an
exact signed parent attempt. Those process-owned dependencies already live in
cmd/goobers; a separate server would duplicate their lifecycle and authority.

Reusable transport, authenticated routing, signing, bounded storage, Git-state
application, grant retry, and journal attribution live in internal packages.
Command additions resolve the existing pinned/current source and bind those
services to the current daemon. Existing generated-child and ordinary routes keep
their owners. No public parent admission, coordinator, parallel scheduler, or new
identity store is introduced by this slice.

The separate coordinator slice will supply the production parent stage owner and
journal handoff. This layer's worker path and daemon routes are reachable through
the existing contained transport; authentication requires a host-retained exact
contract and custody receipt. Unknown or missing ownership fails closed.
