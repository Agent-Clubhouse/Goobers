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
| `goobers-cloud-database` | Secret | keys `url` (PgBouncer) and `direct-url`; mounted as files for `control`, `secretKeyRef` for migrate. Never in argv. |
| `goobers-cloud-control-tls` | Secret (`kubernetes.io/tls`) | the `control` role requires TLS off loopback; workers and stage pods must trust the issuer. |
| `goobers-cloud-pod-key` | Secret | key `keyring.json`: `{"current":{"id":"k1","key":"<base64, >=32 bytes>"}}`. |
| `goobers-cloud`, `goobers-cloud-migration` | ConfigMaps | shipped in `prereqs/configmaps.yaml` (`instance-id`, `database-auth`, `backup-ref`); patch the values in your overlay. `instance-id` is shared by migrate and control. |

`database-auth` is `entra` in the base and applies to both migrate and control;
the migrate Job and the NetworkPolicy `allow-migrate-identity` assume it.

## Rollout order

The migrate Job must finish before `control` rolls.

**Argo CD.** Create two Applications from the same repo: `prereqs/` first (earlier
sync wave), then `workload/`, where `migrate-job.yaml` is a PreSync hook. The hook
needs the Namespace, ServiceAccounts, PriorityClasses and ConfigMaps to exist,
which is why they are not part of the same Application's sync.

**kubectl.**

```sh
kubectl apply -k deploy/reference/goobers-cloud/prereqs
# create the three Secrets above in the goobers-cloud namespace, then:
kubectl kustomize deploy/reference/goobers-cloud/workload > /tmp/cloud.yaml
kubectl apply -f /tmp/cloud.yaml -l app.kubernetes.io/name=goobers-cloud-migrate
kubectl -n goobers-cloud wait --for=condition=complete job/goobers-cloud-migrate --timeout=30m
kubectl apply -f /tmp/cloud.yaml
```

`kubectl apply -k deploy/reference/goobers-cloud` applies both halves at once
(used by the nightly kind job, where ordering is enforced by waiting on the Job).

## Shutdown

There is no preStop hook: SIGTERM makes the `control` role stop its loops and
release the lease, and the Recreate rollout has no traffic to drain. The 30s
grace covers the release and the HTTP shutdown.

## Why one replica

Readiness requires holding the lease, so `RollingUpdate` with `maxUnavailable: 0`
would deadlock. The hot standby and per-role namespaces arrive in P5.
