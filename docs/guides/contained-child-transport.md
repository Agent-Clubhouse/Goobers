# Contained child worker transport

## Availability

This is the worker transport foundation for generated child workflows. The daemon
connects an isolated child factory when its Temporal client, configured pod signing
key, surrender store and journal service are available. Narrow authenticated
child routes serve retained kits, credentials, journal observations and surrender.
Missing execution dependencies leave accepted children queued.

Opted-in parent stages can execute through configured contained workers when the
daemon has all required authority, queue, workspace and recovery services. The
sequential parent qualification below covers authoring, durable wait and return.
Missing services and standalone execution remain refused. Delegated publication
and human intervention have separate qualification requirements.

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
application and recovery of an uncertain worker. The parent qualification below
exercises those services together with the originating parent invocation, durable
wait and continuation. Additional execution shapes need equivalent qualification;
a simulated transport or isolated shutdown test does not establish that journey.
Contract digests and pod environment comparisons do not replace authentication.

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
parent's upfront permission. Parallel parent stages and human intervention have
their own lifecycle qualification. Worker transport grants no new provider or
human permissions.

## Recorded child activity in the Portal

Run details project the generated run's recorded parent and its current durable
child waits. Each wait identifies its stage, branch, child run and requested
handoff action. A whole-run waiting message appears only when the journal accounts
for all unfinished branches as waiting. A recorded continuation removes its wait.
Malformed or mismatched custody is shown as unavailable.

This read-only view uses the existing run-detail read surface and permissions.
Links identify recorded runs; queued execution and expired history can make a
linked run unavailable. The projection includes recorded waits and accepted-child
history. Human intervention controls require the remaining HITL work. A displayed
cancellation request provides no stopped-writer confirmation; the runtime
separately verifies physical custody before releasing a worker or workspace.

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

### Worker process loss after a durable pod receipt

The child dispatch activity now retains the API-observed namespace, name and UID
in its Temporal heartbeat, bound to the immutable dispatch input. Following a
heartbeat timeout, a versioned workflow path can reconcile that exact pod on the
existing dispatch queue. It verifies instance, owning workflow, run, stage,
physical attempt, contract and UID before stopping the pod, observing terminated
writers, confirming surrender and completing disposal. This path cannot create
a replacement; retry decisions remain with the parent.

`TestIntegrationChildRecoversAfterDispatchProcessLoss` kills an actual worker
Host subprocess after a running child has a durable receipt, then starts a fresh
Host. It checks one durable child start and the original pod's stopped-writer,
surrender and disposal evidence. Run with the explicit disposable kubeconfig,
Temporal CLI and locally built parent qualification image using the same
`GOOBERS_CHILD_QUALIFICATION_IMAGE` opt-in as the existing real-child tests.

Missing or changed receipts, replacement pods and missing stopped-writer or
surrender proof remain refusals. Loss before the first durable receipt, loss
during reconciliation and daemon reconstruction are not qualified by this test.
That test alone does not qualify the parent lifecycle; see the parent journey
tests below.

### Sequential agent-authored parents

The daemon coordinates opted-in parent stages through their pinned Linux image
runners. Configure the existing authenticated worker transport, an eligible
Claude Code or Codex API-authenticated Goober, a managed repository workspace,
and the stage's child-workflow policy. Standalone execution and missing contained
services remain refused. Ordinary stages retain their existing placement policy.

The parent can submit a child composed of allowed existing Goobers, then wait
without consuming its concurrency permit. The wait retains the accepted child,
source generation, stage context, human instructions and cumulative accounting.
After the child settles, the parent reacquires capacity and continues. The portal
can read the linked parent/child history. Repeated children use separate physical
attempts and retain their own outcomes.

The daemon records the parent workspace hold before dispatch. Returned committed,
staged and working state remain distinct. On terminal completion, verified
recovery inventory owns the archive before the checkout is released; interrupted
retirement retries under the journal lease. Resume restores the exact recorded
archive before another executor starts. Missing custody evidence, unfinished or
unacknowledged children, changed source policy or intervening checkout changes
prevent cleanup or replacement.

The production-path qualification replaces only the external model and forge
with deterministic fixtures. Build `cmd/goobers/testdata/qualification-claude.cjs`
into the local worker image as `/usr/local/bin/claude`, and set
`GOOBERS_PARENT_QUALIFICATION_IMAGE` to a unique
`localhost:45081/goobers:haw-parent-...` tag. With the explicit disposable kubeconfig
and Temporal CLI above, run the `TestIntegrationContainedParent` tests using
`go test -race -tags=integration ./cmd/goobers -count=1 -timeout=25m -v` and an
appropriate `-run` filter. The daemon-loss case kills a real fsync-enabled daemon
process and starts a fresh one, requiring the same accepted child and exact
physical stop/surrender/disposal evidence before completion.

Recursive children and generated Goober definitions are outside v1. Delegated
child PR publication
requires its own provider-write and ambiguous-result recovery qualification.
Loss before a durable worker receipt or during reconciliation is not proved by
the parked-parent daemon-loss test. A closed connection never proves work stopped.

### Codex parent adapter qualification

For the Codex variant, install the same deterministic fixture as
`/usr/local/bin/codex` in the qualification image. The fixture reads the private
MCP configuration produced by the real Codex adapter, requires the child tool
allowlist, launches the supplied MCP server with its explicit environment, and
emits Codex session/completion events. It verifies that only the Codex model
credential reaches that invocation. This exercises the Goobers adapter and
transport; it does not execute a live Codex model or qualify an external CLI
release.

Configure the parent Goober without a restrictive built-in `tools` declaration:
the Codex adapter refuses that unsupported configuration. Child admission,
capability ceilings, scoped MCP grants and contained placement remain enforced.
The host CLI probe permits only preflight, so accidental host inference fails.

With the same disposable cluster, image and Temporal settings, run:

```sh
go test -race -tags=integration ./cmd/goobers -count=1 -timeout=25m -v \
  -run '^(TestIntegrationCodexParentAuthorsChildThroughRealWorkers|TestIntegrationCodexParentCancellationStopsAuthoredChild|TestIntegrationContainedParentSurvivesDaemonProcessLossCodex)$'
```

These journeys cover parent-authored repository child creation and merge,
cancellation while the child is active, and fsync-enabled daemon process loss
while the parent waits. Recovery must retain the accepted child and lineage,
resume the parent, and prove exact physical worker stop, surrender and disposal.

### Parallel parents with one child per stage

Each parallel parent stage can author one active child using its own workspace
fork. Different stages may reuse an invocation key: stage ownership keeps their
children distinct. Branch results rejoin only after verified return; one branch
cannot observe or overwrite another branch's live workspace. Both instance and
workflow concurrency limits apply to child starts, so configure sufficient
capacity when concurrent children are intended.

Admission requires the complete fork, source, result and join services. Generated
children may also compose parallel stages as described below; recursion is refused.
Cancellation fences the parent family, stops each unfinished child and retains
unresolved branch workspaces until physical custody is verified.

The following tests use the same disposable environment and actual worker image:

- `TestIntegrationParallelParentsAuthorSeparateChildrenThroughRealWorkers`
  requires overlapping children, independent stage identities, preserved branch
  edits, verified joined results and Portal parent/child links.
- `TestIntegrationParallelParentCancellationStopsBothAuthoredChildren` cancels
  with both children active and requires exact stopped/surrender/disposal evidence.
- `TestIntegrationContainedParentSurvivesDaemonProcessLossParallel` kills an
  fsync-enabled daemon with both children active, then requires the same accepted
  identities and completed acknowledgements after ordinary startup recovery.

These tests passed together against one source/image. They do not qualify loss
before a durable worker receipt, interruption during reconciliation, delegated
publication or human intervention.

### Parallel stages inside a generated child

A parent may author a child workflow that uses the ordinary DSL's parallel
stages, existing tasks and supported workspace modes. This remains one child
owned by the originating parent stage. It does not enable recursive generation
or define new Goobers, capabilities, credentials or branch-write permissions.

The qualified journey runs two repository read-only child branches against the
parent's forked snapshot, then a writable root join returns the combined result.
The parent adopts that result using the ordinary workspace disposition contract.
Each physical branch worker retains its own journal identity and stop/surrender
evidence. After a daemon crash, recovery reconciles the original workers before
resuming the same child; it does not create a replacement child.

Startup cleanup retains detached stage views while their repository writers are
unacknowledged. Reconciled stage views can retire independently, while the shared
child fork remains protected until every repository writer is settled. A stage
finish or a closed connection alone cannot authorize cleanup.

Using the disposable environment above, these tests passed together with race
detection against the same source and worker image:

- `TestIntegrationParentAuthorsParallelChildThroughRealWorkers`
- `TestIntegrationCancelParentWithParallelGeneratedChild`
- `TestIntegrationContainedParentSurvivesDaemonProcessLossGeneratedParallel`

They cover overlapping child branches, parent cancellation while both branches
are active, and a real fsync-enabled daemon kill/restart. The recovery case checks
the original accepted child identity and each physical worker's stopped,
surrendered and disposed state. Human intervention remains a separate HITL slice;
these tests do not qualify interruption before a durable worker receipt or during
workspace disposition.
