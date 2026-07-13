# Design: Sandboxing + per-goober credential injection (isolation rung 2) — V1 epic #35

> Status: **Draft for review** · Area prefix: `SEC` · Milestone: **V1**
> Requirements: [`docs/requirements/security.md`](../../requirements/security.md)
> (SEC-044/045, SEC-Q6) · Architecture: [`docs/ARCHITECTURE.md`](../../ARCHITECTURE.md) §9
>
> Detailed-design artifact for epic **#35**. The dispatchable work items (S0–S4)
> each link back to the correspondingly-named section here. This is the most
> security-sensitive epic in the pass; fail-closed is the rule throughout.

## 1. Verdict

**Half-built, and gated on one open decision.** Credential injection is already
**capability-scoped and fail-closed** ([`internal/credentials/capability.go`](../../../internal/credentials/capability.go):
`Injector`, `Grant`, `ErrUndeclaredCapability`, `ErrNoCredentialForCapability`) — nothing
is materialized for an undeclared capability. What's missing is **isolation rung 2**:

- **Per-goober** credential injection (SEC-045) — today injection is per-*stage*; it must
  be scoped to a goober identity.
- **Sandboxed agentic execution (SEC-044) is unbuilt.** [`internal/harness/copilot.go`](../../../internal/harness/copilot.go)
  runs `copilot` as a bare subprocess and explicitly notes isolation is "deferred to V1."
- **The sandbox mechanism is an OPEN question (SEC-Q6)** — container vs OS-native vs
  harness-native, per platform. This must be resolved by a **spike (S0) before the impl
  missions are dispatchable.** Sending sandbox impl to devs without S0 would violate the
  PO's "no vague work" rule.

## 2. Scope boundary

**In scope (V1, tiers 1–2):** per-goober credential injection; a sandbox for agentic
stages confining the subprocess filesystem to its stage worktree; egress posture
documented (and enforced where the chosen mechanism makes it cheap); fail-closed when the
sandbox is unavailable.

**Out of scope — V2 (do NOT build):** tier-3 namespace/pod isolation, network-policy
egress enforcement (SEC-Q5 is resolved as tier-3/V2), Key Vault/managed identity (that's
the #38 secret-resolver seam).

## 3. The gating decision: SEC-Q6 sandbox mechanism (spike S0)

The Copilot CLI runs as a **locally signed-in binary** that needs `HOME`, `PATH`, and
keychain/token access to function — which makes full containerization awkward (auth and
credential plumbing break inside a clean container). The realistic rung-2 options:

| Mechanism | macOS | Linux | Notes |
|-----------|-------|-------|-------|
| **OS-native sandbox** | `sandbox-exec` (Seatbelt profile) | `bwrap` (bubblewrap) / namespaces | Confines FS to the worktree cheaply; **Seatbelt is deprecated-but-functional** — residual risk to document |
| Container | Docker/Apple container | Docker/Podman | Strong isolation but breaks Copilot's local auth; heavyweight |
| Harness-native | limited | limited | Only as strong as the CLI's own flags |

**Recommendation (to be confirmed by S0):** OS-native sandbox as rung 2 — Seatbelt on
macOS, bubblewrap on Linux — confining the agentic subprocess FS to its stage worktree,
container deferred. S0 must validate this against a real `copilot -p` run on macOS
(auth still works, FS confined) and produce an ADR. **This is the key call for PO/PM /
Special-Agent-security** — see OQ-1.

## 4. Missions (dispatchable, single-PR-sized)

### S0 — Sandbox mechanism spike (SEC-Q6) — gates S2/S3/S4
- Evaluate OS-native vs container vs harness-native for confining the `copilot` subprocess
  on **macOS and Linux**; validate a real `copilot -p` run still authenticates under the
  chosen mechanism with FS confined to the worktree; write an ADR + a thin prototype seam.
- **Deliverable:** decision record + `Sandbox` interface shape the impl missions build on.
- **Test plan:** prototype confines a scripted subprocess to a temp worktree (write outside
  → denied) on both OSes in CI; documented result for the macOS Copilot auth check.

### S1 — Per-goober credential injection (SEC-045)
- Scope the `credentials.Injector` to a **goober identity** (per-goober grants), not just
  the stage's declared capability set; keep fail-closed (`ErrUndeclaredCapability` /
  `ErrNoCredentialForCapability`). Independent of S0.
- **Seams:** `internal/credentials/capability.go`, harness `credentialEnv`, runner wiring.
- **Test plan:** goober A cannot resolve goober B's grant; undeclared capability fails
  closed; a declared-but-ungranted capability fails closed; redaction still applies.

### S2 — Filesystem confinement to the stage worktree (SEC-044, part)
- Using S0's `Sandbox`, confine the agentic subprocess so reads/writes outside its stage
  worktree are denied. Depends on **S0**.
- **Seams:** `internal/worktree`, `internal/harness`, S0 sandbox seam.
- **Test plan:** a stage that attempts to read/write outside the worktree is denied and the
  run fails closed with a clear journal event; in-worktree I/O succeeds.

### S3 — Sandboxed agentic execution (SEC-044, core)
- Wrap the harness subprocess launch in S0's mechanism; **fail-closed if the sandbox is
  unavailable** (block the run) with an explicit, logged opt-out for trusted-local
  (dogfood) use. Depends on **S0**.
- **Seams:** `internal/harness/copilot.go` (process launch), S0 sandbox seam.
- **Test plan:** sandbox-unavailable → run blocked + journal event (not a silent bypass);
  opt-out flag path is logged; a normal run executes sandboxed and completes.

### S4 — Egress posture documentation (+ optional enforcement)
- Document the V1 egress posture: what the chosen sandbox does/doesn't restrict on the
  network, and the stated residual risk. If the mechanism supports egress restriction
  cheaply, offer it as opt-in; else document only. No tier-3 network policy (V2).
- **Seams:** docs; S0 mechanism.
- **Test plan:** doc-lint; if enforcement offered, an egress-blocked stage is denied network
  and the residual-risk section matches the mechanism's actual guarantees.

## 5. End-to-end / integration test

An agentic stage runs under the chosen sandbox with per-goober credentials through the
**real local runner + fake harness**: assert FS confinement (out-of-worktree denied),
per-goober credential scoping (no cross-goober leakage), and fail-closed on
sandbox-unavailable. Journal-only assertions.

## 6. Dependencies & consumers

- **S0 gates S2/S3/S4.** S1 is independent and can land first.
- **Feeds #34** (per-gaggle credentials, that epic's OQ-3) and **#38** (secret-resolver seam).
- Coordinate S0's mechanism choice with **Goobers-Special-Agent** (security/target-arch).

## 7. Open questions (for PM / PO / Special-Agent-security)

- **OQ-1 — SEC-Q6 mechanism (the big one):** OK to adopt OS-native (Seatbelt/macOS,
  bubblewrap/Linux) as rung 2 with container deferred, accepting the documented
  Seatbelt-deprecation residual? *(Recommend: yes, pending S0 validation.)*
- **OQ-2 — sandbox-unavailable behavior:** fail-closed/block (recommend) vs. warn-and-proceed?
  *(Recommend: fail-closed, with a logged trusted-local opt-out.)*
- **OQ-3 — credential granularity:** per-goober (SEC-045) sufficient for V1, or per-goober
  **and** per-gaggle now? *(Recommend: per-goober in V1; per-gaggle rides on #34.)*
- **OQ-4 — egress:** document-only for V1, or ship opt-in enforcement if cheap under the
  chosen mechanism? *(Recommend: document-only unless S0 finds egress control is low-cost.)*
