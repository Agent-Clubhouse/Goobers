# Child workflow admission internals (preview)

Child execution is not enabled by this slice. Public runner and engine entry
points still refuse workflows containing opted-in child stages. Offline
`goobers workflow validate-child` remains the usable authoring preview.
The internal service and tools described here prepare the admission boundary for
later workspace custody and launch support. They do not prove an executing
parent can submit, wait for, or reconcile a child.

## Authority and receipt ownership

The daemon derives stage authority from committed journal identities, the exact
retained parent definitions, and current successfully applied configuration.
Agents supply a proposal and invocation key, never credentials, gaggle selection,
parent identity, or a policy override. Stage grants use a separate signing domain
and expire with the harness session. Grants are scrubbed before reaching the
harness; teardown revokes the durable binding.

A grant identifies one parent stage occurrence and attempt. An occurrence keeps
its invocation keys across an authorized replacement attempt; different parallel
stages have independent slots. One unresolved child occupies a stage's slot.
Changing an invocation's source is a conflict. Parent cancellation and grant
replacement are checked in the acceptance transaction, including duplicate reads.

Applied policy reload revokes affected grants before publishing new authority.
An old attempt cannot mint a replacement grant after revocation. A new authorized
attempt can inspect retained child custody even if launch permission has narrowed;
validation and new starts still require current permission. Source and receipt
integrity are verified independently of permission to execute their contents.

## Storage and retention

The existing trigger SQLite database owns acceptance, parent/stage lineage,
source bytes, and authority. This slice adds schema migrations 2–5; reopening is
idempotent and binaries that do not understand the resulting schema must refuse
it. Back up the instance before upgrading. Do not downgrade the database in place
or discard unresolved custody to restore an older binary.

Admission reserves 34,050,048 bytes per authored child for eventual fork/result
artifacts, receipts, and the application plan, in addition to existing retained
bytes. Ordinary admissions respect these reservations. A later workspace slice
must consume reserved credits as it stores artifacts and release them only with
confirmed disposition. This protects completion capacity without claiming that
workspace capture is implemented here.

The store bounds child lineages to 10,000, tombstones to 100,000, and child custody
to a 256 MiB ceiling. The daemon invokes bounded pruning. A settled and acknowledged
family can become a tombstone after 30 days; tombstones retain deduplication for
another 30 days. Unacknowledged children, uncertain launches, or unresolved family
members pin their evidence. Capacity exhaustion refuses admission with a retryable
error; it does not silently delete owned work.

## Remaining delivery gates

Workspace capture/result custody, a recoverable launcher, durable waits, confirmed
cancellation, result disposition, delegated PR publication, contained execution,
and Portal lineage belong to later LAND-C03–C07 slices. Human restart is the common
HITL path. No browser, human, or Fleet identity is inferred from a stage grant.
