# Shared claim transition locking

Shared claim admission and cleanup coordinate a remote owner-scoped CAS with the local `claims.json` ledger. The remote record controls cross-instance admission. The local ledger retains cleanup custody, including uncertain acknowledgments, and bounds local execution by the last acknowledged deadline.

## Lock order

1. Resolve trusted run policy and provider configuration without the claims lock.
2. Acquire the per-item transition lock (`claim-release-<key digest>.lock`). Admission, renewal, voluntary cleanup, terminal cleanup and administrative cleanup use the same key and lock.
3. Acquire the claims lock only to reopen `claims.json`, compare the expected incarnation and persist a reservation. Release it immediately afterward.
4. Perform provider reads, CAS writes and visibility repair outside the claims lock. The per-item lock remains held so a local release cannot race a local renewal of the same remote incarnation.
5. Acquire the claims lock again, reopen the ledger, compare the reservation, and finalize. Release the claims lock before the per-item lock.

Never acquire the per-item lock while holding the claims lock. Never call a shared `File` primitive from a caller-owned claims-lock callback. Held file sessions fail closed for shared transitions. `claimsclient.Sequence` labels individual operations without holding an enclosing lock; `Locked` remains available for purely local state transactions.

Administrative revocation is the deliberate exception to waiting for the per-item lock: first persist revocation under the claims lock alone, then release that lock before waiting for per-item cleanup. This immediately stops local execution even when the provider is blocked. Final admission rechecks revocation, so a late remote acknowledgment cannot restore execution authority.

Backlog selection keeps blocked-record reconciliation and its local snapshot in a short claims-lock section, then admits candidates separately. Daemon acquire/release, renewals and recovery use the outer file client rather than a client wrapping an already-held ledger. Recovery treats scan results as hints and compares the persisted incarnation again before mutation.

## Crash and retry contract

A new admission reservation has a shared owner and an already-expired deadline. It is cleanup custody, not execution authority. Renewal retains only its previous acknowledged deadline while admission is pending. An uncertain provider response leaves the reservation persisted; retry uses the same owner and token. Neither expiry recovery nor local-only admission may discard that custody.

A release reservation blocks renewal and reassignment until an acknowledged owner-scoped release, or a validated observation that the old remote owner is gone. A successor's remote record is never released. Restart retries use the persisted owner, claimed-at incarnation and pending state; they do not infer ownership from current workflow settings or provider-visible labels.

The per-item lock is bounded by the existing file-lock timeout. Provider calls use their caller's bounded context. A timeout or crash leaves a durable, retryable reservation; it does not grant admission or silently free the item.
