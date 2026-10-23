# goobers-cloud reference base (P3)

Kustomize base for the cloud install in `docs/design/control-plane-ha.md`
([§14](../../../docs/design/control-plane-ha.md#k8s-shape)). `../goobers-system` stays
the mode-2 base; the two are independent.

| Piece | File |
|---|---|
| `control` Deployment: 1 replica, `Recreate`, grace 30s, lease-gated readiness, 30s node tolerations, preStop `goobers-cloud sleep 5`, no PVC | `control-deployment.yaml` |
| PDB (`maxUnavailable: 1`, never `minAvailable: 1` on one replica) | `control-pdb.yaml` |
| Migrate Job (Argo PreSync hook, `backoffLimit: 0`, 30 min deadline) | `migrate-job.yaml` |
| Default-deny NetworkPolicies and allows | `networkpolicies.yaml` |
| Namespace with Pod Security `restricted` enforced | `namespace.yaml` |
| PriorityClasses `goobers-control-plane` / `goobers-stage` | `priorityclasses.yaml` |
| Rainbow worker hooks (ServiceAccount, contract) | `components/worker-base` |
| Stage priority guard for gaggle namespaces | `components/stage-priority-guard` |

Workers are one Deployment per build ID, produced by the rainbow overlay, not by
this base.

## Secrets

Database credentials come from the `goobers-cloud-database` Secret (keys `url`,
`direct-url`), mounted as files for `control` and read via `secretKeyRef` by the
migrate Job. They never appear in argv. Create the Secret out of band (or use the
Entra federated identity with `--database-auth=entra`).

## Rollout order

The migrate Job must finish before `control` rolls. Argo CD runs it as a PreSync
hook. With plain kubectl:

```sh
kubectl kustomize deploy/reference/goobers-cloud > /tmp/cloud.yaml
kubectl apply -f /tmp/cloud.yaml -l app.kubernetes.io/name=goobers-cloud-migrate
kubectl -n goobers-cloud wait --for=condition=complete job/goobers-cloud-migrate --timeout=30m
kubectl apply -f /tmp/cloud.yaml
```

(Apply the Namespace, ServiceAccounts, and PriorityClasses first on a fresh cluster.)

## Why one replica

Readiness requires holding the lease, so `RollingUpdate` with `maxUnavailable: 0`
would deadlock. The hot standby and per-role namespaces arrive in P5.
