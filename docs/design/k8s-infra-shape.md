# Kubernetes Infrastructure Shape — what Goobers needs from a customer-managed cluster

**Status:** Approved for backlog planning (PO directive, 2026-07-16). Documentation-first:
this doc (plus reference manifests filed as backlog issues) is the deliverable.
**Provisioning code (Bicep/Terraform/cloud accounts) is explicitly out of scope** — the
customer brings a cluster they procure and manage. The existing `infra/` Bicep tree remains
quarantined per ARCHITECTURE.md §11 and is superseded for V2 planning by this shape
statement; DEP-001/002 are satisfied at V2 by *documented requirements*, not shipped IaC.

This is the companion to docs/design/v2-cloud-scale.md §9. It states requirements and
recommended defaults; it does not prescribe a vendor. Azure names from the requirements
docs (AKS, Key Vault, Entra, ADX) appear as the *reference substrate* — every row has a
vendor-neutral form.

---

## 1. Assumptions about the customer

- A supported-version Kubernetes cluster, customer-procured and customer-operated
  (upgrades, node OS, cluster RBAC, cost).
- Cluster-admin (or equivalently scoped) access for initial install of CRDs/operator.
- An OIDC issuer for humans (portal/API auth) and, ideally, workload-identity federation
  for pods (per-gaggle identities).
- A container registry the cluster can pull from (for Goobers images and team-baked
  workspace/sandbox images).
- Outbound egress to: the git/backlog provider (github.com or ADO), the model/agent
  endpoint the harness uses, and the team's own sandbox/provisioner targets. Everything
  else can be denied.

## 2. Component inventory (what runs in the cluster)

| Component | Kind | Namespace | Notes |
|---|---|---|---|
| Goobers operator | Deployment (1 replica, leader-elect) | `goobers-system` | Reconciles Goobers CRDs → runtime; owns domain logic (DEP-012) |
| Config sync | Argo CD app (or equivalent GitOps agent) | customer's GitOps ns | Git (config repo `main`) → CRs; Argo owns sync/drift/rollback, operator owns domain reconcile |
| Temporal server | Helm release (self-hosted, OSS) | `goobers-temporal` | DEP-011; basic visibility (no Elasticsearch) |
| PostgreSQL for Temporal | Customer-managed (recommended: managed cloud PG) | external or in-cluster | Temporal persistence; the one stateful service we require |
| Goobers workers | Deployment(s) per task-queue | `goobers-system` or per-gaggle | `goobers worker` (v2-cloud-scale A1.6); HPA-scalable |
| Agent stage pods | Ephemeral pods/Jobs | per-gaggle namespaces | Spawned per stage attempt (DEP-004..007); never long-lived |
| Daemon API + portal | Deployment + static assets | `goobers-system` | `/api/v1` read + authed mutation surface; SSE through ingress |
| Sandbox environments | Team-defined (Jobs/CRDs/external) | per-gaggle or team-chosen | BYO provisioner contract (v2-cloud-scale C4) |

## 3. Namespaces, identity, RBAC

- **`goobers-system`** — control plane (operator, workers, API). One service account per
  component, least-privilege Roles; the operator alone holds CRD-reconcile rights.
- **One namespace per gaggle** (GAG-012): stage pods, sandbox resources, and secrets for
  that gaggle live there. The operator provisions/labels these from the Gaggle CR.
- **Workload identity per gaggle** (SEC-001/002): each gaggle namespace's pods run as a
  distinct federated identity; the secret resolver scopes refs per gaggle — gaggle A's pods
  cannot resolve gaggle B's secrets even with cluster access equal.
- **Human access:** portal/API auth via customer OIDC issuer (Entra as a configured
  issuer, #38 ladder), roles view/operate/admin mapped in Goobers config — cluster RBAC is
  for operators of the cluster, Goobers RBAC is for users of Goobers; the two are not
  conflated.

## 4. State & storage

- **Journal & artifacts:** same on-disk layout as tiers 1–2 (`runs/`, `scheduler/`) on a
  cluster volume — requirement: a `ReadWriteMany`-capable StorageClass **or** blob-backed
  CSI mount; append-only usage, digested artifacts. Run ownership/single-writer is
  enforced by Temporal workflow identity at tier 3, **not** by file flocks — shared
  storage is a projection target, not a coordination mechanism.
- **Temporal persistence:** PostgreSQL (managed recommended); sizing guidance: modest —
  history is bounded per run and projected out; retention window configurable.
- **Telemetry:** OTLP export; reference substrate ADX, vendor-neutral form = any OTLP
  collector endpoint the customer runs. Partitioned/tagged per gaggle.
- **Workspace/object caches** (v2-cloud-scale B3/B5): node-local ephemeral volumes for
  worktrees; PVC snapshots or OCI images for baked workspaces; a per-repo object cache
  volume class. All rebuildable — never durable state.

## 5. Networking

- **Ingress:** one HTTPS ingress for API + portal (SSE requires proxy timeouts/heartbeat
  tuning documented); webhook/signal endpoint on the same surface (single inbound door).
  TLS via customer's cert machinery (cert-manager or equivalent).
- **Network policy defaults (deny-first):** stage pods get egress only to: git/backlog
  provider, model endpoint, their gaggle's sandbox targets, and the journal/artifact
  mounts. No pod-to-pod chatter across gaggles. Temporal reachable only from workers and
  operator. (Tier-3 resolution of SEC-Q5.)
- **No inbound to stage pods.** Ever. Results flow through the stage contract, not
  sockets.

## 6. Secrets

- Reference substrate: Key Vault via CSI/secret-resolver (SEC-010); vendor-neutral form:
  the `Resolve(ctx, name)` seam with a cloud resolver — customer chooses store. Token refs
  in config never change shape; only resolution does.
- Static PATs supported but discouraged at tier 3; short-lived minting seam
  (v2-cloud-scale D4) preferred where the provider supports it.
- Nothing secret in images, CRs, or the config repo (SEC-046 unchanged).

## 7. Sizing & node pools (starting guidance, revisit with B0 benchmarks)

- Control plane (operator+workers+API+Temporal): 2–4 vCPU / 8–16GB total, HA optional at
  first (Temporal replicas per Helm defaults).
- Stage pods: requests sized per workflow class; agentic stages are memory/IO-lean but
  long-running (minutes–hours) — bin-pack on a general pool; enable cluster autoscaler.
- Large-repo pools: nodes hosting object caches/baked snapshots want fast local NVMe;
  label + affinity (`goobers.dev/cache-node`) documented.
- Windows node pools: only for teams whose stages require Windows
  (cross-platform-support P13); default is Linux-only.

## 8. Deliverables filed from this doc

- **K1.** This shape doc merged (this PR) and kept current as V2 lands.
- **K2.** Reference manifests: kustomize base for `goobers-system` (operator, workers,
  API), a values file for the Temporal Helm chart, per-gaggle namespace template with
  network policies — *reference*, not managed IaC; customer applies/adapts.
- **K3.** `goobers doctor --k8s`: preflight that validates a target cluster against this
  doc (storage class capabilities, RBAC, registry pull, OIDC reachability, egress) and
  prints a conformance report. The install-time enforcement of this document.

## 9. Out of scope

- Bicep/Terraform, cloud accounts, cluster procurement/upgrades, multi-cluster/federation,
  service mesh, SaaS/multi-tenant hosting.
