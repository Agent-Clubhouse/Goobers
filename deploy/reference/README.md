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
| `goobers-system/` | kustomize base: operator, worker, daemon API + portal, RBAC, journal storage | §2, §3, §4, §5 |
| `gaggle-namespace/base/` | per-gaggle namespace template: namespace, identity-annotated ServiceAccount, deny-first NetworkPolicies | §3, §5 |
| `gaggle-namespace/examples/` | two example gaggle overlays (`gaggle-a`, `gaggle-b`) stamping the template | §3, §5 |
| `temporal/` | values for the OSS Temporal Helm chart + Temporal-isolation NetworkPolicy | §2, §4, §5 |

## Conventions

- **`CHANGE-ME`** marks every value the customer must replace (registry, hosts, CIDRs,
  storage class, identity client ids). Nothing here references a real registry or tenant;
  documentation CIDRs (`198.51.100.0/24`, `203.0.113.0/24`) stand in for real endpoints.
- **Image**: containers reference the image name `goobers`; the kustomize `images:`
  transformer in each kustomization rewrites it to your registry. Build the image with
  `make image` (packaging/docker/Dockerfile) and push it to a registry the cluster can
  pull from (§1) — Goobers does not publish images yet (CI publishing is a follow-up).
- **CRDs**: initial CRD install is a cluster-admin action (§1) from the operator release
  you deploy — regenerate from `api/v1alpha1` (`make manifests`) rather than trusting a
  stale checkout; the committed `config/crd/bases` are not CI-gated.
- **Stubs**: the worker `args` (`goobers worker`, v2-cloud-scale A1.6/#632) and the
  daemon API's in-cluster listener (#652) are stubbed with CHANGE-ME comments until those
  land — per #663 the manifests express the target shape now.

## Validation

No cluster is required:

```sh
kubectl kustomize deploy/reference/goobers-system
kubectl kustomize deploy/reference/gaggle-namespace/examples/gaggle-a
kubectl kustomize deploy/reference/gaggle-namespace/examples/gaggle-b

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
