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
  stored in the goober-infra repo.
- **SEC-011 (MUST):** Goober run credentials MUST follow least privilege — scoped to the
  gaggle's target repo + backlog + telemetry write only.
- **SEC-012 (MUST):** Harness auth and git/provider auth MUST be injected into run pods
  securely (not baked into images or the repo).

### Authorization & audit
- **SEC-020 (MUST):** Portal interactive actions (gate approvals, run intervention) MUST
  be access-controlled (authN + authZ).
- **SEC-021 (MUST):** Tutor self-modification MUST respect the configured approval gate
  and stay within its bounded change scope (`TUT-005`).
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

- **SEC-Q1:** Identity mechanism specifics — AKS workload identity / managed identity
  federation per gaggle.
- **SEC-Q2:** Secure injection flow for Copilot harness auth + git provider auth into
  ephemeral pods.
- **SEC-Q3:** Portal identity provider (Entra ID?) and role model (who may approve/
  intervene).
- **SEC-Q4:** Tool allowlisting model to bound goober capability/exfiltration risk.
- **SEC-Q5:** Network egress policy for run pods.
