# Temporal rollout and contract inspection

The experimental Temporal operator surface can collect routing evidence and
inspect a workflow's exact source against a binary before a rollout. These are
read-only inspection commands. They do not change traffic, admit runs, publish
worker metadata, or enable worker versioning.

## Inspect native deployment state

```sh
goobers temporal rollout-status --timeout 30s ./instance
```

The command uses the instance's engine endpoint, namespace and TLS settings. It
reads the `goobers` Worker Deployment, describes each registered version, and
checks the deployment's conflict token again before emitting JSON. A routing
change, failed version read, inconsistent inventory, or inventory above 128
versions causes a nonzero exit with no partial snapshot. The timeout bounds the
complete dial/read sequence; its maximum is five minutes.

`phase` describes routing only: `stable` means no ramp, `ramping` means a nonzero
ramp, and `paused` means a versioned candidate still referenced at zero percent.
The unversioned target is `__unversioned__`. A paused candidate is not drained.
The output keeps each native drainage check's timestamp. Missing drainage is
`unknown`; an old `drained` observation is not a fresh retirement decision.

Use `goobers engine-queues` and `goobers status` for the existing queue/poller
diagnostics. These are observations rather than positive execution readiness
probes. Routing rollback changes new traffic; histories already pinned to a build
still need that build and its resources. Accepted or uncertain start receipts can
also retain a build even before a history is visible. Queries to closed histories
may need it after native drainage reports `drained`.

## Inspect the exact binary and source

```sh
goobers temporal worker-contract > worker-contract.json
goobers temporal worker-contract --workflow-ir workflow.json
goobers temporal worker-contract --workflow-ir preview-workflow.json --allow-preview
```

Run this command from the executable or container image being evaluated. Its
manifest includes the on-disk executable file's SHA-256, embedded build ID, current IR/compiler
contracts, and DSL/feature support derived from the existing interpreter
registries. The manifest digest is a content identity, not a signature or proof
that a remote worker runs those bytes. Replacing the executable file while this
process is running can separate those bytes from the running image. A development or reused build ID remains
unsafe to treat as an immutable deployment identity. No registration or remote
attestation is performed.

The optional workflow input is canonical Workflow IR JSON, bounded at 1 MiB.
Unknown JSON fields and extra documents are refused. Inspection requires the
persisted exact `sourceDefinition`; a claimed digest without that source is
`unverifiable`. The inspector verifies the source digest, normalizes the source,
and compares the complete canonical IR before invoking the runtime's existing
child/fan-out guards and DSL compiler. This preserves the DSL as runtime
authority. Cosmetic IR `featureGates` do not establish runtime requirements;
requirements come from `workflow.FeaturesForWorkflow`.

`compatible` means the exact source passes these local runtime checks with the
selected preview posture. It does not establish remote-worker support, placement,
goober/provider credentials, stage images, config retention, or readiness. The
output always states `enforced: false`. Exit status is 0 for a manifest or a
compatible inspection, 1 for incompatible/unverifiable source or inspection
failure, and 2 for invalid flags/input. Preview inspection does not change the
instance's preview policy.

## Delivery boundary and next slices

This is partial delivery for #4084 and #4085. It reuses the recovered #4084
`Describe`/`DescribeVersion` and conflict-token patterns from commit
`8b7a6dc7bb9fc8419bfba0c11a5ab731b52f9472`, adapting status to shared
`internal/workerrouting`. Previous-current metadata is not labeled last-known-good.
There are no new persisted records to prune and no runtime input/replay changes.

Manual ramp/promote/rollback controls still require operation provenance,
verified candidate evidence, unknown-outcome reconciliation and an admission
barrier. Exact admission must serialize accepted input generation with that
barrier, pin the chosen build before digest/custody, and cover scheduler starts,
direct starts and the separate child-dispatch contract. A route read followed by
an unpinned start is not enforcement. Rollback must retain committed receipts;
Temporal not-found does not free an already-attempted start for reissue.

Real local-server ramp/rollback/pinning tests and retained-resource migration
qualification remain required before those later slices can be enabled. Removing
these inspection commands has no effect on existing routing or run histories.
