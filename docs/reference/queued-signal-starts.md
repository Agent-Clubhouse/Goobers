# Durable signal and webhook starts

Named signals and authenticated GitHub webhook deliveries enter the existing
host start queue before they are acknowledged. Each delivery records its
recipient set and the applied configuration generation, workflow digest and
Goober digest for every recipient. An empty match is also recorded, so a retry
cannot acquire new recipients after a configuration change.

## Operators

`goobers signal --request-id <key> <name> [path]` accepts a named signal while
holding the instance lock. Omit the key to generate and print one before
acceptance. Keep that key to retry after a lost response. The command waits for
its own dispatched runs. A capacity-held start remains durable and exits with
code 1 and a receipt; start `goobers up` to drain it, or retry the same key.
An empty recipient set exits successfully. This command does not delegate to
a running daemon; authenticated webhook delivery is the daemon ingress here.

Webhook HTTP 202 means the recipient set is durably accepted, including an
empty set. It does not mean a run has started. The existing daemon queue drain
performs dispatch. Signature verification precedes acceptance. Delivery identity
binds the complete payload digest, event and delivery metadata; reusing the ID
with different content fails acceptance. Queue/storage failures return HTTP 503
so the sender can retry the same delivery.

## Custody and bounds

Recipient acceptance is atomic, with at most 32 starts per delivery. Archive
leases cover capture through commit; pending receipts retain those generations
across restart. Each source receipt consumes queue capacity, even with no
recipients. It shares the existing storage byte ceiling. Exact replay reuses the
first recipient set and does not acquire the current configuration again.

The seven-day replay retention does not prune a delivery while any recipient
is unfinished. Current scope, subscription and execution eligibility are
rechecked at dispatch. Execution uses the accepted definition. A matching
journal is required before the queue reports a dispatched run.

## Scope and provenance

This is the named-signal and GitHub-webhook adapter of LAND-E01. It narrowly
adapts source receipt and pinned signal pieces from reference snapshot
`fa34a754148ea3076bfd04fe4976a5a461b063f2`. It adds one source-receipt table to
the existing queue; it does not import schedule/demand cursors, interactive
sessions, generic workflow event publication, or Fleet authentication. Those
remain separate landing slices. Existing webhook repository filters and
backoff selection apply. New receipts require this dispatcher version; keep
custody and upgrade if rolling back with outstanding typed starts.
