# Contained workflow attempts

Generated child workflow execution and a parent task that can delegate require
an isolated worker pod. These contracts are implementation building blocks;
general child execution remains unavailable until production launcher admission
and the supported workflow shapes are verified together.

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
harness returns. Failed or uncertain acquire responses are not an instruction
to start another child; the durable occurrence remains authoritative.

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
Parallel parent stages, PR publication delegation, portal child visibility and
shared human intervention on sealed child results remain delivery work. Recursion
is deferred as agreed.
