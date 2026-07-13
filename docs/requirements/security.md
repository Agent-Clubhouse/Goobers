# Spec: Security & Isolation

> Status: **Draft** · Aligned to ../ARCHITECTURE.md (2026-07-12) · Derives from ../VISION.md §5, §8 · Area prefix: `SEC`

## Purpose

Goobers act on real repos with real credentials, so isolation and least-privilege are
core, not afterthoughts. This spec defines the **auth/isolation ladder** across the three
deployment tiers (`ARCHITECTURE.md §9`): how runs are contained, how secrets and identity
work, and how interactive actions are authorized. The protocol (OIDC) and the seams (an
`Authenticator` + a secret-resolver interface) are constant; tiers select implementations.

## Model

- **The ladder (decided):**

  | Tier | Identity/auth | Secrets | Isolation |
  |---|---|---|---|
  | 1 — Solo | None (local trust) | Env vars / token file, redacted from journals | Worktree + process isolation, capability admission |
  | 2 — Team | Optional OIDC on portal/daemon | Env/file or team secret store | + sandboxed stage execution, per-goober credential injection (V1) |
  | 3 — Cloud (V2) | Entra ID (OIDC) | Azure Key Vault | Per-gaggle namespaces + identities, network policy |

- **Fail closed:** undeclared capabilities, unvalidated definitions, or an unwritable
  journal stop a run rather than degrade it (`ARCHITECTURE.md §2`).
- **Secrets referenced, never stored:** token refs in `instance.yaml` (tiers 1–2) or Key
  Vault references (tier 3); secrets are never embedded in code (`CFG-009`) and never
  land in run journals.
- **Least privilege:** a goober run's credentials are scoped to that gaggle's target repo,
  backlog, and telemetry write — nothing broader.
- **Ephemeral, isolated runs:** run environments are fresh, isolated, and torn down after
  use — worktree + process at tiers 1–2, pod at tier 3 (`DEP-004`, `DEP-007`).
- **Authorized interactivity:** portal runtime actions (gate approvals, run intervention)
  are access-controlled per the ladder — local trust, optional OIDC, then Entra RBAC.

## Requirements

### Local-tier baseline (tiers 1–2)

- **SEC-040 (MUST):** *(Tier 1)* Tier 1 MUST require no identity provider or auth service
  to function: the daemon and portal operate under local user trust (bind local by
  default). Security floor = filesystem permissions + the requirements below.
- **SEC-041 (MUST):** *(All tiers)* Credentials and known secret material MUST be
  **redacted before events/spans are written** to the run journal or rollup store; raw
  secrets MUST NOT land at rest in `runs/` or `telemetry.db` (`TEL-013`,
  `ARCHITECTURE.md §4`).
- **SEC-042 (MUST):** *(All tiers)* **Capability admission, fail closed:** a stage may
  only exercise capabilities its definition declares (e.g. `github:issues:write`,
  `repo:push`, `telemetry:read`). Undeclared use MUST stop the stage, not degrade it
  (`ARCHITECTURE.md §5`).
- **SEC-043 (MUST):** *(Tier 2, V1)* The portal and daemon MUST support **optional OIDC**
  when exposed beyond the local machine (shared box / small VM). Same `Authenticator`
  seam as tier 3; only the issuer changes.
- **SEC-044 (MUST):** *(Tiers 1–2, V1)* Agentic stage execution MUST be sandboxable —
  constraining filesystem and network reach of the harness process beyond bare worktree +
  process isolation.
- **SEC-045 (MUST):** *(Tiers 1–2, V1)* Credentials MUST be resolved and injected
  **per goober, per run** into the run environment — never ambient to the whole daemon —
  so per-gaggle credential scoping (`SEC-002`) holds locally too.
- **SEC-046 (MUST):** *(Tiers 1–2)* Secrets MUST be referenced via `instance.yaml` token
  refs resolving to env vars or a token file (or a team secret store at tier 2) — never
  committed to `config/` or the instance directory. This is the same secret-resolver
  seam Key Vault implements at tier 3 (`SEC-010`).

### Isolation

- **SEC-001 (MUST):** **Tier 3 (V2):** Each gaggle MUST run in its own k8s namespace.
  Tiers 1–2 counterpart: worktree + process isolation per run (`SEC-004`).
- **SEC-002 (MUST):** **Tier 3 (V2):** Each gaggle MUST have its own Azure identity/
  credential scope; cross-gaggle access MUST be prevented. Tiers 1–2 approximate this
  via per-goober credential injection (`SEC-045`).
- **SEC-003 (MUST):** *(All tiers)* Goober-run telemetry MUST be partitioned/scoped per
  gaggle within the shared store (`TEL-Q4`) — attribute-scoped in the journal + SQLite
  rollup at tiers 1–2, partitioned in ADX at tier 3.
- **SEC-004 (MUST):** *(All tiers)* Run environments MUST be ephemeral and isolated, and
  torn down after the run (`DEP-004`, `DEP-007`) — disposable git worktrees + processes
  at tiers 1–2; **Tier 3 (V2):** ephemeral pods.

### Secrets & identity

- **SEC-010 (MUST):** **Tier 3 (V2):** Secrets MUST be referenced from a secret store
  (**Azure Key Vault**), never stored in the `infra` or `config` repo. Tiers 1–2
  counterpart: `SEC-046`.
- **SEC-011 (MUST):** *(All tiers)* Goober run credentials MUST follow least privilege —
  scoped to the gaggle's target repo + backlog + telemetry write only.
- **SEC-012 (MUST):** *(All tiers)* Harness auth and git/provider auth MUST be injected
  into run environments securely — never baked into images, the binary, or any repo.
  Tiers 1–2: env/file injection at run setup (`SEC-045`, `SEC-046`). **Tier 3 (V2):**
  Key Vault injection into run pods.

### Authorization & audit

- **SEC-020 (MUST):** **Tier 3 (V2):** Portal interactive actions (gate approvals, run
  intervention) MUST be access-controlled via **Microsoft Entra ID** (SSO + RBAC).
  Ladder below tier 3: local trust at tier 1 (`SEC-040`), optional OIDC at tier 2
  (`SEC-043`) — one `Authenticator` seam throughout.
- **SEC-021 (MUST):** *(All tiers)* The Tutor's identity MUST be granted write access to
  the `config` repo/directory only — never the provisioning surface (`instance.yaml`
  locally; the `infra` repo at tier 3) — making its change scope a **permission
  boundary**, not just policy (`TUT-005`, `CFG-010`, `ARCHITECTURE.md §6`). Enforced via
  filesystem/repo permissions at tiers 1–2 and repo + identity permissions in the cloud.
  Self-modification MUST also respect the configured approval gate.
- **SEC-022 (SHOULD):** *(All tiers)* Security-relevant actions (approvals, credential
  use, definition changes) SHOULD be auditable — via the append-only run journal and the
  telemetry store.

### Containment

- **SEC-030 (SHOULD):** *(All tiers)* A goober's tool/permission surface SHOULD be
  constrained (allowlisting) to limit exfiltration or out-of-scope actions (`GBO-Q2`) —
  realized at every tier by capability admission (`SEC-042`); **Tier 3 (V2)** adds
  restricted pod egress via network policy.

## Relationships

- Enforces isolation for → **Gaggle** (`GAG-005`) and every **run environment**
  (`DEP-004`).
- Constrains → **Goober** runs (capability admission) and the **Tutor** (write boundary).
- Gates → **Portal** interactive actions (auth ladder).
- Partitions → the **Telemetry** store.

## Open questions

- **SEC-Q1:** **Resolved (default, Tier 3, V2):** AKS **workload identity**
  (managed-identity federation) per gaggle. *(Build-time: federation/role specifics.)*
- **SEC-Q2:** **Resolved (default, Tier 3, V2):** Key Vault via CSI driver; harness + git
  tokens short-lived, injected per run, never in images/repo. *(Build-time: exact flow.)*
- **SEC-Q3:** ~~Resolved: Microsoft Entra ID for portal + system auth~~ **Re-resolved
  (2026-07-12):** identity/auth is a **ladder by tier** — none (local trust) → optional
  OIDC → Entra ID — behind one `Authenticator` seam; Entra remains the tier-3 issuer.
  See `ARCHITECTURE.md §9`, `SEC-040`, `SEC-043`, `SEC-020`.
- **SEC-Q4:** ~~Resolved (default): per-goober tool allowlist~~ **Re-resolved
  (2026-07-12):** generalized to **capability admission, fail closed** at every tier
  (`ARCHITECTURE.md §5`); the per-goober allowlist in the definition remains the
  declaration surface (default-deny beyond declared capabilities). See `SEC-042`.
- **SEC-Q5:** **Resolved (default, Tier 3, V2):** restricted pod **egress** via network
  policy (allowlist provider/telemetry endpoints).
- **SEC-Q6:** *(build-time design)* Sandboxing mechanism for local agentic stages at V1
  (`SEC-044`) — container, OS sandbox, or harness-native — per platform.
