# Selected-revision workspaces

> Status: approved — local milestone delivered; distributed and writable-sandbox work remains.
> Owner: @brandiv
> Area: runtime / repository workspaces
> Tracking: #4157, #5121, #5122, #5123, #5124, #5125, #5126, #5127
> Delivered-by: #5121, #5122, #5123
> Pending-delivery: #5124, #5125, #5126, #5127
> Scope-delta: #5121 delivery covers the typed contract and local journal replay. Its Temporal state/replay obligation remains pending with #5124. Pod parity, isolated remote writable branches, and lifecycle/conformance/reference workflows are not yet delivered.

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
| `workspaceBranch` | Existing writable-workspace ownership flow | Writable branch continuity; it never grants ownership of a selected source branch. |
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

Shared non-retryable failures preserve these codes through runner boundaries:
`workspace_revision_invalid`, `workspace_revision_unauthorized`,
`workspace_revision_conflict`, `workspace_revision_acquisition`,
`workspace_revision_object_type`, and `workspace_revision_sha_mismatch`.

## Pending distributed and writable milestones

### Temporal and workerhost — #5124

The existing Temporal substrate does not by itself implement selected-revision
state. The pending integration must keep the binding in deterministic workflow
state, reconstruct the identical value from recorded history, carry it through
activity/workspace requests, and retain legacy behavior when absent. Replay must
not reselect a PR or resolve a source branch. Workerhost acquisition must match
local exact-SHA verification, materialization policy, and failure classification.
This explicitly carries the still-pending Temporal portion of #5121.

### Pod dispatch and checkout — #5125

Dispatch/attempt contracts must propagate the immutable selection. Pod checkout
must acquire and verify its exact object, preserve declared partial/sparse policy,
and refuse repository/branch fallback, delta publication, and undeclared checkout
expansion.

### Isolated remote writable branch — #5126

Writable iteration targets only the configured base repository, never the source
branch. The planned path creates a remote workflow-owned branch at the selected
commit with create-only/expected-SHA semantics. Fork objects require sterile exact
object transfer. Journal repository, owned ref, and starting SHA before emitting
`workspaceBranch`; later publication may update only that owned branch.

### Lifecycle and conformance — #5127

Complete expected-SHA cleanup, contention outcomes, crash/restart reconciliation,
authoring diagnostics, cross-substrate conformance, and reference inspection/
isolated-iteration workflows. Only after these obligations and the distributed
milestones land may the full #4157 design be marked implemented. Direct source
branch mutation, automatic merging, and a revision PR into the original source
branch remain outside this design.

## Validation and documentation maintenance

Focused local contract validation:

```sh
go test ./api/v1alpha1 ./api/schemas ./internal/workspacerevision
go test ./api/validate -run '^TestSchemaBackedEnvelopeCompleteness$'
go vet ./api/v1alpha1 ./api/schemas ./internal/workspacerevision
```

The local milestone additionally exercises provider identity/selection, runner
trust and resume, journal conformance, ordinary/pinned same-repository and fork
checkout, materialization policy, and exact-object refusal paths. Subsequent
milestones must extend these tests rather than treating local coverage as evidence
of distributed parity.

Keep this ledger, Architecture §5.1, the stage contract, and schemas synchronized.
Regenerate the design index with `go run ./test/designstatus -write` after ledger
changes and validate it with `go run ./test/designstatus`.
