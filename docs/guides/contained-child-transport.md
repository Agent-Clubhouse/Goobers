# Contained child worker transport

## Availability

This is the worker transport foundation for generated child workflows. The daemon
connects an isolated child factory when its Temporal client, configured pod signing
key, surrender store and journal service are available. Narrow authenticated
child routes serve retained kits, credentials, journal observations and surrender.
Missing execution dependencies leave accepted children queued.

Public parent workflow execution with child-workflow policy remains gated. The
connected components have separate qualification tests; they do not yet establish
a supported complete parent-to-child-to-parent execution journey.

The [child workflow design](../design/agent-authored-child-workflows.md) and
[admission guide](child-workflow-admission.md) track the remaining lifecycle work.

## Private workspace carrier

The worker receives a canonical, digest-bound contract naming the admitted child,
logical stage attempt, physical pod attempt, retained configuration and credential
ceiling. Carrier bundles are limited to 16 MiB; the complete JSON contract is
limited to 24 MiB. Unknown fields, substitutions, invalid lineage and publication
permission are refused.

A carrier contains the permitted tree on a synthetic root commit. It carries no
source commit history, provider remotes or credential helpers. Import verifies
bundle metadata, root ancestry, tree identity and excluded paths before creating
a workspace in an empty private directory. Preparing and importing carriers moves
neither the real parent's HEAD nor its index or working files. Applying a returned
tree belongs to the separate host execution/result-custody slice.

## Worker and pod lifecycle

The dedicated dispatch path requires Linux PID 1 in a private process namespace.
Its pod uses private volumes and environment settings; inherited host credentials,
shared HOME and caller-supplied pod bearers are refused by the dispatcher. The
contained worker stops descendant processes before capturing output and reporting
surrender. Successful foreground command exit alone does not prove writer exit.

The dispatcher records the pod's immutable UID and requires observed writer
termination plus confirmed surrender before releasing its custody finalizer.
The reference dispatcher role adds the Kubernetes `patch` verb on pods to release
that finalizer after confirmation. Cancellation initiates graceful termination and bounded observation; socket or
pod disappearance alone does not count as successful cancellation.

Ordinary orphan cleanup keeps its existing retention policy. An isolated child
with unconfirmed custody remains retained after that ordinary hold expires;
verified result custody must settle it. The same unattended orphan sweep owns
this distinction. Tests exercise the sweep with an expired, stopped child and
confirm that unverified results are not deleted.

## Follow-up requirements

The connected factory and authenticated routes cover exact signed attempt
authority, child-only artifact storage, retained agent kits, returned-tree
application and recovery of an uncertain worker. Before public runtime enablement,
qualify these together with the originating parent invocation, durable wait and
continuation. A simulated worker transport or an isolated process-shutdown test
does not establish that full journey. Contract digests and pod environment
comparisons do not replace the host's authentication checks.

### Private Linux process-namespace qualification

The opt-in `TestIntegrationPID1QuiescesDetachedWriter` test runs the production
supervisor as PID 1 under a non-root identity. It launches a detached writer,
checks that PID 1 adopted it, then verifies that quiescence stops and reaps it.
A cancelled observation must refuse to claim quiescence while the writer still
runs. This qualifies the process cleanup primitive; it does not prove the full
daemon, queue, worker and parent-continuation journey.

Use a disposable local Linux container with a private PID namespace, `sh` and
`sleep`. The test intentionally stops all other processes in that namespace.
Set `GOOBERS_QUALIFICATION_IMAGE` to a locally prepared runner image, then run:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -tags=integration \
  ./internal/childpod -o /tmp/goobers-childpod.test
docker run --rm --platform linux/amd64 --network none \
  --user 65532:65532 --cap-drop ALL --security-opt no-new-privileges \
  --read-only --pids-limit 64 --memory 256m --cpus 2 \
  --tmpfs /tmp:rw,nosuid,nodev,size=64m \
  --mount type=bind,source=/tmp/goobers-childpod.test,target=/qualification/childpod.test,readonly \
  --env GOOBERS_CHILD_PID1_TEST=1 --env TESTDEP_STRICT=1 \
  --entrypoint /qualification/childpod.test "$GOOBERS_QUALIFICATION_IMAGE" \
  -test.run '^TestIntegrationPID1QuiescesDetachedWriter$' -test.v -test.timeout=30s
```

The ordinary integration suite skips this test unless explicitly opted in.
Neither cluster access nor provider or model credentials are needed.

Delegated branch/PR publication follows that host factory and requires the
parent's upfront permission. Parallel parent stages and human intervention use
their own subsequent lifecycle qualification. This slice introduces no storage
migration and grants no new provider or human permissions.

## Recorded child activity in the Portal

Run details project the generated run's recorded parent and its current durable
child waits. Each wait identifies its stage, branch, child run and requested
handoff action. A whole-run waiting message appears only when the journal accounts
for all unfinished branches as waiting. A recorded continuation removes its wait.
Malformed or mismatched custody is shown as unavailable.

This read-only view uses the existing run-detail read surface and permissions.
Links identify recorded runs; queued execution and expired history can make a
linked run unavailable. The projection covers recorded waits, with the complete
accepted-child queue, history and intervention controls still requiring the
remaining Portal and HITL work. It provides no cancellation or stopped-writer
confirmation. Runtime enablement continues to require the qualification above.

## Disposable Kubernetes qualification

`TestIntegrationQueuedChildUsesRealKubernetesWorker` exercises the actual durable
queue, retained runtime builder, child runner, Temporal server/worker, Kubernetes
pod dispatcher, signed worker API, process shutdown, terminal result capture and
parent disposition acknowledgement. The child runs a deterministic shell command
in a scratch workspace. The originating parent stage and its wait/continuation
records are fixtures. This does **not** qualify agent-authored child creation,
managed-repository return, public parent admission, parallel parents or recovery
from a lost real worker.

This opt-in test is intended for Docker Desktop and a disposable **kind** cluster.
It never reads the default kubeconfig: an explicit file, a `kind-haw-child-` context
and a literal-loopback API endpoint are required. The test creates and deletes its
own namespace. It serves a private test daemon on host loopback, reached from the
worker through Docker Desktop's `host.docker.internal` address. Its signing key and
all run state are synthetic test data; no provider or model account is used.

Prepare kind following its [quick start](https://kind.sigs.k8s.io/docs/user/quick-start/)
and [local registry guide](https://kind.sigs.k8s.io/docs/user/local-registry/), with:

- a cluster name starting with `haw-child-`;
- a separate kubeconfig file passed to `kind create cluster --kubeconfig`;
- a registry published only at `127.0.0.1:45081`, with the node's
  `localhost:45081` registry host mapped to that registry container;
- the node and worker image built for the same architecture.

Build the worker from the checkout under test. For a temporary image context,
copy the Linux `goobers` binary beside this minimal Dockerfile:

```dockerfile
FROM docker.io/library/node:22-bookworm-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5
RUN apt-get update && apt-get install --no-install-recommends -y git ca-certificates && rm -rf /var/lib/apt/lists/*
COPY goobers /usr/local/bin/goobers
USER 1000:1000
ENTRYPOINT ["goobers"]
```

Use a unique image tag such as `qualification-<commit>`, embed that same value in
`internal/version.Version` with `go build -ldflags`, and push the image to
`localhost:45081/goobers:<tag>`. The test retains the production `Always` pull
policy, signature checks, namespace isolation and exact pod-UID custody checks.
Provide the repository-pinned Temporal CLI (`v1.8.2`) through
`GOOBERS_TEMPORAL_CLI`, then run:

```sh
GOOBERS_CHILD_KUBE_QUALIFICATION=1 \
GOOBERS_CHILD_QUALIFICATION_KUBECONFIG=/path/to/private-kind-kubeconfig \
GOOBERS_CHILD_QUALIFICATION_IMAGE="localhost:45081/goobers:qualification-<commit>" \
go test -tags=integration ./cmd/goobers \
  -run '^TestIntegrationQueuedChildUsesRealKubernetesWorker$' -count=1 -timeout=8m -v
```

A passing run reports the real pod UID, confirmed writer termination and durable
surrender, then verifies the parent's authenticated result decision and releases
its unfinished-child slot. A failed or interrupted run may retain an unconfirmed
pod through its custody finalizer. Delete the **disposable cluster** and its local
registry when qualification ends; do not clear production custody finalizers to
imitate a passing test.

### Cancelling an active child pod

`TestIntegrationParentCancellationStopsRealKubernetesChild` uses the same
explicit disposable environment. Its child shell signals that execution has
started before the test sends a parent cancellation through the daemon cancel
service. The durable family fence records cancellation intent; the bounded queue
sweep then delivers it to the live child runner. The test requires a cancelled
retained result and independently verifies the exact pod UID, stopped workspace
writers and durable surrender. A cancellation request alone is never treated as
stop confirmation.

The startup signal is a test-only loopback HTTP endpoint reachable from the
local container; it grants no Goobers authority. Cancelling before command
startup can instead preserve a failed child outcome when the execution fence
refuses startup first. This does not rewrite an already-recorded terminal
outcome. The originating parent remains a fixture, so this test does not qualify
cancellation of a complete agent-authored parent/child journey.


### Parent-authored journey qualification (preparation branch)

The contained-parent preparation now has a complete sequential journey through
normal queued admission, the scheduler and runner, a contained parent, actual
Goobers MCP child tools, a generated child, durable wait, result disposition and
parent completion. These tests exercise the Portal's production read service
for the parked stage, accepted-child history and child-to-parent link.

The external model is replaced by `cmd/goobers/testdata/qualification-claude.cjs`.
The production Claude adapter launches that deterministic fixture, which launches
the supplied real Goobers MCP server. The host's test Claude executable supports
preflight only and refuses inference, making accidental local execution fail.
Temporal, Kubernetes, signed credential/blob/journal APIs and PID 1 cleanup remain
real. Forge and model credentials are synthetic; no external account is used.

For the parent image, use the Dockerfile above and copy the fixture into the image
as `/usr/local/bin/claude` with executable permissions before the `USER` line.
Build the binary and image with a matching `haw-parent-<commit>` version/tag, and
set `GOOBERS_PARENT_QUALIFICATION_IMAGE` to its local registry reference. Reuse the
explicit kubeconfig and Temporal CLI described above:

```sh
GOOBERS_CHILD_KUBE_QUALIFICATION=1 \
GOOBERS_CHILD_QUALIFICATION_KUBECONFIG=/path/to/private-kind-kubeconfig \
GOOBERS_PARENT_QUALIFICATION_IMAGE="localhost:45081/goobers:haw-parent-<commit>" \
go test -race -tags=integration ./cmd/goobers \
  -run '^TestIntegrationContainedParent(AuthorsChildThroughRealWorkers|ReconcilesChildWorkspaceThroughRealWorkers|CancellationStopsAuthoredChild|IteratesChildrenThroughRealWorkers)$' \
  -count=1 -timeout=15m -v
```

The scratch-child case verifies creation, completion and discard. Repository
cases separately verify merge, replace and discard: the child sees the parent's
pre-fork uncommitted file, commits its own new file, and the resumed parent checks
whether that file and a later parent-only file were retained as each decision
requires. Every physical invocation must report its exact pod UID, stopped
writers and surrendered result, with confirmed cleanup.

The scratch/Portal journey and all three repository decisions passed with host
race detection in the disposable environment on 2026-10-09. The repository cases
first exposed a Kubernetes finalizer version race; their passing rerun includes
the bounded, exact-UID retry. This is preparation-branch evidence, not public
activation or qualification of parallel parents, real daemon/worker-loss recovery,
delegated PR publication.

The authored-parent cancellation case waits for the generated child's actual
shell startup before sending a persistent daemon cancellation request. The
response reports `cancellation_requested` while the parent is aborted and child
custody still prevents retirement. This is request acceptance, not proof that
all work stopped. The test separately requires the cancelled child result, both
workers' exact stopped/surrendered pod custody and cleanup, and stable replay of
the cancellation receipt. It also verifies that the unacknowledged child still
protects the original parent checkout and its pre-child work. Cancellation does
not silently acknowledge or discard that result.

Expected child-retention errors are distinguished from other failures by their
typed causes. Storage, permission or unrelated cleanup failures remain errors;
they cannot be hidden by a pending child in the same aggregate. The full
cancellation case passed with host race detection on 2026-10-09. These proofs
still do not qualify real daemon/worker-loss recovery or parallel parent starts.

The iterative-child case completes two scratch child workflows from the same
parent stage visit. The second is accepted only after the first result is
acknowledged, under a distinct invocation key and the next occurrence sequence.
It verifies both retained outcomes and Portal parent links, five contained
parent invocations, two actual child workers, and exact physical custody for
every invocation. This passed with host race detection in the disposable
environment on 2026-10-09 (112.03 seconds). No recursion or parallel parent
admission is enabled by this qualification.

The SDK dispatch-worker restart probe stops and recreates the actual Temporal
workers while the first generated child's shell is active. Its first run exposed
a lost completion: SDK 1.49 outbound processing used the stopped worker's
cancelled background context and replaced its final custody report with a
context-cancelled activity failure. The candidate repair completes the exact
activity through the existing authenticated Temporal client and returns the
SDK's pending-completion signal only after that completion succeeds.

With that repair, the two-child restart journey passed with host race detection
on 2026-10-09 (133.02 seconds): the interrupted child remains failed, the parent
acknowledges and discards that result, then authors a distinct successful child.
Both children remain in history; all five parent and two child pod invocations
require exact UID, stopped writers, surrender and disposal proof. A separate
real Temporal test verifies a denied completion cannot become successful custody.

The same two-child journey now runs through the production worker Host:
shutdown stops all queue pollers, preserves the pending-completion sentinel,
and keeps its authenticated client alive for bounded child custody settlement
before closing it. A fresh Host opens its own client and resumes from Temporal
history. This passed with actual Kubernetes pods and host race detection on
2026-10-09 (135.91 seconds). A separate real Temporal Host regression verifies
exactly one durable completion and no duplicate dispatch.

After the configured SDK drain, only unfinished child dispatch activities get
up to five additional minutes for pod termination, surrender, disposal and
completion. Expiry still reports abandoned work. The reference deployment
allows 360 seconds for the default 30-second drain, settlement and process exit;
increase that pod grace when increasing the drain timeout.

This qualifies graceful production Host shutdown/recreation in a surviving
test process. It does not qualify abruptly killing the worker process or
restarting the daemon. Those recovery journeys remain part of the public
activation gate.


### Abrupt dispatch-process loss

A separate qualification starts the production Host in a subprocess, waits
until a generated child shell is running and Temporal has retained its exact
API-observed pod identity, then kills the process without running its cleanup.
The original regression failed after 35.47 seconds: the activity's heartbeat
timeout closed its dispatch workflow while the original pod was still alive.

The dispatcher now offers the observed namespace, name and UID to the child
activity's heartbeat recorder. The heartbeat binds that observation to the
immutable dispatch input. After a heartbeat timeout, a versioned workflow path
can schedule one bounded reconciliation activity on the existing dispatch
queue. It verifies the instance, owning workflow, run, stage, physical attempt,
contract and original UID before conditionally stopping that pod. It observes
writer termination, confirms surrender and disposes the retained object. This
path has no create fallback and leaves the parent in charge of retries.

With this repair the actual process-kill journey passed on 2026-10-09 with host
race detection (149.48 seconds): both child results remain in history, the
parent explicitly discards the interrupted result and authors a second child,
and five parent plus two child physical attempts retain exact custody proof.
Existing real Temporal cancellation and graceful completion regressions also
passed. Tests refuse absent/mismatched heartbeat custody, changed pod identity,
missing writer/surrender proof and replay of histories predating recovery.

This evidence covers loss after the API identity reached Temporal. Loss before
that first durable receipt still fails closed: a pod name alone cannot release
custody or authorize another execution. Loss during reconciliation and daemon
process reconstruction remain separate recovery qualification work. Public and
parallel activation remain gated.


The independently runnable child fixture also includes
`TestIntegrationChildRecoversAfterDispatchProcessLoss`: one durable child start,
an actual dispatch-process kill, a fresh worker, and exact original-pod custody
reconciliation without another dispatch. It passed with host race detection
(23.93 seconds). This fixture can travel with the isolated recovery PR while
the broader agent-authored parent journey remains on its preparation branch.

### Daemon process loss while a parent waits

`TestIntegrationContainedParentSurvivesDaemonProcessLoss` runs production
`goobers up` in a separate process, submits through its normal HTTP trigger
route, and kills that process after the Portal shows a durable parent wait and
the generated child's shell has actually started. The replacement process uses
the same protected instance storage and pod key; its queue, authority registry,
read model and journal owners are reconstructed by normal startup. Journal fsync
is enabled for both daemon processes. Temporal, the dispatch Host and Kubernetes
remain alive. Only the external model and forge are disposable local fixtures.

This exposed three integration defects: loopback mode did not authenticate pod
principals; the child-tool grant advertised a daemon-local URL to a remote pod;
and generic startup resumed the generated child before original pod custody was
reconciled. The repairs keep local administration available while enforcing
signed machine scopes, keep child tools on the worker-configured daemon origin,
and leave generated-child stop/join and resume with the existing custody queue.
The queue preserves the accepted child identity and transfers its reconciled
capacity only when it takes exclusive journal ownership.

The complete process-kill journey passed with host race detection on 2026-10-10
(153.64 seconds), using image `haw-parent-daemon-proof-v2` at digest
`sha256:31facf60b590f37a5235d260b34755cd7724bdb44f12a92d20cd717dd80232a1`.
It verifies the same accepted child, stage occurrence and acceptance time; the
completed acknowledged result and both Portal links; and all three parent plus
two child physical invocations against their exact retained inputs. Every
invocation requires a distinct pod UID, stopped writers, surrender, disposal,
and a host-authored joined marker. Restart reconciles the interrupted physical
child attempt before resuming that same accepted child; it does not accept a
second child workflow.

This qualifies daemon loss at the durable parent-wait point. It does not claim
all crash cut points, loss during custody reconciliation, parallel parent
activation, recursive children or delegated PR publication. Those remaining
qualification boundaries still gate public activation. The loopback auth and
startup-owner fixes are extracted as independent main-based delivery slices;
this preparation branch is not a single merge unit.
