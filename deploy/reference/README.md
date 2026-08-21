# Goobers Kubernetes reference manifests

Reference expression of [docs/design/k8s-infra-shape.md](../../docs/design/k8s-infra-shape.md)
(deliverable K2, issue #663) — the manifests a customer **applies and adapts** on a cluster
they procure and operate. This is *reference, not managed IaC*: Goobers does not apply,
sync, or reconcile these files at runtime, and provisioning code (Bicep/Terraform, cloud
accounts) is explicitly out of scope per the shape doc's status header. The quarantined
`infra/` tree is unrelated and stays quarantined.

Every manifest carries a comment citing the shape-doc section it implements, so drift
between the doc and these files is greppable (`grep -rn 'k8s-infra-shape' deploy/reference`).

## Layout

| Path | Contents | Shape doc |
|---|---|---|
| `goobers-system/` | kustomize base: operator, worker, daemon API + portal, RBAC, RWO instance storage, RWX artifact storage | §2, §3, §4, §5 |
| `gaggle-namespace/base/` | per-gaggle namespace template: namespace, identity-annotated ServiceAccount, deny-first NetworkPolicies | §3, §5 |
| `gaggle-namespace/examples/` | two example gaggle overlays (`gaggle-a`, `gaggle-b`) stamping the template | §3, §5 |
| `temporal/` | values for the OSS Temporal Helm chart + Temporal-isolation NetworkPolicy | §2, §4, §5 |

## Hand-managed node-pool contract

The reference deployment assumes that the cluster operator creates and manages its node
pools. It relies on pool properties, not particular VM sizes, node counts, purchase
models, or scaling limits:

| Pool | Required node properties | Workload placement |
|---|---|---|
| Linux (required) | Linux kubelet OS and the standard `kubernetes.io/os=linux` label | Normal Goobers workloads, including the operator, worker, API, and Temporal components, select `kubernetes.io/os: linux`. No Goobers-specific taint or toleration is required. |
| Windows (optional) | Windows kubelet OS, the standard `kubernetes.io/os=windows` label, and the operator-applied `kubernetes.io/os=windows:NoSchedule` taint | Windows stage workloads select `kubernetes.io/os: windows` and tolerate the matching `NoSchedule` taint. Do not add this toleration to Linux workloads. |

AKS supplies the OS label but does **not** automatically taint Windows pools. Apply the
taint when creating a Windows pool, and preserve it when replacing or adding nodes:

```sh
kubectl taint node <windows-node> kubernetes.io/os=windows:NoSchedule
```

Karpenter and AKS Node Auto Provisioning (NAP) are deliberately deferred. The current
worker Deployment does not create one unschedulable pod per stage, and Goobers has no
pod-level scaling signal for an autoprovisioner to observe. Revisit autoprovisioning
after a pod-level scaler or pod-per-stage execution model exists. Until then, operators
choose and manage capacity for both pools according to their own workloads; this
reference sets no fixed capacity, VM SKU, spot policy, or scale-to-zero default.

## Conventions

- **`CHANGE-ME`** marks every value the customer must replace (registry, hosts, CIDRs,
  storage class, identity client ids). Nothing here references a real registry or tenant;
  documentation CIDRs (`198.51.100.0/24`, `203.0.113.0/24`) stand in for real endpoints.
- **Image**: containers reference the image name `goobers`; the kustomize `images:`
  transformer in each kustomization rewrites it to your registry. Build the image with
  `make image` (packaging/docker/Dockerfile) and push it to a registry the cluster can
  pull from (§1) — Goobers does not publish images yet (CI publishing is a follow-up).
  The image includes Node.js, GitHub CLI, and the Copilot CLI default agent harness, so
  the reference worker can run deterministic and agentic stages.
- **Mixed-OS safety**: the Linux control-plane workloads are pinned with
  `kubernetes.io/os: linux`. In a cluster with Windows nodes, also taint every Windows
  node so an unpinned Linux workload cannot attach and initialize a Linux volume there:

  ```sh
  kubectl taint node <win-node> kubernetes.io/os=windows:NoSchedule
  ```

  `NoSchedule` does not evict existing pods, so this is safe on a live cluster. Windows
  workloads must select `kubernetes.io/os: windows` and carry the matching toleration:

  ```yaml
  tolerations:
    - key: kubernetes.io/os
      operator: Equal
      value: windows
      effect: NoSchedule
  ```

  If you choose an in-cluster CloudNativePG database instead of the recommended managed
  PostgreSQL service, pin its Linux pods through the `Cluster` scheduling field (not a pod
  template):

  ```yaml
  spec:
    affinity:
      nodeSelector:
        kubernetes.io/os: linux
  ```
- **CRDs**: initial CRD install is a cluster-admin action (§1) from the operator release
  you deploy. The committed `config/crd/bases` are generated from `api/v1alpha1`; update
  them with `make manifests`. The merge gate regenerates the CRDs and rejects any diff.
- **Stubs**: the worker `args` (`goobers worker`, v2-cloud-scale A1.6/#632) are stubbed
  with CHANGE-ME comments until they land. The daemon API Deployment is explicitly
  disabled (`replicas: 0`) until its in-cluster listener (#652) lands; enabling the
  current lock-owning daemon would also contend with the worker for the RWO instance
  volume. Per #663 the manifests express the target shape now.

## Validation

No cluster is required. The merge gate runs `make deploy-validate`, which renders all
three kustomizations and passes them through strict kubeconform schema validation.
The Go test suite also checks every Deployment's container arguments against the
registered CLI flags and requires execution-critical worker flags such as `--instance`.
Run the same render and schema gate locally with:

```sh
make deploy-validate

# Temporal values render (pinned chart version — see temporal/values.yaml header):
helm repo add temporal https://go.temporal.io/helm-charts
helm template temporal temporal/temporal --version 0.62.0 \
  --namespace goobers-temporal -f deploy/reference/temporal/values.yaml >/dev/null
```

`goobers doctor --k8s` (deliverable K3, issue #668) is the companion preflight: it
verifies a target cluster against the same shape-doc requirements these manifests express.

## Stamping a new gaggle

Copy one of `gaggle-namespace/examples/*`, set `namespace:` to the gaggle's namespace
name and the `goobers.dev/gaggle` label pair, then replace the CHANGE-ME egress CIDRs
and workload-identity annotation for that gaggle (§3: one namespace and one federated
identity per gaggle; GAG-012, SEC-001/002).

## Operating notes from a real cluster

Everything below was hit while standing up these shapes on AKS end to end
(Goobernetes spike, epic #2889): a Linux daemon, a Windows worker, Temporal with
CloudNativePG, and a workflow that merged a pull request across both operating
systems. They are recorded because each one cost real time to diagnose and none
of them are guessable from the manifests.

### Mixed-OS clusters: pin every workload, taint the Windows nodes

**AKS does not taint Windows nodes.** `kubectl get node <win> -o jsonpath='{.spec.taints}'`
returns empty, so nothing stops a Linux pod from being scheduled onto one.

That is not a scheduling inconvenience, it is **data loss**. A Linux pod with no
`nodeSelector` landed on a Windows node, Windows initialized the attached managed
disk, and an MBR partition table was written over the ext4 superblock — the volume
was not corrupted, it was replaced (#2883). It survives a pod restart perfectly;
it does not survive a scheduling accident.

Two defences, and you want both:

```sh
kubectl taint node <windows-node> kubernetes.io/os=windows:NoSchedule
```

`NoSchedule` does not evict running pods, so it is safe to apply to a live
cluster. Give Windows workloads the matching toleration, and pin every Linux
workload with `nodeSelector: {kubernetes.io/os: linux}` — including anything a
CRD schedules for you. CloudNativePG takes it under `spec.affinity.nodeSelector`,
not the pod template, which is easy to miss.

The failure is also **misreported**. The visible error is
`401 Unauthorized` from the registry, because the Windows containerd snapshotter
fails to unpack a Linux image and the client falls back to an anonymous token
request. Ignore the 401 and look for `io.containerd.snapshotter.v1.windows` in the
message.

### Storage: keep the instance root on RWO and artifacts on RWX

`goobers doctor --k8s` warns when it finds an RWX-capable class because
provisioner-name inference cannot prove cross-client coordination safety. On
Azure Files, EFS, Filestore, CephFS and NFS, POSIX `flock` does not exclude
across clients and SQLite WAL is documented-unsafe — and the instance root
carries both file locks and SQLite databases (#2854). Put the **instance root
and journal** on a ReadWriteOnce block volume mounted by a single node.

The **artifact store** is the opposite case: put it on a ReadWriteMany volume.
It is safe on exactly those network storage classes because a content-addressed
store never locks: a digest is written once, published by rename, and two
writers racing on one digest are writing identical bytes.

### Secrets

The Secrets Store CSI driver mounts secrets `0644` by design. Goobers accepts
those mode bits only when `internal/platform/secfile` can prove that the file is
on a read-only tmpfs, as Kubernetes uses for pod-private CSI and Secret-volume
mounts. Everywhere else, token files remain owner-only and the check fails
closed if the mount protection cannot be established. For providers that do not
expose a read-only tmpfs, use an initContainer to copy and chmod the secret into
a memory-backed `emptyDir`.

On Windows the same control is expressed through ACLs rather than mode bits, and
a copied file inherits a grant to `S-1-5-11` (Authenticated Users). Break
inheritance: `icacls <file> /inheritance:r /grant:r '<principal>:F'`.

### `subPath` ConfigMap mounts never receive updates

A `subPath` volumeMount looks identical to a whole-file or whole-directory mount
in the manifest — same `configMap` ref, same `mountPath` — but its update
behavior is not. Kubernetes materializes a `subPath` target as a **copy** made
once at pod creation, not the symlinked, periodically-resynced view a
whole-ConfigMap mount gets. Editing the ConfigMap afterwards changes nothing in
the running container until the pod restarts, and nothing at write time warns
about it: the manifest is indistinguishable from a hot-reloadable one (#3365).

This bit a live instance running a Squid egress proxy: the allowlist ConfigMap
was mounted with `subPath` for a single-file target, an allowlist fix (adding an
entry the proxy needed) landed in the ConfigMap, and Squid kept enforcing the
stale rule until the pod was restarted — dropping in-flight agentic HTTP. The
same trap applies to `goobers up --watch-config` or any other config-driven
sidecar: the daemon's own reloader polls its config directory for a changed
digest every second (`configReloadInterval`, `cmd/goobers/configreload.go`), but
a second's cadence buys nothing if the bytes underneath a `subPath` mount never
change — the loop ticks forever and finds nothing to reload.

Avoid it by mounting the **whole ConfigMap (or a projected volume combining
several) at its own directory, without `subPath`**, and pointing the app at that
directory instead of a single carved-out file:

```yaml
volumeMounts:
  - name: allowlist
    mountPath: /etc/squid/allow.d   # whole directory, no subPath
volumes:
  - name: allowlist
    configMap:
      name: egress-allowlist
```

The kubelet resyncs a whole-ConfigMap mount by atomically swapping a symlink, so
`squid -k reconfigure` (or any watcher polling the directory) sees the new
content without a restart. When the application truly needs one fixed file path
and can't take a directory, an initContainer that copies the ConfigMap entry into
an `emptyDir` at startup also works — it adds a moving part, but keeps
whole-mount update semantics on the source you actually edit.

Either way, verify by execution, not by reading the manifest: edit the
ConfigMap, wait past the resync/reload interval, and confirm the running process
(or the mounted file's content — `kubectl exec … -- cat <path>`) actually
changed. The manifest that broke reload here read exactly like a working one.

### Images

The default image carries the Copilot CLI agent harness. Its home is mounted from
an `emptyDir` in the reference worker so `$HOME/.copilot` remains writable while
the container root filesystem stays read-only. To use another harness, derive an
image that installs it and set the matching `runner.harnessCommand` in
`instance.yaml`; the map is keyed by harness name.

Budget for the Windows image: roughly **2.4 GB**, and about **4m30s** for a cold
pull on a fresh node. If your operator-selected capacity policy scales the Windows
pool to zero, expect that pull on the first run after it scales up.

### Container init: who reaps orphaned stage descendants

The image is `ENTRYPOINT ["goobers"]` in exec form with no init wrapper, so the
daemon is **pid 1** of its container. That makes it the kernel's reparent target
for every stage descendant that outlives its parent — a double-fork, or a
descendant whose parent `KillTree` reaches first — and a Go program waits for
nothing but its own `exec.Cmd` children. Nobody else is above it to reap.

The daemon supplies that missing init half itself: at startup it checks
`os.Getpid() == 1` on Linux and, only then, runs a SIGCHLD-driven loop that
`wait4`s orphaned descendants (#3398). It logs
`startup: running as container init (pid 1)` when it does. The loop deliberately
waits specific pids rather than `wait4(-1)`, so it never consumes a stage's exit
status out from under the runner. Nothing is needed in the pod spec for the
reference deployments.

Two arrangements put the daemon somewhere other than pid 1 and switch the loop
off; give those a reaping init instead:

- Wrapping the entrypoint in a shell (`sh -c "goobers up …"`) — the shell
  becomes pid 1 and most shells do not reap. Prefer exec form, or `exec goobers`.
- Sidecar or debug containers sharing a pid namespace
  (`shareProcessNamespace: true`), where the pause container is pid 1.

The general escape hatch is any minimal init as pid 1 — `tini -g`, the
`--init` flag for plain `docker run`, or `shareProcessNamespace: true` so the
pause container reaps. Symptom when nobody does: `Z`-state processes
accumulating in the pod and worktrees never released.

### Timezones

Windows containers ship no IANA database. `goobers` embeds Go's copy, so a
location name works — but any other Windows tooling in your image will not have
it.

### Multi-worker runs need the artifact store

A stage consumes prior work through ContextPointers, which resolve as a path on
the **local** filesystem. One worker: correct and free. Two workers: stage 2 is
polled by a node whose staging area is empty and fails closed on an integrity
fault. `--blob-store` on a ReadWriteMany volume is what makes a run spannable;
omit it (and the volume) if you run exactly one worker.

Code state is a separate channel and is **not** shared: every stage attempt gets
a fresh worktree on the run branch, so what survives between stages is the branch
in that worker's own git mirror. A stage that hands work to another platform must
push first (#2861).

The `goobers-system` namespace enforces Pod Security Standards `restricted`.
PSS evaluates **initContainers** too, so an adopter's per-worker instance-root
seed must set the same non-root, no-escalation, dropped-capabilities, read-only
root filesystem, and `RuntimeDefault` seccomp controls as the application
container. The reference worker includes that restricted-compatible seed.
These controls are Linux-only: Kubernetes rejects them on Windows pods. Keep
Linux and Windows workers as separate Deployments, set `spec.os.name: windows`
for Windows workers, and do not copy the Linux security context into them.

### Cluster rebuilds

`az aks get-credentials --overwrite-existing` does not reliably repoint `kubectl`
after a cluster is deleted and recreated under the same name — it can keep
resolving the old control-plane FQDN and fail with `no such host`. Re-run it
explicitly as a step rather than trusting the flag.
