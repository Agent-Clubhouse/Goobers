# Bounded pending-start scans

The daemon inspects at most 100 accepted ordinary/source/child records in each
queue pass. A shared SQLite cursor orders inspection by acceptance time and ID.
It advances past held or unsupported records, wraps at the tail, and survives
daemon restart. A malformed record reports its error while other records in the
window can still be inspected. Direct engine inputs retain their separate owner.

Reserving a window changes only scan progress. Every start keeps its payload,
acceptance time, state and reserved execution identity until its existing owner
claims or settles it. A crash between reservation and inspection delays untouched
records until the next traversal; it neither loses them nor authorizes execution.
Concurrent store handles reserve windows transactionally. This is not an execution
lease, a promise of execution order, or completion of weighted gaggle scheduling.

The additive migration preserves accepted custody and adds one fixed cursor row
plus a pending-order index. Older binaries refuse the newer queue schema. Queue
inspection, cancellation, deadlines, typed waiting and bounded-time admission
remain separate implementation slices.
