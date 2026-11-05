# Cloud worker sample

Apply this optional overlay after the cloud control base, Temporal Worker
Controller, and the gaggle namespaces. It uses the production cloud config,
blob, and control endpoints; workers receive no database or storage credentials.
The controller creates two workers per build and a separate PDB for each build.
Use the controller values in `../../workers/controller-values.yaml` (relative
to this directory) to permit WorkerResourceTemplate PDBs.

Supply `goobers-cloud-pod-key` with `keyring.json` and the public trust bundle
ConfigMap `goobers-cloud-control-ca` with `ca.crt`. The trust bundle must trust
the control Service certificate. Stage images must trust the same issuer;
worker trust does not automatically install a CA in stage pods. Never mount
control's private TLS key into workers or stages.

Patch the image and `unsafeCustomBuildID` to a distinct immutable release or
commit for every release; release binaries require the build ID to contain
their embedded version. Keep WorkerDeployment name `goobers`, which is the
production worker routing contract. Keep this worker's task queue and Connection aligned
with `engine` in the applied instance configuration. The existing cloud base
supplies control/Temporal/API-server egress; this overlay adds frontend
ingress from cloud control and worker pods in `goobers-temporal`; narrow the API-server CIDR
for your cluster. The worker service account uses its Kubernetes token to
create and adopt stage pods.

For **every** gaggle namespace, compose `../../gaggle-namespace/base`, retarget
its dispatcher RoleBinding subject to `goobers-worker` in `goobers-cloud`, and
supply the per-runner egress policy. The worker's boot preflight refuses missing
namespace/RBAC grants. Stage pods retain the unprivileged `goobers-stage`
service account with no API token.

`test/ha/kind/child` consumes this overlay with a test-only model image. Its
`pending #8085` gate requires both the production parent handoff adapter and
instance child-workflow enablement before removal. The sample itself does not
enable children or install a model substitute.
