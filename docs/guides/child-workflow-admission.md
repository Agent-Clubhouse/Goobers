# Child workflow admission internals (preview)

Child execution is not enabled by this slice. Public runner and engine entry
points still refuse workflows containing opted-in child stages. Offline
`goobers workflow validate-child` remains the usable authoring preview.
The internal services described here establish admission, workspace custody and
wait coordination. A qualified isolated execution backend is still required
before an executing parent can submit, wait for, or reconcile a child publicly.

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

## Workspace and wait foundation

The custody follow-up adds trigger database migrations 6–7 after admission's
version 5. It consumes the existing completion reservation for the bounded fork
and result artifacts; duplicate writes do not charge twice. Upgrade tests retain
accepted source and receipt identity. Older binaries must refuse the newer schema.

A daemon-owned handoff stops and joins the parent invocation before capturing its
managed repository workspace. A terminal status or a disconnected transport alone
is not proof that writers stopped. Unknown or unjoined writers retain custody and
refuse sealing. The child gets a separate managed fork; capture leaves the parent
files and index unchanged. Result capture retains its snapshot and artifact
receipt without applying it to the parent.

The queued launcher rechecks retained source, current policy, parent cancellation,
and capacity. A journal-publication barrier distinguishes an unstarted receipt
from an observed execution after a crash. Reconciliation continues after dispatch,
including cancellation delivery and independent terminal observation. Cancellation
is confirmed only by observed completion with writer evidence.

For the internally qualified serial-parent path, waiting retains the exact parent
checkout and records context, transcript, usage, and attempt accounting. It releases
concurrency without refunding budget, and reacquires capacity before recording the
continuation. Durable wait time is excluded from the ordinary run-duration limit;
explicit caller cancellation and deadlines still apply. Restart recovery adopts
the retained checkout and refuses malformed or mismatched wait records.

These owners do not enable a public execution path yet. The installed launcher
requires an isolated stage backend and defers when one is unavailable. The local
host process path refuses child execution before credentials or environment are
materialized. Without delegated publication, the credential ceiling allows only
an explicitly delegated model credential; opaque repository/provider and named
MCP tokens are withheld. Filtering a capability label cannot narrow a token's
actual permissions or isolate stored CLI logins.

The current internal handoff is serial and requires a managed repository workspace.
Parallel parent stages, parent pod waits, and the supported contained executor need
separate qualification before runtime enablement. Admission's separate per-stage
slots do not imply that these execution shapes already work. Merge, replace, and
discard are refused here; completed children remain unacknowledged and retain their
slot and evidence until the disposition follow-up lands. No recursion is enabled.

## Remaining delivery gates

Public parent→child→result execution still requires a qualified isolated backend,
result disposition, delegated PR publication, and Portal lineage in the remaining
LAND-C04–C07 slices. Internal custody/wait tests do not qualify a live provider,
pod, Temporal, or Fleet execution journey. Human restart is the common
HITL path. No browser, human, or Fleet identity is inferred from a stage grant.
