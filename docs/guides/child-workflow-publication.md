# Child workflow publication

This guide describes delegated publication for generated children. The qualified
sequential journey uses a contained parent and child worker with publication on
the authenticated host. The backend builds on the
[contained transport](contained-child-transport.md) and
[parent disposition](child-workflow-admission.md#parent-result-disposition).

## Authority and execution

The parent must delegate publication before accepting the child. Current policy,
the retained proposal, exact stage and managed fork are verified again before
credentials are materialized. The current-policy lease spans the bounded effect.
Publication requires one of these canonical deterministic commands:

| Command | Required delegated capability |
| --- | --- |
| `goobers push-branch` | `repo:push` |
| `goobers open-pr` | `provider:pr:write` or `github:pr:write` |

Flags, scripts, environment overrides, alternate workspaces, base synchronization
and injected run context are refused for these commands. Other child stages
cannot consume publication capabilities. Agent and reviewer pods retain their
model-only credential ceiling. Publication runs synchronous, trusted Git/provider
operations on the host; authored commands continue through the contained worker.

GitHub and Azure DevOps use the configured gaggle identity and brokered stage
credential. ADO's authorization scheme comes from the delivered credential.
Expired credentials are refused, and the operation deadline cannot exceed the
credential expiry. A publication attempt is bounded to 30 seconds.

## Workspace and provider effects

The publication branch is derived from the gaggle namespace and accepted child
run. Its commit retains repository ancestry, includes child edits, and restores
excluded paths from the original base. Preparing the commit preserves the child's
HEAD, index and working files. Transport uses a private bare Git directory,
disables hooks and credential helpers, and creates the branch only if absent.

Each child has at most one immutable branch intent and one immutable PR intent.
The PR stage accepts typed `title`, `body` and `draft` inputs; text is scrubbed
before custody or provider use. Provider creation is a create-only operation.
Neither a concurrent PR nor a lost response authorizes an update to another PR.
Further edits after the retained publication snapshot require a separate child
workstream or the parent's subsequent work.

## Recovery and parent visibility

Publication progresses from `prepared` to `effect_pending` to `confirmed`.
Intent is durable before an external mutation. A retry reconciles the exact
branch and PR destination. An uncertain PR creation is never sent a second time.
A missing or changed observation remains uncertain.

The host records bounded publication custody in the child journal. The parent's
completion artifact includes verified publication status, branch/commit, PR link
when confirmed, and a `needsHuman` indication for uncertain effects. That status
is observational data and grants no new authority. Merge/replace/discard changes
the parent's workspace; discard does not retract a published branch or PR.

Cancellation and result acknowledgement preserve uncertain publication custody
and its reserved receipt capacity. The production lineage pruner keeps that
family until confirmation. Publication retry owns automatic reconciliation while
the original execution is authorized. Human reconciliation of a stopped or
cancelled child belongs to LAND-H06 in the
[incremental landing plan](../hitl-advanced-workflows-landing-plan.md); no new human
credential authority is inferred from the child's original delegation.

## Storage and qualification

Trigger store migration 12 follows the contained authority's migrations 10–11.
It stores the original emitting execution with each immutable intent, bounds
intent/receipt bytes, and reserves receipt space before effects. Older binaries
refuse the newer schema. Upgrade tests cover existing stores at versions 8–11.

Real Git tests cover ancestry, exclusions, unchanged workspace, lost push replies
and conflicting references. Native provider tests cover GitHub/ADO PR recovery
and create-only conflicts. Host integration tests exercise the actual child
factory and runner across publication stages with host-only credentials.
General human restart and recursive child execution remain separate workstreams.

### Complete contained-parent journey

`TestIntegrationContainedParentDelegatesChildPublicationThroughRealWorkers`
uses the disposable Kubernetes environment described in the transport guide.
The deterministic model substitute authors a child through the actual MCP tools.
A real child worker edits its private repository fork; canonical host stages then
publish the branch and PR using the configured delegated credentials. The native
GitHub adapter talks to a local HTTP fixture, and Git uses a local bare remote.
No real provider or model account is involved.

The confirmed case checks the published child edits, exact PR destination,
one create request, retained publication receipts, and publication status in the
parent's completion artifact. Discarding the child workspace leaves the published
branch intact. The lost-response case closes the HTTP connection after recording
creation and requires exactly one create request, an `effect_pending` PR intent,
and `needsHuman` in the parent's retained status. A lost response is never treated
as permission to create again.

These cases qualify the sequential execution and parent-return path. The Portal's
child history still needs its own publication-link and uncertainty projection;
human reconciliation remains LAND-H06. The existing native-provider tests cover
ADO request/reconciliation behavior, but this full worker journey uses GitHub.
