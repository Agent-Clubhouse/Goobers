# Spec: Security & Isolation

> Status: **Draft** · Derives from `../VISION.md` §6 + isolation/secrets decisions · Area prefix: `SEC`

## Purpose

Goobers act on real repos with real credentials, so isolation and least-privilege are
core, not afterthoughts. This spec defines how gaggles are contained, how secrets and
identity work, and how interactive actions are authorized.

## Model

- **Per-gaggle isolation (decided):** each gaggle runs in its **own k8s namespace** with
  its **own Azure identity** and secret scope. Cross-gaggle access is prevented.
- **Secrets referenced, never stored:** the infra repo references secrets from a secret
  store (**Azure Key Vault**); secrets are never embedded in code (`CFG-009`).
- **Least privilege:** a goober run's credentials are scoped to that gaggle's target repo,
  backlog, and telemetry write — nothing broader.
- **Ephemeral, isolated runs:** run pods are fresh, isolated, and torn down after use.
- **Authorized interactivity:** portal runtime actions (gate approvals, run intervention)
  are access-controlled.

## Requirements

### Isolation
- **SEC-001 (MUST):** Each gaggle MUST run in its own k8s namespace.
- **SEC-002 (MUST):** Each gaggle MUST have its own Azure identity/credential scope;
  cross-gaggle access MUST be prevented.
- **SEC-003 (MUST):** Goober-run telemetry MUST be partitioned/scoped per gaggle within
  the shared store (`TEL-Q4`).
- **SEC-004 (MUST):** Run pods MUST be ephemeral and isolated, and torn down after the run
  (`DEP-007`).

### Secrets & identity
- **SEC-010 (MUST):** Secrets MUST be referenced from a secret store (Key Vault), never
  stored in the `infra` or `config` repo.
- **SEC-011 (MUST):** Goober run credentials MUST follow least privilege — scoped to the
  gaggle's target repo + backlog + telemetry write only.
- **SEC-012 (MUST):** Harness auth and git/provider auth MUST be injected into run pods
  securely (not baked into images or the repo).

### Authorization & audit
- **SEC-020 (MUST):** Portal interactive actions (gate approvals, run intervention) MUST
  be access-controlled via **Microsoft Entra ID** (SSO + RBAC).
- **SEC-021 (MUST):** The Tutor's identity MUST be granted write access to the `config`
  repo only (never the `infra` repo), making its change scope a permission boundary, not
  just policy (`TUT-005`, `CFG-010`). Self-modification MUST also respect the configured
  approval gate.
- **SEC-022 (SHOULD):** Security-relevant actions (approvals, credential use, definition
  changes) SHOULD be auditable via telemetry.

### Containment
- **SEC-030 (SHOULD):** A goober's tool/permission surface SHOULD be constrained
  (allowlisting) to limit exfiltration or out-of-scope actions (`GBO-Q2`).

## Relationships

- Enforces isolation for → **Gaggle** (`GAG-005`).
- Constrains → **Goober** runs and the **Tutor**.
- Gates → **Portal** interactive actions.
- Partitions → the **Telemetry** store.

## Open questions

- **SEC-Q1:** **Resolved (default):** AKS **workload identity** (managed-identity
  federation) per gaggle. *(Build-time: federation/role specifics.)*
- **SEC-Q2:** **Resolved (default):** Key Vault via CSI driver; harness + git tokens
  short-lived, injected per run, never in images/repo. *(Build-time: exact flow.)*
- **SEC-Q3:** **Resolved:** **Microsoft Entra ID** for portal + system auth (SSO + RBAC
  for who may approve/intervene). See `VISION §8`.
- **SEC-Q4:** **Resolved (default):** per-goober **tool allowlist** in the definition
  (default-deny beyond declared tools).
- **SEC-Q5:** **Resolved (default):** restricted pod **egress** via network policy
  (allowlist provider/telemetry endpoints).
