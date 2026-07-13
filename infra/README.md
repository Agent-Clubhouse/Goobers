# Goobers — `infra` reference

> **Tier-3 (V2) — quarantined, not on the V0 path.** See
> [`docs/ARCHITECTURE.md` §11](../docs/ARCHITECTURE.md#11-repo-impact-map). Revived in V2.

Reference layout + templates for a Goobers instance's **`infra` repo** — the rarely-changing
half of the two singleton repos per instance (the other being `config`, the
workforce-as-code). Per-instance teams fork this as their own `infra` repo.

> This lives in the upstream monorepo as a **reference example**. A real instance's `infra`
> repo is a standalone singleton (INST-007), permissioned independently from `config` so the
> Tutor can be granted write to `config` only (DEP-010, SEC-021, CFG-010).

## Two-stage GitOps deploy (DEP-002, DEP-003, DEP-012)

```
 release pipeline                         ArgoCD (in-cluster)
 ────────────────                         ───────────────────
 1. az deployment sub create  ──►  AKS, Log Analytics, Storage,
    (Bicep, infra/bicep)             Key Vault, ADX, PostgreSQL
 2. install ArgoCD + apply    ──►  root app-of-apps
    infra/argocd/bootstrap                │
                                          ├─ cluster-baseline (ns + netpol)
                                          ├─ temporal (Helm, external PG)
                                          ├─ goobers-operator (CRD reconcile)
                                          └─ goobers-config  ──► the SEPARATE
                                                                 `config` repo
```

Stage 1 (`infra`) runs rarely. Stage 2 (ArgoCD watching `config`) reconciles continuously
(DEP-003): change a scale factor + merge → more replicas; add a goober definition → a new
team member appears.

## Bootstrap order

The dependency order is **infra → Temporal → ArgoCD → operator**:

1. **Infra** (Bicep): AKS, Log Analytics, Storage, Key Vault, ADX, **PostgreSQL** — the
   datastore Temporal needs must exist first.
2. **Temporal**: self-hosted in-cluster against that PostgreSQL (schema setup jobs run).
3. **ArgoCD**: installed, then handed the app-of-apps root.
4. **Operator**: reconciles Goobers CRDs from the `config` repo.

GitOps inverts steps 2–4 *operationally*: ArgoCD is installed first and then **deploys**
Temporal and the operator as child Applications (`argocd/applications/`), so Argo owns
their sync/drift/rollback (DEP-012). So the install sequence the pipeline executes is
`Bicep → ArgoCD → {Temporal, operator, config}`, which satisfies the same dependency
order — PostgreSQL (from Bicep) is up before Temporal's Application syncs. The
`infra-bootstrap.yml` stages encode exactly this.

### How Temporal gets wired (no secrets or per-instance values in git)

The Temporal DB connection has two instance-specific pieces that are deliberately **not**
committed; the `BootstrapGitOps` stage supplies them from the provision outputs:

- **Host** — the Bicep `postgresFqdn` output is exported as a stage variable and injected
  as an Argo **Helm parameter** (`server.config.persistence.{default,visibility}.sql.host`)
  via `argocd app set`. `temporal/values.yaml` leaves `host` unset; the Temporal
  `Application` is **manual-sync** so it never reconciles before the host is set.
- **Password** — pulled from the bootstrap Key Vault and written to the
  `temporal-postgres-credentials` k8s secret the chart consumes via `existingSecret`
  (SEC-010). The pipeline creates the namespace + secret before syncing Temporal.

The cluster name / resource group used for `az aks get-credentials` come from the same
Bicep outputs (not hard-coded), so any `namePrefix`/`environment` works.

## Layout

| Path | Purpose | Spec |
|---|---|---|
| `bicep/main.bicep` | Subscription-scope bootstrap; creates RG + wires modules | DEP-001, INST-003 |
| `bicep/modules/aks.bicep` | AKS + OIDC + workload identity + CSI + Container Insights | DEP-001, SEC-002 |
| `bicep/modules/adx.bicep` | ADX goober-run telemetry store | TEL-001, TEL-010 |
| `bicep/modules/postgres.bicep` | PostgreSQL flexible server for Temporal | DEP-011 |
| `bicep/modules/keyvault.bicep` | Key Vault (secrets referenced, never stored) | SEC-010 |
| `bicep/modules/storage.bicep` | Storage account / disk | DEP-001 |
| `bicep/modules/loganalytics.bicep` | Logging workspace | DEP-001 |
| `bicep/main.bicepparam` | Example params; secret via `getSecret` | SEC-010 |
| `cluster/` | Baseline namespace + default-deny egress policies | SEC-001, SEC-Q5 |
| `temporal/values.yaml` | Temporal Helm values (external PG, basic visibility) | DEP-011 |
| `argocd/bootstrap/` | ArgoCD namespace + root app-of-apps | DEP-012 |
| `argocd/applications/` | Child apps: temporal, operator, config-repo, baseline | DEP-012 |
| `pipelines/infra-bootstrap.yml` | Release pipeline (provision + GitOps bootstrap) | DEP-002 |

## Deploy

```sh
# 1) Validate locally
az bicep build --file infra/bicep/main.bicep --stdout > /dev/null

# 2) Preview (requires an Azure login + the bootstrap Key Vault reachable)
az deployment sub what-if \
  --location eastus \
  --template-file infra/bicep/main.bicep \
  --parameters infra/bicep/main.bicepparam

# 3) Provision (normally run by the release pipeline, not by hand)
az deployment sub create \
  --location eastus \
  --template-file infra/bicep/main.bicep \
  --parameters infra/bicep/main.bicepparam
```

The pipeline (`pipelines/infra-bootstrap.yml`) then installs ArgoCD and applies the root
app, after which GitOps owns the cluster.

## Decisions baked in

- **Workflow engine:** Temporal self-hosted in-cluster, Azure managed PostgreSQL
  persistence, **basic** (SQL) visibility — Elasticsearch deferred (DEP-011).
- **Reconciliation:** ArgoCD (git→cluster sync, drift, rollback) + Goobers operator
  (domain reconcile of CRDs → replicas + Temporal registrations) (DEP-012).
- **Isolation:** per-gaggle namespace + federated workload identity + secret scope; the
  operator creates these per gaggle from `config` (SEC-001/002).
- **Secrets:** referenced from Key Vault via the CSI driver; never in `infra`/`config`
  (SEC-010, CFG-009).
- **Telemetry:** ADX goober-run store, separate from any project telemetry; OTel
  traces/spans exported to it (TEL-001, TEL-010).

## Build-time follow-ups (not yet wired)

- Private networking (VNet integration) for PostgreSQL + Key Vault instead of
  service-firewall allow rules.
- Per-gaggle `FederatedIdentityCredential` + Key Vault role assignments (templated by the
  operator from `config`).
- Warm-pool / pre-warmed run pods (DEP-Q3) and AKS autoscaler sizing under load (DEP-Q4).
- ADX table schemas + per-gaggle partitioning policy + ingest-time PII redaction (TEL-013).
