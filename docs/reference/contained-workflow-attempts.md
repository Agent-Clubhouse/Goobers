# Contained workflow attempts

Generated child workflow execution and a parent task that can delegate require
an isolated worker pod. The first connected parent lane supports serial opted-in repository agents with
explicit Linux image placement and configured worker transport. Unsupported
workflow shapes are refused. Parent and child host recovery use the shared
executor to reconcile a retained worker without launching a replacement.

## Parent task authority

A signed parent pod credential names a run and one exact execution-contract
digest. It uses a different signature domain from ordinary and generated-child
pod credentials. The daemon reads the contract from that run's bounded retained
custody, verifies the journal identity, and matches the active stage, logical
attempt, physical stage-start sequence and start time. Request bodies cannot
supply a different policy, actor or parent occurrence.

`POST /api/v1/runs/{run}/child-workflow-access` accepts only the signed
`contractDigest`. It returns a short-lived delegation endpoint and bearer token
with `Cache-Control: no-store`. The token is registered for redaction before
return. `DELETE` on the same route revokes that occurrence's matching attempt.
An old attempt cannot revoke its replacement. The pod closes access when its
harness returns. Retrying an uncertain exchange recovers the same live grant
nonce and expiry after checking current authority; it does not extend the grant.
Revoked, cancelled or replaced attempts cannot recover it. No bearer is stored
at rest. The durable occurrence remains authoritative for child invocation.

The parent credential resolver reconstructs its archived task and current gaggle
policy. The initial contained lane delivers model credentials only. Provider,
external MCP and refresh credentials remain unavailable in that lane. The
resolver checks the durable parent-cancellation fence again before delivering
credentials. This does not revoke a credential value already delivered; process
cancellation and pod termination remain necessary.

## Artifact and observation custody

Parent custody is owned by the run journal and partitioned by signed contract.
The trusted launcher permits reads of the contract, execution kit and selected
context. Pod writes become readable only within that same attempt. Guessed
sibling or shared blob digests do not authorize access. Custody limits are
64 MiB and 512 entries per parent run, with a 24 MiB single-object bound; index
records count toward those limits. Retention follows existing run-family holds.

Contained pods may emit bounded execution observations, artifacts, spans and
transcript checkpoints. They cannot create journals, write control-flow events,
claim source trust or access operator/trigger/state mutation routes. Parent
observations bind to the exact active stage and logical attempt. Journal keys
and transcript captures are namespaced by physical contract. Inline artifact
publication, span reads and artifact-reference adoption all use the request's
scoped store, including bounded reads; none fall back to shared blob storage.

An exact host-started, unjoined parent or child contract retains bounded teardown custody
after its stage stops. Credentials and new-child grants still require active
authority. Once the run journal is terminal, fresh direct observations remain
refused; final transcript and workspace artifacts can still be uploaded and
surrendered for host recovery. A joined or replaced worker loses this custody.

Parent completion records must name the same run, stage and physical attempt.
The output carrier must match the retained contract. Returned artifact pointers
must exist inside the attempt and match their declared sizes. Ordinary workspace
bundles, provider mutation claims and reviewer verdicts cannot be substituted
for contained task output.

## Completion and cancellation

The worker owns Kubernetes pod creation and termination; the daemon does not gain
Kubernetes permissions. The dispatcher uses exact pod UID/resource-version
custody. A supervisor stops and reaps workspace writers before surrendering the
bounded output. Parent cancellation cascades to unfinished child work.

A stopped pod alone does not return workspace custody. The host must validate and
import the output tree and retain its proof before acknowledging writer
quiescence. Failed output import preserves a refusal; it cannot cause the parent
to resume with an older checkout presented as the returned child workspace.

## Evidence and remaining scope

In-process HTTP acceptance covers signed grant issuance/revocation, sibling blob
refusal, scoped journal publication/reference reads, completion binding and
credential refusal after cancellation. Queue, contract, worker and executor race
tests cover durable custody and import failures. Human restart acceptance covers
HTTP saved guidance, fresh retry allowance, recovery and current-policy refusal;
its model process and provider transport are simulated.

Live Kubernetes, Temporal and model execution are not claimed by those tests.
Parallel parent stages, PR publication delegation and shared human intervention
on sealed child results remain delivery work. Recursion
is deferred as agreed.

## Generated child attempts and retained parent work

Generated child credentials also name an exact execution-contract digest in a
separate signature domain. The daemon checks accepted lineage, the pinned task
or reviewer, logical attempt, physical stage-start sequence and timestamp. Each
contract has durable read memberships for its kit/context and own uploads inside
the existing bounded child store. A sibling stage cannot read those uploads,
resolve another stage's credentials, emit observations for it or surrender its
result. These memberships survive a database reopen and share the existing
per-child row/byte bounds and retention.

Parent worker launch now holds the original managed checkout and records the
host fork beside its exact contract. Only host-authored writer-start/join records
can release that custody. Retry, Resume and Rerun inspect it before replacement
journal effects. An uncertain worker preserves the checkout; a terminal run
record alone does not prove that worker stopped or returned its edits.

The shared reconciliation engine retains the original dispatch binding and host
snapshot before launch. Recovery addresses the existing Temporal workflow ID,
stops/joins it and verifies its exact payload binding; it never submits another
start. Returned edits require both stopped-writer evidence and a matching output
carrier. Workspace application records the exact plan before effects, so a crash
replays that plan instead of adopting a later checkout as a new merge baseline.
Conflicting intervening edits retain custody and require resolution. Parent host
recovery imports the original checkout before recording writer join; an authorized
continuation then reuses it. Child recovery uses the same contract, preserves its
RunID and rechecks the durable parent fence before resuming execution. Recovery
reservations prevent terminal result capture from racing a new child owner.
Recovery observation can finish after cancellation, while fresh execution still
requires current admission and credential authority.

The completion boundary checks both inline outputs and reviewer evidence against
bounded attempt custody. It rejects claimed source-trust grades, inconsistent
sizes and non-reviewer verdicts. Successful reviewers require a schema-valid
verdict; failed reviewers may surrender their failure and workspace without one.

## Acceptance before parent wait publication

Before an opted-in attempt starts, the host records its policy-attempt count,
infrastructure failures and cumulative usage. If child acceptance commits before
parent wait publication, recovery observes that same accepted child and restores
the wait with the original accounting, context, guidance and workspace. It does
not consume another attempt or dispatch a continuation worker while the child is
unsettled or capacity is unavailable. Missing legacy accounting is refused rather
than reconstructed from guesses. Pods cannot supply this host accounting record.

A terminal parent remains terminal during custody recovery. Its existing
authorized continuation must reopen it before the pending child wait is restored;
parent cancellation still prevents continuation. A terminal child result remains
deliverable even when its original grant has expired or been revoked.

## Portal child monitoring

The run page displays up to 50 child records at a time, with explicit next-page
and refresh controls. It shows queue state, cancellation requests, acknowledgement
and expired detail retention. A child run link appears only after its actual
journal identity matches the accepted lineage; reserving a RunID does not imply
execution has started. Child runs also show their parent workflow link.

`GET /api/v1/runs/{run}/children` requires verified human view access and applies
the current gaggle viewer policy. If interactive policy is absent, authorized
instance viewers retain read-only monitoring. Internal pod identities cannot use
this surface. Responses are not cached and expose neither generated source nor
workspace paths, credentials or result bodies. Revoked access clears previously
shown family data on the next refresh. Queue transitions currently require manual
refresh; existing run detail updates do not promise live child state.

## Parallel wait accounting groundwork

Journal projection now tracks each branch's exact wait independently. A declared
but unstarted sibling remains runnable; a waiting branch cannot release that
sibling's whole-run capacity. The whole-run execution clock excludes only periods
when every remaining branch waits, while a branch clock excludes its own waits.

The parallel coordinator serializes wait/continued/finished publication with
whole-run scheduler suspension and reacquisition. A final runnable sibling ending
can release the parent's slot; one ready branch reacquires it once before its
continued marker. Failed durable publication stops the coordinator for recovery.
Old branch wait handles cannot resume a later wait. Real scheduler race tests
cover these boundaries and unchanged start allowances. Branch lane scheduling,
contained branch factories and writable fork/join integration are still required
before parallel child delegation is enabled; the current shape gate remains.

Contained factories now accept only a runner-owned root or branch journal
recorder. Host writer receipts retain branch attribution, and one branch's join
cannot release a sibling's writer. Whole-run recovery still requires all pending
scopes to join, processing distinct held workspaces in physical-attempt order and
refusing ambiguous shared-workspace recovery receipts. This does not enable the
unfinished parallel execution lane.

## Discard after workspace permission changes

A new discard plan only acknowledges the exact verified terminal result. It does
not import the child's carrier, capture the parent's current files or require a
writable checkout path. This allows an authorized current parent occurrence to
discard after workspace mutation permission or exclusion policy narrows. The
retained result and repository binding must still verify, and stale grants remain
refused. Existing published application plans are immutable and still follow
their recorded recovery path before the occurrence can be released. Discard does
not retract a child PR.
