# LAND-E01: standalone and detached starts

The command package owns the one-shot scheduler, instance lock, claim renewal,
detached worker lifecycle and CLI output. A thin adapter now transfers these
existing starts to `internal/startintent` and attempts only the caller's receipt.
It keeps cleanup with the original execution owner and does not drain unrelated
starts. Targeted PR starts consume the shared scheduler's current provider
validation before dispatch, using a separate bounded admission context.

Envelope validation, accepted-definition identity and queue custody remain in
the foundation packages. There is no new queue or credential resolver. The new
command helper only binds these owners and carries the request ID across the
existing detached-selector transport. No growth baseline is changed.
