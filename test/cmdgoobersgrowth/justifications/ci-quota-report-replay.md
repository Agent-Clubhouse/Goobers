# CI quota waits and durable terminal reports (#7668)

The command additions wire the internal terminalreport outbox into existing
failure-comment and abort-label finalization, resolve current provider
credentials for replay, and start/stop replay with the daemon. These adapters
need the command's private provider factories, credential configuration and
run-journal ownership rules. Durable storage, locking, due-time selection and
publication outcomes live in internal/terminalreport; quota classification and
polling waits live in providers and internal/executor.

Pod event emission also gains a context-aware entry point so a quota-wait event
cannot outlive its stage deadline. ADO terminal abort labeling remains explicitly
unsupported; enabling that capability is a separate follow-up.

The author matcher resolves the same configured backlog connection as comment
publication, and delegates stable identity checks to internal/gate. This wiring
prevents another commenter from acknowledging a forged terminal intent marker.
