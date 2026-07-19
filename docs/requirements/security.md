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
  default). The V0-effective security floor is: filesystem permissions + definition
  validation and capability admission via credential non-injection (`SEC-042`,
  `SEC-045`) + token-ref hygiene (`SEC-046`) + redaction (`SEC-041`) + the
  untrusted-input gate (`SEC-047`). `SEC-043` (OIDC) and `SEC-044` (sandboxing)
  arrive at V1 and are not part of the V0 floor.
- **SEC-041 (MUST):** *(All tiers)* Raw secrets MUST NOT land at rest anywhere under
  `runs/` — events, spans, snapshots, **and artifacts** — or in `telemetry.db`. The
  journal package MUST scrub every write path: registry-based for all
  resolver-issued credentials plus pattern-based scanning for secret-shaped
  material, applied **before digesting** so digests commit to the scrubbed bytes.
  Because scanning cannot be perfect, a sanctioned remediation MUST exist:
  `goobers journal redact` replaces a leaked blob and appends a redaction event
  recording old→new digests — the one append-only exception (`TEL-013`,
  `ARCHITECTURE.md §4`).
- **SEC-042 (MUST):** *(All tiers)* **Capability admission, fail closed:** a stage may
  only exercise capabilities its definition declares (e.g. `github:issues:write`,
  `repo:push`, `telemetry:read`). The enforcing components are named per tier: at
  tiers 1–2, the DSL compiler rejects undeclared capability references at validation
  time, and the credential resolver + stage executors **materialize only declared
  capabilities' credentials** into the run environment — undeclared use fails closed
  because nothing is injected. V1 adds runtime containment via sandboxing
  (`SEC-044`); tier 3 adds namespace/identity/network policy. **Stated residual risk
  at tiers 1–2 until V1:** an agentic harness runs as the local user and can reach
  ambient credentials (shell config, keychain, its own signed-in session); Goobers
  does not claim to stop that pre-sandbox. The accepted V0 posture is local trust
  (`SEC-040`) + non-injection + the untrusted-input gate (`SEC-047`) + reviewer and
  human-merge gates (`ARCHITECTURE.md §5`).
- **SEC-043 (MUST):** *(Tier 2, V1)* The portal and daemon MUST support **optional OIDC**
  when exposed beyond the local machine (shared box / small VM). Same `Authenticator`
  seam as tier 3; only the issuer changes.
- **SEC-044 (MUST):** *(Tiers 1–2, V1)* Agentic stage execution MUST be sandboxable —
  constraining filesystem and network reach of the harness process beyond bare worktree +
  process isolation.
- **SEC-045 (MUST):** *(Tiers 1–2)* Credentials MUST be resolved and injected **per
  run, scoped to the stage's declared capabilities** — never ambient to the whole
  daemon. This ships at V0 (it is the enforcement mechanism behind `SEC-042`); V1
  deepens it with per-goober sandbox integration (`SEC-044`) so per-gaggle
  credential scoping (`SEC-002`) holds locally too.
- **SEC-046 (MUST):** *(Tiers 1–2)* Secrets MUST be referenced via `instance.yaml` token
  refs resolving to env vars or a token file (or a team secret store at tier 2) — never
  committed to `config/` or the instance directory. This is the same secret-resolver
  seam Key Vault implements at tier 3 (`SEC-010`).
- **SEC-047 (MUST):** *(All tiers)* **Backlog content is untrusted input.** Backlog
  items can be authored by anyone — on public repos, literally anyone — and their
  text MUST be treated as data, never as instructions carrying authority. Claim
  eligibility MUST be gated: the default for public repos is a trust label applied
  by a user with triage/write permission (e.g. `goobers:approved`), which the
  provider's own permission model makes enforceable — workflows MUST NOT claim
  unapproved items. Instances on private/trusted backlogs MAY relax the gate
  explicitly in config.

### Isolation

- **SEC-001 (MUST):** **Tier 3 (V2):** Each gaggle MUST run in its own k8s namespace.
  Tiers 1–2 counterpart: per-gaggle scoping (`GAG-011`) plus worktree + process
  isolation per run (`SEC-004`).
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
- **SEC-021 (MUST):** *(All tiers)* The Tutor MUST be granted write access to the
  `config` repo/directory only — never the provisioning surface (`instance.yaml`
  locally; the `infra` repo at tier 3) (`TUT-005`, `CFG-010`, `ARCHITECTURE.md §6`).
  Enforcement: at tiers 1–2 the Tutor's stages receive a write grant scoped to
  `config/` (capability admission — a same-user local directory gives runtime
  enforcement, not an OS boundary); backing `config` with its own git remote +
  required review upgrades this to a **hard permission boundary** and is
  recommended at tiers 1–2, required at tier 3, where repo + identity permissions
  enforce it. Self-modification MUST also respect the configured approval gate.
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
- **SEC-Q6:** **Resolved (V1):** OS-native sandboxing for local agentic stages
  (`SEC-044`) — Seatbelt on macOS and bubblewrap on Linux; containers are deferred.
  See [`ADR 0001`](../adr/0001-agentic-sandbox-mechanism.md).
