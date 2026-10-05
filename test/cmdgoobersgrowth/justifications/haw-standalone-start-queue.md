# HAW-EVT-002: standalone and detached queued admission

The command package composes its existing one-shot instance lock, archived
configuration builder, normal scheduler, and shared durable start service. The
new adapter replaces the direct manual/targeted-PR trigger block and attempts
only the caller's receipt. Generic pinning, replay, queue storage, admission and
reconciliation remain in internal/startintent and internal/triggerqueue.

The detached selector carries the existing caller request ID to the worker;
these CLI protocol helpers and receipt/error output belong beside that worker.
No independent queue, persistent store, or alternate execution path is added.
