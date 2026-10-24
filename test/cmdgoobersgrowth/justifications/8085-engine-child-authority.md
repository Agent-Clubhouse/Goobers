# Engine parents' child-workflow authority (#8085 b)

The `goobers-cloud` control role now binds engine parents' child-workflow
grants, and its worker delivers them to stage pods
(docs/design/engine-parent-child-authority.md). All behaviour lives below the
command layer:
- `internal/cloud/childengine.ParentAuthority`, `Authorities` and
  `FencedAuthority` handle the authority checks, the binding and the release;
- `internal/enginechildplane.Authority` and the `httpapi` routes provide the
  plane;
- `internal/childadmissionclient` is the worker client;
- `internal/engine` does the stage-dispatch acquire and release;
- `internal/runtimewiring.DeliveredChildWorkflowAccess` is the in-pod
  provider.

The command layer keeps only composition:
- `constructEngineChildAuthority` is the controller-side seam registered after
  `EngineChildAdmissions`. It reuses the admissions' fenced custody and fails
  closed without them, the Temporal client or the credential-grant key.
- The control role dials Temporal before assembling its planes, rather than
  after, and hands the client to the seams through `databaseServices`.
- The worker's stage dispatch constructs `childadmissionclient.AuthorityForWorker`
  with its pod endpoint and shared secret registry, and threads it into the
  engine deps.
- `cmd/goobers` changes by one wrapped expression, so the pod's harness prefers
  the delivered grant.

None of this can live below `cmd/goobers-cloud`. Only the roles' composition
roots hold the Temporal client, the per-role databases, the lease, the signing
keys and the worker's secret registry together. It adds no command and no second
authority path.
