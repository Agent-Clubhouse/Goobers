# goobers-cloud reference base (P3)

Kustomize base for the cloud install in `docs/design/control-plane-ha.md`
([§14](../../../docs/design/control-plane-ha.md#k8s-shape)). `../goobers-system` stays
the mode-2 base; the two are independent.

| Piece | File |
|---|---|
| `control` Deployment: 1 replica, `Recreate`, grace 30s, lease-gated readiness over TLS, 30s node tolerations, no PVC | `workload/control-deployment.yaml` |
| PDB (`maxUnavailable: 1`, never `minAvailable: 1` on one replica) | `workload/control-pdb.yaml` |
| Migrate Job (Argo PreSync hook, `backoffLimit: 0`, 30 min deadline) | `workload/migrate-job.yaml` |
| Default-deny NetworkPolicies and allows (migrate→identity, worker→API server, ...) | `prereqs/networkpolicies.yaml` |
| Namespace with Pod Security `restricted` enforced | `prereqs/namespace.yaml` |
| PriorityClasses `goobers-control-plane` / `goobers-stage` | `prereqs/priorityclasses.yaml` |
| Rainbow worker hooks (ServiceAccount, contract) | `components/worker-base` |
| Stage priority guard for gaggle namespaces | `components/stage-priority-guard` |

Workers are one Deployment per build ID, produced by the rainbow overlay, not by
this base.

## Inputs you supply

| Input | Kind | Notes |
|---|---|---|
| `goobers-cloud-db-owner` | Secret | key `direct-url` as role `goobers_owner` (DDL); used only by the migrate Job via `secretKeyRef`. |
| `goobers-cloud-db-api`, `goobers-cloud-db-controller` | Secrets | keys `url` (PgBouncer) and `direct-url` as roles `goobers_api` / `goobers_controller`; both mounted as files into `control`, which holds two pools. Never in argv. |
| `goobers-cloud-control-tls` | Secret (`kubernetes.io/tls`) | the `control` role requires TLS off loopback; workers and stage pods must trust the issuer. |
| `goobers-cloud-pod-key` | Secret | key `keyring.json`: `{"current":{"id":"k1","key":"<base64, >=32 bytes>"}}`. |
| `goobers-cloud`, `goobers-cloud-migration` | ConfigMaps | shipped in `prereqs/configmaps.yaml` (`instance-id`, `database-auth`, object-store URL/container, `backup-ref`); patch the CHANGE-ME values. Set `instance-id` once (it also names `goobers_<instance-id>`). |
| `goobers-cloud-instance` | ConfigMap | generated from `prereqs/config/instance.yaml` and mounted read-only at `/etc/goobers/config`; replace that reference instance with the configuration generation source your controller applies. |

`database-auth` is `entra` in the base and applies to both migrate and control;
the migrate Job and the NetworkPolicy `allow-migrate-identity` assume it.
The control role likewise uses Entra for object storage by default. Emulator or
private shared-key overlays opt in with `--object-store-account-name` and
`--object-store-account-key-file`; the key stays in a mounted Secret file.

## Provisioning

Roles and the database exist before migrate runs (the owner role needs its
database, and grant reconciliation needs the API/controller roles). Production
uses `goobers-cloud bootstrap` (`pgroles.Provision` as the server admin). The
nightly kind job does not call it yet (it refuses until #7813, #7818 and #7763
ship). Its kind CNPG cluster already declares the production role contract
(`goobers_owner`/`goobers_api`/`goobers_controller`/`temporal_app`, random
passwords from Secrets the runner creates, never logged), so the one thing the
nightly still does by hand is create `goobers_<instance-id>` with its CONNECT
grants as the superuser. That step is the single flip point to `bootstrap`.

## Rollout order

The migrate Job must finish before `control` rolls.

A migration that cannot take its table locks (behind a long-running reader, or
another migration) does not queue: it is retried with backoff for up to 20
minutes inside the Job. If the database is still busy, the Job exits **75**
(EX_TEMPFAIL) and the sync fails having applied nothing in the busy step. That
is contention, not a broken migration: re-sync once the contention ends. Any
other failure exits 1.

**Argo CD.** Create two Applications from the same repo: `prereqs/` first (earlier
sync wave), then `workload/`, where `migrate-job.yaml` is a PreSync hook. The hook
needs the Namespace, ServiceAccounts, PriorityClasses and ConfigMaps to exist,
which is why they are not part of the same Application's sync.

**kubectl.**

```sh
kubectl apply -k deploy/reference/goobers-cloud/prereqs
# provision the database and roles (goobers-cloud bootstrap, or your own
# provisioning), create the Secrets above in the goobers-cloud namespace, then:
kubectl kustomize deploy/reference/goobers-cloud/workload > /tmp/cloud.yaml
kubectl apply -f /tmp/cloud.yaml -l app.kubernetes.io/name=goobers-cloud-migrate
kubectl -n goobers-cloud wait --for=condition=complete job/goobers-cloud-migrate --timeout=30m
kubectl apply -f /tmp/cloud.yaml
```

`kubectl apply -k deploy/reference/goobers-cloud` applies both halves at once
(used by the nightly kind job, where ordering is enforced by waiting on the Job).

## Stage blob plane

Workers and their stage pods read and write content-addressed blobs (stage artifacts, spans, an engine child's sealed result) over the blob plane. `control` serves that plane directly over its object store (#8188).

**Worker contract.** Every per-build worker Deployment (the rainbow overlay; see `components/worker-base`) passes the `control` Service as its blob endpoint:

```yaml
args:
  - --blob-endpoint
  - https://goobers-control.goobers-cloud.svc:8080
```

- The dispatcher hands the same URL to stage pods as `GOOBERS_BLOB_ENDPOINT`.
- The base NetworkPolicies already admit worker and stage pods to `control` on 8080 (`allow-worker-control`, `allow-control-ingress`).
- Workers and stage pods must trust the issuer of `goobers-cloud-control-tls`.
- Authentication is the pod and worker-blob bearers minted from `goobers-cloud-pod-key`. No other credential reaches the plane.
- At boot, a worker checks the wiring: it reads control's per-instance canary through this endpoint, and withholds engine child workflows until it can.

**When it is live.**
- `control` serves its API, the blob plane included, only once its plane assembly is complete. Check `goobers-cloud control --check-assembly`. Until then, keep workers on the mode-2 endpoint (below).
- The blob routes are served whether or not the replica holds the controller lease. The `goobers-control` Service only routes to the Ready (lease-holding) pod, which is the single replica in this base. Multi-replica serving arrives with P5.

**The mode-2 `goobers-blobs` PVC stays.** It backs the `goobers` API's blob plane in `../goobers-system` (modes 1 and 2) and is unchanged. The cloud install never mounts it.

**Migrating a mode-2 install.** Blobs written to `goobers-blobs` before the switch are not in the object store:
- Either copy them across before switching. The layout is identical, `blobs/sha256/<aa>/<rest>`, so it is a straight tree copy into the container.
- Or drain in-flight runs on the old endpoint first, and switch workers only for new runs.

A stage whose context points at a blob only the PVC holds fails closed when it resolves that context. It never silently skips the blob.

## Shutdown

There is no preStop hook: SIGTERM makes the `control` role stop its loops and
release the lease, and the Recreate rollout has no traffic to drain. The 30s
grace covers the release and the HTTP shutdown.

## Why one replica

Readiness requires holding the lease, so `RollingUpdate` with `maxUnavailable: 0`
would deadlock. The hot standby and per-role namespaces arrive in P5.
