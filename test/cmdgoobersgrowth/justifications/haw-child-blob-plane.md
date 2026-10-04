# HAW-CHD-003 authenticated child blob custody

Daemon startup connects its existing HTTP blob plane to child-scoped custody.
The command adapter resolves the authenticated pod run against the daemon's
queue and local journal before selecting storage; these objects are owned by
this composition root. It does not introduce a second queue or blob service.

The reusable overlay, quotas and retention remain in internal/childpod and
internal/triggerqueue. Cancellation permits only bounded output surrender for
an existing child, while new execution and credentials retain their independent
revocation checks. Child requests never fall through to the shared blob store.
