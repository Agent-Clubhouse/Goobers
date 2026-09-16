# Selected-revision workspaces

> Status: approved — local, Temporal/workerhost, pod inspection, and owned remote branches delivered; lifecycle milestone remains.
> Owner: @brandiv
> Area: runtime / repository workspaces
> Tracking: #4157, #5121, #5122, #5123, #5124, #5125, #5126, #5127
> Delivered-by: #5121, #5122, #5123, #5124, #5125, #5126
> Pending-delivery: #5127
> Scope-delta: Typed state, exact-SHA inspection, and remote owned-branch establishment/publication are delivered across local, workerhost, and pod execution. Final lifecycle/conformance/reference workflows remain a separate milestone.

## Purpose and authority

A deterministic stage may select a repository and exact commit for later stages
to inspect without checking out a moving branch or changing the run's configured
writable target. The contract is the optional typed
`ResultEnvelope.workspaceRevision`, not an arbitrary output scalar or artifact.
See [Architecture §5.1](../ARCHITECTURE.md#51-repository-state-has-four-distinct-authorities)
and the [stage contract](../stage-contract.md).

Four states remain distinct:

| State | Owner | Meaning |
|---|---|---|
| Configured base and additional repositories | Configuration | Authorized repository access and materialization policy; the base remains the writable target. |
| Selected revision | Successful deterministic producer, then immutable run history | Exact repository/commit inspection input with optional provenance. |
| `workspaceBranch` | Existing writable-workspace flow, constrained by typed ownership after explicit establishment | Writable branch continuity; immutable `workspaceBranchBinding` records the configured target, generated ref, and starting SHA. Neither grants ownership of a selected source branch. |
| Workspace delta artifacts | Existing writable-workspace flow | Mutable work-product continuity, not repository-routing authority. |

The identity is losslessly convertible to `providers.RepositoryRef`: provider,
owner, ADO project, name, native ID, and URL. Branches, credential selectors,
checkout policy, and arbitrary extension fields are forbidden by the closed
identity schema. The selected commit and optional base SHA must each be a full
lowercase SHA-1 or SHA-256 object ID: exactly 40 or 64 hexadecimal characters.
Source-ref display and source/PR ID are provenance, never refs to resolve.

## Immutable transition and local persistence

`internal/workspacerevision.Accept` is the shared transition:

1. An absent control is a legacy-compatible no-op.
2. An agentic producer cannot emit workspace-routing authority, even when it
   repeats a previously accepted value.
3. An unsuccessful deterministic result establishes nothing.
4. The first successful, valid deterministic value establishes an independent
   deep copy, including optional base identity.
5. Identical re-emission, including provenance, is idempotent.
6. Any different value fails with non-retryable `workspace_revision_conflict`.

The local runner records the accepted binding in the normative typed
`workspaceRevision` field of `stage.finished`. Event replay reconstructs it
without provider polling or moving-ref resolution. `run.yaml` is never modified
to add a later selection. Old result and journal fixtures omit the optional field
and retain their previous configured-base behavior.

The result, repository-identity, workspace-revision, and journal-event schemas
are the cross-language contract. JSON Schema/type round trips, closed-field
rejection, envelope completeness, local resume, and journal conformance protect
the representation and backward compatibility.

## Configuration authorization

`internal/workspacerevision.Resolve` accepts the selected revision plus the
configured base and additional repositories. It returns only a matching
configured reference, including its existing checkout policy. It does not mint
credentials or derive grants from the producer's result.

Identity comparison includes provider, service scheme/host, repository owner/name,
ADO project, and consistent repository URL path. Self-hosted Gitea requires its
service URL; same owner/name on another host or in another ADO project is not an
authorized substitute. Ambiguous configured references with different authority
or policy are rejected rather than selecting one by iteration order.

Configuration `RepoRef` has no separate native-ID field. Consequently a claimed
native ID is preserved as evidence but must not override the returned configured
name for provider routing. When ADO configuration explicitly names the repository
by ID, that configured value remains the authority. Proving that a claimed ID
belongs to a configured repository name otherwise requires a trusted provider
lookup; a stage-authored ID or URL is not such proof.

## Delivered local exact-SHA inspection

Ordinary and pinned local `repo-readonly` execution acquire the authorized exact
object and verify commit type and final `HEAD`. A missing or different object
fails closed; a matching branch name is never a fallback. The selected-revision
marker keeps this path separate from ordinary delta preservation and cleanup.

Selected inspection does not synchronize/rebase the configured base, restore
workspace deltas, publish changes, or acquire source-branch write authority.
Declared partial-clone and sparse-checkout policy remains effective. Undeclared
submodules, LFS expansion, and custom checkout filters remain inert.

Pinned inspection is serialized by the whole-run lease and resets/cleans before
and after each selected `repo-readonly` stage. It intentionally does not preserve
repository modifications or ordinary ignored/untracked build state between those
stages. Configured additional read-only worktrees can coexist with the pinned
primary and have independent teardown; coexistence creates no new access grant.

Shared failures preserve these codes through runner boundaries:
`workspace_revision_invalid`, `workspace_revision_unauthorized`,
`workspace_revision_conflict`, `workspace_revision_acquisition`,
`workspace_revision_object_type`, and `workspace_revision_sha_mismatch`.
All except acquisition are non-retryable. Acquisition may use the existing
bounded transport retry policy against the same authorized repository and exact
SHA, including when an object is unavailable. Retrying never permits a different
repository, branch, object ID, checkout policy, or credential.

## Delivered Temporal and workerhost inspection — #5121 / #5124

`RunInput.WorkspaceRevision` and accepted deterministic activity results form
immutable workflow state. Acceptance precedes `stage.finished` projection;
failed, agentic, malformed, unauthorized, and conflicting controls cannot enter
that accepted record. The next invocation carries the same optional
`WorkspaceRevision`, while retaining the configured base `RepoRef` and
`BaseBranch` for provider operations.

Activity arity is unchanged: the optional structured invocation control carries
the revision to `WorkspaceRequest.WorkspaceRevision`. The independent
`Checkout` request field preserves ordinary materialization policy despite
`RepoRef.EnvelopeRef()` excluding checkout configuration. Workerhost authorizes
selected acquisition against worker configuration, returns that configured
source reference, and uses its sparse cones plus the existing credentialed
manager's original partial-clone policy. It provisions detached
`BaseRef = ExpectedSHA = commitSha`, with shared object-type/HEAD verification and
stable classified failures.

Read-only revision requests carry neither `WorkspaceBranch` nor `WorkspaceDelta`,
never sync base, and publish no delta. Explicit contradictory requests fail
closed. Scratch selectors do not acquire a selected checkout, and existing
writable continuity remains independent unless explicit sandbox establishment runs.

Checked-in Temporal histories predating the field replay with the new workflow.
The real-server E2 replay fixture set includes selection and identical
re-emission. Workflow tests exercise an infrastructure retry with identical
selection and normative accepted journal events. Workerhost integration tests
exercise same-repository/fork, full/partial, sparse/full worktrees on independent
workers after the source branch moves, plus exact-object refusal parity.
Neither retries nor replay reselect a PR or resolve a moving source ref.

Selected read-only pod consumers require live journaling. Accepted controls are
acknowledged by the journal writer before downstream pod dispatch; projection-only
engine runs fail closed because source checkout credentials authorize against the
durable accepted `stage.finished`, not an invocation's asserted selection.

See [distributed state and workspace continuity](distributed-state-and-coordination.md#72-selected-revisions-and-workspace-continuity)
for the request transport and authority boundaries.

## Delivered pod dispatch and checkout — #5125

Dispatch/attempt contracts propagate the immutable selection separately from the
configured source and pinned checkout policy. Privileged pod environment values
carry these controls only to provisioning, not into the stage's environment.
Checkout fetches the exact source object into a fresh, template-free repository,
verifies commit type and detached HEAD, and preserves full/partial/sparse policy.
Submodules, LFS expansion, custom filters, hooks, and filesystem monitors remain
inert. Selected read-only pods neither consume nor publish workspace deltas.

The authenticated credential plane verifies the pinned read-only stage and the
run's durably accepted revision before resolving the source's repository-qualified
read grant. It never substitutes a generic stage grant, daemon identity, or base
write credential. A configured credential that cannot resolve fails closed;
anonymous access is reserved for explicitly configured public sources without a
credential. See [pod dispatch](goobernetes-dispatcher.md) and
[credential guidance](../guides/github-token-scopes.md#selected-revision-checkout-credentials).

## Delivered isolated remote writable branch — #5126

The deterministic kinds `workspace-branch-establish` and
`workspace-branch-publish` execute in the trusted host backend, including when
ordinary stages use pod placement. Establishment requires a prior selection,
scratch workspace, declared `repo:push`, and separately resolved configured
source-read/base-write grants. Neither operation permits `syncBase`, dynamic
kind substitution, or parallel-branch execution.

`internal/workspacebranch.Expected` generates only
`refs/heads/<namespace><workflow>/<run-id>` in the configured base.
`internal/worktree.EstablishRemoteBranch` fetches the exact authorized SHA into
sterile temporary Git state, verifies commit type and exact identity, and pushes
that object with expected-old-zero protection. This also covers source-only fork
objects. Matching existing refs are verified idempotent success; different tips
are stable non-retryable conflicts. No source-ref lookup or fallback occurs.

The typed `workspaceBranchBinding` control is closed and immutable. Local and
Temporal acceptance journal target identity, full ref, and starting SHA before
downstream scalar `workspaceBranch` adoption. Full-history resume verifies the
pinned producer and source authorization; live Temporal journal acknowledgment
precedes pod dispatch. A crash between remote creation and journaling retries the
same operation and verifies the matching target object even if the source is no
longer available. The current branch tip is not the immutable starting SHA.

Ordinary/pinned local workspaces and workerhost/pod workspace deltas retain their
existing continuity mechanisms. Owned writable acquisition checks attached branch
and ancestry and preserves configured base sparse/partial policy. Read-only
selected stages still discard all changes. Merely selecting a revision does not
opt legacy writable workflows into ownership or credential restrictions.

`PublishRemoteBranch` imports the exact local commit without trusting stage
remotes, push refspecs, hooks, or Git configuration for authority. It verifies
descent from the immutable starting SHA and current owned remote tip, then
compare-and-swaps only the owned ref. The backend's repository write credentials
never enter authoring/agent stages. Owned pod checkout gets only server-verified
base read access under `workspace-branch:checkout`; generic requests cannot
recover write credentials by impersonating another stage in the run.

Evidence: real bare-remote create/rediscovery/conflict and exact fork tests,
source-ref preservation and missing-grant refusals; ordinary/pinned local
continuity and resume; live Temporal ownership-before-consumption and journal
projection; worker-to-pod-to-fresh-worker delta/broker integration with full,
partial, and sparse policy; HTTP checkout response validation and actual harness
credential non-injection. See the [stage primitives](../reference/workflow-primitives/stage-commands.md#owned-remote-branch-operations)
and [credential guidance](../guides/github-token-scopes.md#owned-selected-revision-branch-credentials).

## Terminal lifecycle and recovery

The immutable `workspaceBranchBinding.startingSha` proves the root of ownership;
the separate typed `workspaceBranchTip` proves the exact last acknowledged
publication. The trusted publisher returns the object it actually transferred,
not a moving ref read after publication. Local and Temporal `stage.finished`
records carry this evidence; result-file promotion reserves it from scalar
outputs and acceptance rejects arbitrary producers.

The shared terminal preparer covers completion, failure, abort, and restart
recovery. It reconstructs the full pinned history and reauthorizes the configured
base repository. Cleanup uses sterile Git, the base's repository-qualified write
grant, a bounded timeout, and
`--force-with-lease=<owned-ref>:<durable-expected-tip>`. It never deletes the source
ref, derives authority from source text, or retries using a freshly observed tip.
If the generated target equals known same-repository source-ref provenance,
establishment refuses the collision even when native-ID representations differ.

| Typed cleanup outcome | Meaning |
|---|---|
| `workspace_branch_deleted` | The owned expected tip was deleted, including a reconciled lost response. |
| `workspace_branch_already_absent` | Repeated cleanup has nothing left to delete. |
| `workspace_branch_tip_changed` | Another writer advanced/replaced the ref; preserve it. |
| `workspace_branch_ownership_missing` | No durable binding or no known publication lease; preserve it. |
| `workspace_branch_ownership_invalid` | Pinned producer, identity, or history validation failed; preserve and report the error. |
| `workspace_branch_cleanup_failed` | Authorization, transport, or deletion failed; report the error without inventing success. |

Before publication the lease is the immutable starting SHA. A crash after remote
creation but before journaling retries create-only establishment and verifies
the identical ref. A crash after publication but before recording its tip leaves
the old lease: cleanup preserves the advanced branch. A successful old publisher
record without a typed tip makes the lease unknown. A crash after deletion but
before recording its outcome converges to already absent. Audit failures are
returned, not hidden; cleanup may already have succeeded and can safely repeat.

Local cleanup annotations go to the live run journal before `run.finished`.
Engine terminal hooks annotate the instance log because the engine's completed
run journal must still equal its normative projection. Legacy namespace-based
reconciliation refuses sandbox runs using their pinned definition or typed
ownership, including a crash before ownership acknowledgment; it cannot
manufacture a cleanup lease from the current remote tip.

## Authoring and materialization boundaries

Validation rejects branch/delta/`syncBase` inputs for selected read-only work,
typed authority supplied through scalar inputs, invalid trusted backend kinds,
and sandbox writable work not dominated by establishment on every reachable
path. Sandbox authority and writable work remain serial; distributed static
fan-out/fan-in is still outside this feature. Runtime authorization checks
dynamically selected sources against configuration; a source unknown until a
selector runs cannot be proven authorized solely by static validation.

`repo-readonly` never carries inspection changes to another stage, including
legacy pinned execution without a selected revision. It detaches at the prior
committed baseline and resets/cleans inspection commits, ignored files, and
untracked products before and after exposure, without advancing the prior
writable branch. Ordinary writable `repo` behavior and declared clean policy
remain unchanged. Use committed writable continuity when build/test/review
stages need to observe earlier edits.

Full, partial (`blob:none`), and sparse policy come from configuration on local
disposable, local pinned, workerhost, and pod paths. Partial plus sparse tests
verify excluded blobs are not materialized, rather than merely checking flags.
Gitlinks remain unexpanded, LFS files remain pointers, and undeclared
clean/smudge/process filters, submodule recursion, hooks, and fsmonitor remain
inert. A selected commit cannot introduce credentials or authorize another
repository through these mechanisms.

Read-only is a repository-continuity contract, not a read-only filesystem or an
OS sandbox. In particular, native local execution does not contain a malicious
same-user process from other host files or credentials. Existing isolation
requirements still apply. Providers may refuse exact-SHA acquisition for objects
their Git endpoint does not serve; this is a classified acquisition failure, not
permission to fetch a branch or substitute an identity.

## Cross-substrate conformance matrix

The matrix names executable evidence, not an exhaustive Cartesian product.
Shared Git/authority tests cover common failure paths; substrate tests verify
transport and classification at their actual boundaries.

| Dimension | Local disposable | Local pinned | Workerhost / Temporal | Pod |
|---|---|---|---|---|
| Same repository / fork | `TestSelectedRevisionLocalRunAndResume`, `TestSelectedRevisionForkWorkspacesCoexist` | Same runner tests in pinned mode; `TestIntegrationPinnedRevisionDiscardAndCustody` | `TestIntegrationWorkerSelectedRevisionMaterializationAndRetry` | `TestIntegrationPodWorkspaceRevisionMaterialization` |
| Full / partial / sparse | `TestIntegrationExactRevisionMaterialization` | `TestIntegrationPinnedRevisionDiscardAndCustody` (full and partial+sparse) | Materialization-and-retry test across policy combinations | Pod materialization test across policy combinations |
| Missing grant / object / non-commit | `TestIntegrationExactRevisionRejectsUnavailableAndNonCommit`, `TestIntegrationExactRevisionPreservesAuthorizationRefusal` | Shared exact-object acquisition and pinned custody tests | `TestIntegrationWorkerSelectedRevisionFailureParity`, `TestSelectedRevisionRefusesInvalidRequestsBeforeGit` | `TestIntegrationPodWorkspaceRevisionRefusals`, `TestPodWorkspaceRevisionNoDeltaOrCredentialFallback` |
| SHA mismatch / branch substitution | `TestIntegrationExactRevisionVerificationRefusals`, `TestExactRevisionOptionsFailClosed`, exact HEAD checks after moving the source branch | Same exact verification; detached reset/custody checks | Shared verifier plus failure-classification and moving-branch retry tests | Exact HEAD checks, parsing refusals, `TestPodWorkspaceRevisionFailureRetryability` |
| Retry / resume / replay | Local run/resume and immutable-control tests | Local run/resume in pinned mode | `TestWorkspaceRevisionWorkflowRetryAndContinuity`, `TestWorkspaceRevisionRecordedLegacyHistories` | `TestDispatchOneTransportsSelectedRevisionAndLegacyPayloads`, worker-to-pod continuity test |
| Independent inspections | `TestSelectedRevisionParallelBranchesReceiveIndependentTrees`, `TestIntegrationExactRevisionParallelCoexistence` | Serialized by whole-run lease, not parallel checkouts | Independent worker materialization; distributed static parallels not implemented | Independent pod checkouts; distributed static parallels not implemented |
| Writable commit continuity | `TestOwnedBranchLocalContinuityAndResume` | Same test in pinned mode | `TestOwnedBranchWorkflowDurabilityAndContinuity` | `TestIntegrationOwnedBranchWorkerPodDeltaAndBroker` (worker -> pod -> fresh worker) |
| Create / conflict / source preservation | `TestIntegrationRemoteBranchEstablishmentAndPublication`, `TestIntegrationRemoteBranchRefusals` | Same trusted backend, independent of checkout custody | Same trusted backend | Establish/publish execute in host backend, not in authoring pod |
| Cleanup lease loss / crash / ambiguity | `TestIntegrationRemoteBranchCleanupLeaseAndRecovery`, `TestIntegrationOwnedWorkspaceTerminalCleanup` | Same terminal coordinator and durable evidence | Same terminal coordinator; instance-log audit preserves run-journal conformance | Same coordinator after pod result acknowledgment; no pod-owned deletion |

`TestOwnedBranchSourceCollisionAndTipAuthority`,
`TestOwnedBranchResultFilePromotion`, `TestDispatchExecOwnedBranchTipPromotion`,
schema completeness, and journal conformance cover the final typed publication
field. `TestWorkspaceAuthorityDiagnostics` covers control-flow bypass and
parallel misuse. The terminal integration test exercises the actual trusted
backend, pinned journal, legacy sweep exclusion, and shared terminal callback.
`TestLegacyPinnedReadonlyPreservesOnlyWritableCommits` exercises real runner
callbacks across writable -> read-only -> read-only -> writable stages, while
`TestRemoteBranchCleanupRefusesMissingAuthority` covers missing grants and
malformed cleanup leases without contacting a remote.

The opt-in [reference workflows](../../reference-workflows/README.md#selected-revision-examples)
demonstrate stateless inspection and two committed isolated iterations followed
by explicit publication and terminal cleanup. Source-tree validation loads both.
Direct source mutation, revision-PR creation, automatic merge, and automatic
publication without an explicit broker stage are not implemented.

## Validation and documentation maintenance

Focused local contract validation:

```sh
go test ./api/v1alpha1 ./api/schemas ./internal/workspacerevision
go test ./api/validate -run '^TestSchemaBackedEnvelopeCompleteness$'
go vet ./api/v1alpha1 ./api/schemas ./internal/workspacerevision
```

The conformance matrix above distinguishes real Git execution, workflow tests,
and shared coordinator coverage. No credentialed GitHub/ADO/Gitea deployment is
implied by fixture validation. Tests use local remotes, provider fixtures, actual
workerhost/pod provisioning functions, and recorded Temporal histories.

Keep this ledger, Architecture §5.1, the stage contract, and schemas synchronized.
Regenerate the design index with `go run ./test/designstatus -write` after ledger
changes and validate it with `go run ./test/designstatus`.
