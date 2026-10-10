# Child workflow publication

This guide describes the delegated publication backend for generated children.
Public child execution remains gated until the complete parent/child journey is
qualified. The backend builds on the
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

The Portal's accepted-child history shows locally verified publication observations:
confirmed branch names, links to confirmed PRs, and a needs-human notice when the
provider outcome is unconfirmed. Prepared intents are labelled as prepared, not
published. Missing or invalid custody is explicitly unavailable; expired child
records do not expose publication links. Reads use the existing parent-run access
boundary and perform no provider calls, credential acquisition, reconciliation or
writes. A needs-human notice does not itself authorize a recovery action.

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
