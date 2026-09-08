# Authenticated single-gaggle reference topology

This opt-in preparer writes a self-contained kustomization for one daemon and one
Temporal worker/dispatcher. It does not apply manifests, create credentials, or
contact a cluster. The unconfigured `../goobers-system` base remains unchanged.

The daemon exclusively owns the RWO journal PVC. The dispatcher has its own
writable instance `emptyDir`, separate workspaces, and a separate ServiceAccount
with pod create/get/list/delete permissions only in the chosen gaggle namespace.
Only the RWX blob PVC is shared. The daemon has no Kubernetes token or RBAC.
Image-host stage pods use their namespace's **default** ServiceAccount (the
actual dispatcher behavior); this overlay explicitly disables token automount on
that account and grants it no RBAC. There is no unused identity masquerading as
the identity a stage actually receives.

## Required inputs

Use an isolated namespace/instance when evaluating this reference. It is scoped
to Linux image-host runners in one manifest-listed gaggle. It rejects `self`
runners, deployment-host runners, floating images, and `workflowSource` sync.
An image runner must actually supply the capabilities it advertises. A base
image containing only Goobers is sufficient for deterministic Goobers stages;
agentic workflows need the appropriate harness image and credentials.

Before preparing:

1. Install the reference Temporal service in `goobers-temporal`, register the
   instance's Temporal namespace, and verify its ingress policy admits
   `goobers-system` workers and the daemon. Set `engine.hostPort` to
   `temporal-frontend.goobers-temporal:7233`; `engine.namespace` and `taskQueue`
   are used by both processes. Omitted queue/namespace use the product defaults.
2. Prepare a complete local instance (`instance.yaml`, `config/`, and optional
   sibling `goobers/`). Nested shared definitions/assets **within** `config/`
   are copied too. The product's definition walker recognizes `config/` and its
   sibling `goobers/`, not an arbitrary sibling `shared/` tree. Source files must
   be regular files with ASCII letters, digits, dot, underscore, slash, or hyphen
   in their paths. Symlinks and devices are refused. The bundle including its
   path index is limited to 700 KiB and 2048 files.
3. Run the matching RC binary's full instance-aware validation:
   `goobers validate /path/to/instance`. Resolve all placement, capability,
   credential, and workflow diagnostics. **The preparer checks config loading
   and topology contracts; it does not replace workflow admission.** Having an
   image runner declared does not establish that every workflow can use it.
4. Supply immutable `registry/repository@sha256:<64 lowercase hex>` image refs:
   one control-plane image and each `runners[].host`. The control-plane image
   needs `goobers`, `sh`, `mkdir`, and `cp`. No archive tools are needed. Verify
   that every stage image includes the expected Goobers release and tools.
5. Provision RWO block storage with reliable flock/SQLite semantics and RWX blob
   storage. Identify the corresponding StorageClass names. The reference claims
   retain their base sizes; adjust the prepared PVCs deliberately if needed.
6. Supply existing resources in `goobers-system`:
   - A TLS Secret containing `tls.crt` and `tls.key`. The server certificate must
     cover `goobers-api.goobers-system.svc` and be valid for server authentication.
   - A **different** Secret containing `pod-token.key`, at least 32 bytes of
     unpredictable shared signing material. Use a secret manager or secure local
     generation; do not use a sample key. Both processes mount this same Secret.
   - A ConfigMap containing `ca.crt`, a PEM bundle of public roots plus the API
     CA. Go clients in the control plane use it through `SSL_CERT_FILE`.
   - Optionally a Secret of provider/model credential files mounted in both
     processes at `/run/goobers/credentials`. Configure file-backed secret
     references accordingly. No `envFrom` imports arbitrary control variables.

The signing-key and TLS Secret files are mounted read-only with mode `0440` and
pod `fsGroup: 65532`; only the daemon receives its TLS private key. This preparer
never reads existing cluster Secret values. The worker's startup guard validates
its mounted signing key, and daemon startup validates listener configuration.

**Every stage image must separately trust the API CA.** Worker `SSL_CERT_FILE`
is not automatically propagated into a stage image. For a private CA, build an
adopted stage image that installs the public CA in its system trust store, then
pin that image by digest. Do not put the daemon's private key into an image.
Kubelet HTTPS probe success does not prove worker or stage TLS trust.

## Prepare and inspect

From the repository root, with the inputs above already chosen:

```sh
go run ./deploy/reference/authenticated \
  --instance /path/to/validated-instance \
  --out /path/to/new-prepared-overlay \
  --image "${GOOBERS_IMAGE_DIGEST}" \
  --stage-namespace gaggle-example \
  --journal-storage-class "${RWO_STORAGE_CLASS}" \
  --blob-storage-class "${RWX_STORAGE_CLASS}" \
  --tls-secret goobers-api-tls \
  --pod-token-secret goobers-pod-auth \
  --ca-configmap goobers-api-trust \
  --credentials-secret goobers-credentials \
  --apiserver-cidrs "${API_SERVICE_CIDR},${API_ENDPOINT_CIDR}" \
  --apiserver-ports 443,6443
kubectl kustomize /path/to/new-prepared-overlay > /path/to/reviewed-manifests.yaml
```

The output directory must not exist. Output files have mode `0600`: the generated
immutable **Secret** contains the adopted configuration, which may include
sensitive settings. Keep the output in protected deployment storage, not a public
repository. Referenced external TLS/credential files are not dereferenced. Keep private
keys outside the configuration trees that are deliberately bundled.
The source instance is not rewritten; the bundled `api.listen`, TLS paths, and
pod-token-key path are set to the topology's mounted paths.

Review the rendered resources and run your normal schema and cluster preflight
checks before applying them. Apply this generated kustomization as the
control-plane/gaggle deployment, **instead of applying the unconfigured base
alongside it**. Temporal remains a separate prerequisite. The generated topology
has no operator, external ingress, or CRD requirement. Human OIDC authentication,
if configured in the adopted instance, also needs explicit network reachability
for its issuer; the reference is directly usable with pod-only authentication.

NetworkPolicy is deny-first in both namespaces. The output includes:

- Worker egress to the exact API-server `/32` or `/128` destinations and TCP
  ports you supplied. Include the Service IP and actual endpoint addresses/ports
  required by your CNI's position relative to DNAT. No broad API-server subnet
  grant is accepted. Use `goobers doctor --k8s` and the API drift check to validate
  the actual endpoints; the preparer does not discover them.
- Worker-to-daemon egress and matching daemon ingress on HTTPS 8080.
- Per-runner-class stage egress from the production `netpolrender` implementation,
  plus daemon ingress from the chosen namespace **and** stage-role pod selector.
  No generic grant broadens the stage classes.
- Control-plane DNS, Temporal, and direct provider/model/sandbox destinations
  from the adopted `egress.allowlist`. There is no implicit public Internet or
  proxy grant. Complete that allowlist for the configured integrations.

## Configuration changes and restarts

The bundle consists of flat Secret entries and a safe, explicit copy program.
Executable assets use per-entry `0550` permissions; other config files use
`0440`. Executability is included in the bundle identity, so mode changes also
roll both deployments.
Both pod templates reference the same content-hashed immutable Secret. Each init
container populates a **fresh** private `emptyDir`; removed files cannot survive
from an earlier bundle. The daemon and worker mount `instance.yaml`, `config/`,
and `goobers/` read-only, while their own runtime state remains writable.
In-place DSL/config mutations are therefore refused by the filesystem. Update
the source of truth and prepare a new overlay instead. Do not enable a second
config sync writer or edit an immutable bundle.

Changing any bundled config content or file path changes the Secret name and
both pod templates. Applying the new overlay therefore rolls **both** deployments
with `Recreate`. This is a versioned deployment lifecycle, not continuous live
sync, and the documented `subPath` update hazard does not apply: each subPath
belongs to a fresh private volume populated from the new immutable bundle.

Pause admissions and let in-flight work drain before replacing a bundle or
signing key. Do not assume the two independent deployments switch at the same
instant. Worker digest pins fail closed when a run requests a config tree the
worker cannot serve; a restarted worker has no retained prior trees. Retain old
bundles/manifests for rollback until their runs have settled. Worker instance
state is ephemeral; the authoritative daemon journal and shared blobs persist.
Do not increase replicas or co-mount the daemon's instance into the dispatcher.

## Acceptance evidence and limits

Local tests check authority separation, matching shared stores/keys, readonly
configuration, fresh bundle replacement, real stage labels against both halves
of the network grant, and refusal of invalid preparation inputs. To check init
compatibility against an already available local release image:

```sh
GOOBERS_TOPOLOGY_TEST_IMAGE=your-existing-local-image TESTDEP_STRICT=1 \
  go test -tags=topology_image ./deploy/reference/authenticated \
  -run TestIntegrationPreparedTopologyInitRunsInReleaseBase -count=1
```

That check runs the actual generated init command with UID 65532, a read-only
container filesystem, dropped capabilities, and no network, then compares every
prepared file and its executable bit. The dedicated `topology_image` tag keeps
this optional image-specific proof and its Docker dependency out of the ordinary
strict integration inventory. It does not launch a daemon or stage.

Before calling an adopted installation functional, execute an authenticated
stage, retrieve its artifact through the blob plane, observe its surrender and
terminal run state, and repeat across worker and daemon restarts. Verify denied
cross-gaggle traffic and actual CNI policy enforcement. This preparer and its
static/local container checks do not establish the full S1–S9 support gate.
