# HAW-EVT-004: pinned event consumer host integration

This slice adds the daemon adapters that construct an existing runner from a
retained configuration archive, install queued event routing, and discriminate
retained event recovery. These depend on private scheduler composition and journal
layout seams in cmd/goobers. Existing generic trigger dispatch and scheduler
admission remain the sole host start boundary.

The new execution service, input provenance, typed start wrapper, settlement,
queue inventories and prepared scheduler admission live under internal packages.
The command additions contain no separate event protocol, storage engine or
workflow interpreter. Composed tests exercise the actual archive builder,
scheduler, tracked starter, Runner.Start/Resume and durable queue without network.
