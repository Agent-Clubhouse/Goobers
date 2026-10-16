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

The admission-only version reserves 34,050,048 bytes per authored child for eventual fork/result
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
slots do not imply that these execution shapes already work. The disposition
follow-up below releases completed child slots only after a verified parent
choice. No recursion is enabled.

## Parent result disposition

The `resolve_child_workflow` tool accepts merge, replace, or discard for the exact
terminal result returned by `get_child_workflow`. Acceptance records intent; an
`applied=false` receipt means the runner still needs to finish the handoff. The
parent must not write through that handoff. A completed child keeps its stage
slot until the chosen disposition is verified and acknowledged.

Merge applies the fork-to-child change to the current parent; replace uses the
child's permitted tree. Both preserve excluded runtime/credential paths and the
parent HEAD, and stage only the changed paths. A newly configured credential path
outside the retained exclusions prevents merge or replace until authority is
reconciled; nonmutating discard remains available. A conflicting merge leaves the
parent untouched. Discard settles verified result custody without importing or
changing parent files and never retracts provider effects such as a published PR.
File/directory shape replacement remains unavailable; a plan is limited to 1,024
changed paths and 16 MiB of before/after content.

The host stops and joins parent writers, checks the current stage authority and
exact disposition revision, and persists a bounded application plan before
changing files. Retrying a published plan accepts only its recorded before/after
states and verifies the resulting files and index. An unrelated edit, changed
HEAD/index, or substituted symlink refuses recovery. The parent remains stopped
while an uncertain published plan needs reconciliation. An untouched preparation
failure can return a bounded explanation to the parent so it can revise its choice.

A replacement attempt must explicitly adopt the prior request by supplying its
`expectedRequestDigest`. Only an unpublished plan can change action; a published
plan must be resumed unchanged. Up to 32 earlier choices are retained as bounded
receipts, then the system refuses further revision rather than deleting history.

Migrations 8–9 add disposition intent and revision history after custody version 7.
Admission now reserves 34,181,120 bytes per authored child, including 128 KiB for
revision history. Migration increases the reservation for existing unresolved
children; exhausted capacity continues to refuse new intake. A family tombstone
releases its associated history, while unresolved plans retain their evidence.
Back up before upgrading; older binaries refuse the newer schema.

This remains internal runtime preparation. Public execution stays disabled until
an isolated backend and its supported parent/child execution shape are qualified.
The tool and local disposition do not implement delegated PR publication.

## Contained worker authority and recovery

Daemon startup connects the retained child launcher to the existing Temporal
worker transport when a shared pod signing key, Temporal client, surrender store
and live journal writer are available. Missing dependencies leave accepted work
queued. The backend requires pinned Linux image runner placement; it never
substitutes an executor on the daemon host. Public opt-in remains gated pending
the complete supported parent/child journey below.

Each physical attempt receives a separately signed generated-child identity
bound to its run and immutable execution contract. Its dedicated HTTP owners
permit only declared artifacts, model credentials under current policy, bounded
observations, result surrender and read-only parent execution monitoring.
Ordinary pod, human and provider mutation permissions are not inherited. A child
observes the parent's real claim deadlines without acquiring or renewing claims.

The driver lends its journal handle to the remote observation owner while it
runs. A lost dispatch reply parks the exact attempt: no retry, finished stage,
review verdict or terminal run is invented. Recovery rejoins the retained worker
identity and requires stopped-writer evidence before importing its result or
starting another attempt. Cancellation revokes execution and credentials while
allowing the original unresolved worker to finish bounded teardown writes.
Cancelling an escalated child settles its invocation without rewriting the
existing escalation history.

Migrations 10–11 add bounded child artifact bytes and per-contract read
membership after disposition version 9. These use the child custody reservation
and retention lifecycle; guessed digests and sibling-attempt artifacts are not
readable. Older binaries refuse the newer schema. Back up before upgrading.

Signed HTTP tests cover factory dispatch, credentials, execution monitoring,
artifact transport, journal observations, surrender and recovery after a lost
reply. They use an in-process worker transport; they do not certify a live
Kubernetes/Temporal deployment or delegated PR publication.

## Remaining delivery gates

Public parent→child→result execution still requires a qualified isolated backend,
delegated PR publication and Portal lineage in the remaining
LAND-C04–C07 slices. Internal custody/wait tests do not qualify a live provider,
pod, Temporal, or Fleet execution journey. Human restart is the common
HITL path. No browser, human, or Fleet identity is inferred from a stage grant.


## Portal child acceptance history

Run detail has a separate accepted-child history view alongside recorded parent
links and current stage waits. The live daemon reads this history from its existing
trigger queue. Offline readers explicitly report it unavailable; an unavailable
source or failed read is never presented as an empty history.

`GET /api/v1/runs/{run}/children?cursor=...` follows the existing run-detail read
authorization. The read service derives the gaggle from the parent's recorded
identity, binds continuation cursors to that parent and gaggle, and returns at
most 50 entries plus an opaque continuation cursor. It performs no reconciliation,
queue creation, artifact reads, provider calls or mutations. Pages use stable child
IDs, not chronological order. Refresh from the first page to include new entries
that may sort before a previous cursor; retention can remove older entries.

Queued, started, needs-human and terminal outcomes are observations of durable
custody. Started does not establish current worker health. A cancellation request
is displayed independently until a terminal result is recorded. Parent
acknowledgement and expired result custody remain visible; expired results have
no recovery link. Child run details may independently have been retained or pruned.
The view refreshes on gaggle run invalidations and offers explicit refresh; it
shows the observation time and uses the Portal's existing stale-data indication.
This read surface does not enable public child execution or new interventions.
