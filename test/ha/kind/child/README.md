# Engine child qualification on kind

This is scenario 10 of #8085. `CHILD-HAPPY` runs a scheduled parent, accepts
one generated deterministic child through the production MCP/HTTP routes, and
requires the resumed parent to consume its sealed continuation receipt.
`CHAOS-13-CHILD` holds the child after a real side effect, reads its exact pod
UID and owner from a Temporal custody heartbeat, deletes that worker with a UID
precondition, and releases the same stage only after another worker adopts it.
The final checks require one admission, the original child execution and stage
pod, the same attempt/charge, and terminal child and parent results.

The driver reads PG lineage/journal rows through CNPG's local socket and actual
Temporal execution history. It does not mint authority, manufacture a child
handoff, or replace worker/dispatcher activities. Only the `claude` model
executable is replaced, in a derived **test-only** stage image. The model uses
the real registered `goobers-io` MCP server; stage egress permits only DNS and
the cloud control service. Side effects and barriers live in the observed
stage pod, not in worker memory.

The nightly composes `deploy/reference/goobers-cloud/workers`, the cloud base,
and the reference gaggle namespace. It registers a separate `kind-child`
Temporal namespace to avoid changing the earlier worker scenarios' version
routing. The instance omits `engine.childWorkflows`, exercising the default-on
posture shipped by v0.6.0.
Workers and stages trust the generated public control CA; they never receive
the private TLS key or database/object-store credentials.

The nightly runs this qualification after cloud rollout. Inspect the uploaded
`CHILD-HAPPY.json`, `CHAOS-13-CHILD.json`, and `child-chaos-summary.json`
artifacts before claiming qualification.

## Static validation

From the repository root:

```sh
go test ./test/ha/kind/child ./test/ha/childscenarios
node --check test/ha/kind/child/fixture/model.cjs
kubectl kustomize deploy/reference/goobers-cloud/workers
kubectl kustomize test/ha/kind/child
actionlint .github/workflows/ha-nightly.yml
```

`TestMountedConfig` checks the mounted definitions and generated proposal with
production loaders/validators. Its temporary, in-memory opt-in checks schema
compatibility only; the deployment gate is never bypassed. The workflow fills
`BUILD_SHA` with the checked-out commit and `BUILD_ID` with the binary version
plus that commit. The sample reference keeps its release placeholder and
contains no model substitute or child enablement.
