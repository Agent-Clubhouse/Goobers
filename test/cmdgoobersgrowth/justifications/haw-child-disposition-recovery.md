# HAW-CHD disposition recovery composition

The daemon's existing child-stage loader now separates current parent ownership
from permission to launch another child. A small composition helper loads the
current enabled parent with the ordinary config compiler even when its child
opt-in has been removed. This permits an authorized replacement attempt to inspect
or discard previously accepted custody while retaining current workspace write
restrictions and rejecting new execution.

The daemon handoff adapter selects immutable retained custody for discard, binds
an exact disposition request revision, and maps preparation/reconciliation
outcomes into the runner's existing stopped-writer wait. Immutable source checks,
request CAS, bounded revision history, storage reservations, pruning, durable
wait handling, and wire parsing remain in their internal packages. These daemon
changes compose the applied catalog, pinned generation, existing workspace
coordinator, and live runner ownership; they do not resolve or persist secrets.

The root stack owner will re-pin the aggregate command baseline with the exact
final counts after concurrent daemon composition changes are integrated.
