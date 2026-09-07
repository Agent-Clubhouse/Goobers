# Inspect claim verification

Run `goobers claims list --json <instance-root>` to inspect authoritative leases
and their last provider observation. Listing does not contact the provider or
renew a lease. Each entry includes `verification`:

| State | Meaning |
| --- | --- |
| `unverified` | No recorded provider observation for this ownership period. |
| `verified` | The provider confirmed the ledger owner's claim. |
| `missing` | Reconciliation found the provider claim marker absent. |
| `ownership-mismatch` | The provider reported another owner; `providerRunId` names it. |
| `unavailable` | The attempted provider operation could not establish ownership. |

`observedAt` is the observation time, not a promise of current provider state.
An unverified entry has a zero timestamp. A visible label alone does not prove
ownership and does not refresh a verification result. Inspect `expiresAt` and
`releasedAt` separately: a historical verified observation does not make an
expired or released lease active.

Normal backlog claiming records the provider result after reading the exact
ledger lease. The existing invisible-claim reconciliation records missing
markers and its restoration result. Recording never grants, extends, releases,
or changes ownership. Replaced leases and out-of-order observations are rejected;
a live same-owner renewal retains the original observation time. A lease acquired
after release or expiry starts unverified. Recording failures are reported but
do not alter the existing provider arbitration or claim rollback behavior.

Local stages record through the existing locked ledger; remote stages use
`POST /api/v1/claims/verify` through the claims plane. The request identifies the
calling run separately from the observed owner. Pod callers may report only
within their admitted gaggle. The daemon compares the owner and acquisition time
before updating the bounded observation stored with the existing retained lease
history. This endpoint records stage-reported observations; it is not an
independent provider probe or an ownership authority.

Provider claim and release attempts carry the active claim run ID and a
success, failure, or conflict outcome. Stage sidecars carry these facts to the
owning local runner or engine, which journals failed/conflicting attempts as
`error` events, not successful `ref.touched` mutations. Raw provider error text
and credentials are not copied into this telemetry.

Ledger/provider ownership mismatches additionally produce error code
`provider_ledger_ownership_mismatch`, with `runner.claimRunId` naming the ledger
owner and `runner.providerRunId` naming the observed provider owner. The enclosing
journal remains attributed to the executing stage's run. Alert on this code and
inspect both ownership histories; do not force-release a live lease merely
because its provider marker differs.

Legacy unscoped reconciliation remains explicitly skipped when no gaggle can be
resolved. It does not claim to have checked an inaccessible namespace.
