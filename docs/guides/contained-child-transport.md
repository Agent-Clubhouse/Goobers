# Contained child worker transport

## Availability

This is the worker transport foundation for generated child workflows. Public
child execution remains disabled. The daemon has no installed isolated child
factory, and the ordinary token minter does not satisfy the dispatcher's dedicated
child-token requirement. This guide describes internal preparation, not an enabled
Kubernetes, Temporal, Fleet or local host execution journey.

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

Before runtime enablement, connect and qualify exact signed attempt authority,
authenticated child-only artifact storage, retained agent kits, the isolated host
factory, returned-tree application, and rejoin/reconciliation after worker loss.
The internal contract digest and pod environment comparisons do not replace the
host's authentication checks.

Delegated branch/PR publication follows that host factory and requires the
parent's upfront permission. Parallel parent stages and human intervention use
their own subsequent lifecycle qualification. This slice introduces no storage
migration and grants no new provider or human permissions.
