# Design: Sandboxing + per-goober credential injection (isolation rung 2) — V1 epic #35

> Status: **draft — for review** · Area prefix: `SEC` · Milestone: **V1**
> Requirements: [`docs/requirements/security.md`](../../requirements/security.md)
> (SEC-044/045, SEC-Q6) · Architecture: [`docs/ARCHITECTURE.md`](../../ARCHITECTURE.md) §9
>
> Detailed-design artifact for epic **#35**. The dispatchable work items (S0–S4)
> each link back to the correspondingly-named section here. This is the most
> security-sensitive epic in the pass; fail-closed is the rule throughout.
>
> Progress (2026-08-10): S1 per-goober credential scoping **shipped** (#823); S0
> sandbox mechanism **decided**
> ([ADR 0001](../../adr/0001-agentic-sandbox-mechanism.md)) with the native
> implementation landed (`internal/sandbox`, darwin + linux). Write confinement shipped
> in #1418, S3 fail-closed enforcement shipped in #166, and the S4 egress posture shipped
> in #167; S2 remains open for read-path confinement (#165). Windows remains at the
> explicitly logged W0 rung (§3.1, #651), with native sandbox construction returning
> `sandbox.ErrUnsupported`.

## 1. Verdict

**Half-built, with the sandbox mechanism decided.** Credential injection is already
**capability-scoped and fail-closed** ([`internal/credentials/capability.go`](../../../internal/credentials/capability.go):
`Injector`, `Grant`, `ErrUndeclaredCapability`, `ErrNoCredentialForCapability`) — nothing
is materialized for an undeclared capability. What's missing is **isolation rung 2**:

- **Per-goober** credential injection (SEC-045) — today injection is per-*stage*; it must
  be scoped to a goober identity.
- **Sandboxed agentic execution (SEC-044) is partially shipped.** Native write
  confinement shipped in #1418, and S3 enforcement shipped in #166:
  [`internal/harness/confine.go`](../../../internal/harness/confine.go) places Copilot
  runtime state inside the workspace, narrows linked-worktree writable roots, wraps the
  command with `sandbox.Sandbox`, and fails closed when an enforced sandbox is unavailable.
  Read-path confinement remains open in #165. Windows remains at W0 because
  [`internal/sandbox/native_other.go`](../../../internal/sandbox/native_other.go) returns
  `sandbox.ErrUnsupported`.
- **The sandbox mechanism is resolved (SEC-Q6):** OS-native Seatbelt on macOS and
  bubblewrap on Linux, with containers deferred. Windows initially uses a named,
  unsandboxed-with-warning W0 posture, not an implicit omission (§3.1). See
  [ADR 0001](../../adr/0001-agentic-sandbox-mechanism.md). The S0 spike established the
  seam and residual risks before the implementation missions became dispatchable.

## 2. Scope boundary

**In scope (V1, tiers 1–2):** per-goober credential injection; a sandbox for agentic
stages on macOS/Linux confining the subprocess filesystem to its stage worktree; a stated,
observable Windows posture; egress posture documented (and enforced where the chosen
mechanism makes it cheap); fail-closed when a configured sandbox is unavailable.

**Out of scope — V2 (do NOT build):** tier-3 namespace/pod isolation, network-policy
egress enforcement (SEC-Q5 is resolved as tier-3/V2), Key Vault/managed identity (that's
the #38 secret-resolver seam). A Windows sandbox implementation is also out of scope for
the #651 design decision.

## 3. The SEC-Q6 sandbox mechanism decision (spike S0)

The Copilot CLI runs as a **locally signed-in binary** that needs `HOME`, `PATH`, and
keychain/token access to function — which makes full containerization awkward (auth and
credential plumbing break inside a clean container). The realistic rung-2 options:

| Mechanism | macOS | Linux | Notes |
|-----------|-------|-------|-------|
| **OS-native sandbox** | `sandbox-exec` (Seatbelt profile) | `bwrap` (bubblewrap) / namespaces | Confines FS to the worktree cheaply; **Seatbelt is deprecated-but-functional** — residual risk to document |
| Container | Docker/Apple container | Docker/Podman | Strong isolation but breaks Copilot's local auth; heavyweight |
| Harness-native | limited | limited | Only as strong as the CLI's own flags |

**Decision:** OS-native sandbox as rung 2 — Seatbelt on
macOS, bubblewrap on Linux — confining the agentic subprocess FS to its stage worktree,
container deferred. S0 validated this against a real `copilot -p` run on macOS
(auth still works with a fresh in-worktree `COPILOT_HOME` and no
out-of-worktree writable roots) and recorded the decision and residual risks in
[ADR 0001](../../adr/0001-agentic-sandbox-mechanism.md).

### 3.1 Windows isolation ladder (#651)

Windows needs an explicit ladder even before it has a sandbox implementation. Otherwise
the absence of a Seatbelt/bubblewrap equivalent silently grants a stage all of the daemon
user's authority. W0–W2 increase local authority confinement. W3 is a containerized
deployment branch rather than an automatically stronger security rung: process isolation
narrows the environment but shares the host kernel, while Hyper-V isolation adds the
separate-kernel boundary required for hostile workloads.

| Rung | Mechanism | Filesystem guarantee | Network guarantee | Process spawning | Cost and harness-compatibility risk |
|---|---|---|---|---|---|
| **W0** | **Explicitly unsandboxed with warning** | None beyond the daemon user's normal ACLs. The worktree is an organizational workspace, not an authority boundary; a stage can read and write anything the daemon user can. | None. The stage has the host user's network reachability. A separate `run.network: none` request keeps its existing fail-closed or explicit trusted-local override contract; W0 itself does not satisfy it. | Children run with the daemon user's token. A Job Object still owns the tree's lifetime, but does not reduce its authority. | Lowest cost and no additional CLI compatibility risk. Highest security risk, so it is trusted-local-only, visibly warned, and recorded for every attempt. |
| **W1** | **Restricted token (`CreateRestrictedToken`) plus low-integrity level** | Removes selected SIDs/privileges and prevents writes to medium/high-integrity objects. The worktree and required runtime roots need narrowly-scoped DACL and mandatory-label grants. It is a write-restriction rung, not broad read confinement: normal DACL-readable host files can remain readable. | None; a restricted token is not a network broker. Network credentials available to the token may still be used. | Descendants inherit the restricted/low-integrity token unless a separate privileged broker creates them. They also remain in the stage Job Object. | Moderate implementation and ACL-cleanup cost. Copilot/Node profile, temp, credential-store, and updater behavior under low integrity is unverified; an in-worktree `COPILOT_HOME` may help state writes but does not prove authentication works. |
| **W2** | **AppContainer** (LPAC where the stronger boundary is required) | A regular AppContainer removes the daemon user's broad authority, but it can still read system paths and other objects whose DACL grants access to package-wide SIDs such as `ALL APPLICATION PACKAGES`, as well as objects granted to its capability or unique AppContainer SID. The effective ambient and explicit grant set must be inventoried and audited; disposable-workspace grants must be removed when reaped. A Less Privileged AppContainer (LPAC) excludes `ALL APPLICATION PACKAGES` grants, but `ALL RESTRICTED APPLICATION PACKAGES`, capability, and unique-SID grants still require the same audit. Use LPAC when W2 must exclude the regular AppContainer's broad package access. | Network is denied unless explicit AppContainer network capabilities are granted, so this rung can broker egress rather than merely document it. The Copilot service endpoints still require an intentionally granted network capability. | Child processes must remain in the same AppContainer/lowbox token and the stage Job Object. Launches that require an unsandboxed broker are outside the rung and must fail rather than escape. | High setup and cleanup cost: profile/SID lifecycle, per-worktree ACLs, auditing package-wide and capability grants, runtime dependency access, and credential brokering. LPAC further increases compatibility risk. Arbitrary Win32/Node CLIs are known compatibility risks; Copilot CLI install, auth, subprocesses, and updates all require native-Windows proof. |
| **W3** | **Container-only** (process-isolated Windows container; Hyper-V isolation where a security boundary is required) | A separate container filesystem plus minimal explicit workspace/runtime mounts narrows ordinary host-filesystem reach. The process must run as least-privilege `ContainerUser`, never `ContainerAdministrator`; mounts are read-only unless the stage requires writes. Process isolation shares the host kernel and is not a robust security boundary for hostile workloads: a host escape or over-broad mount can expose host authority. Only Hyper-V isolation supplies the separate-kernel boundary needed to rank W3 above W2 as a security boundary. | Container networking can apply an explicit egress policy; no claim is made until that policy is configured and tested. | Descendants remain in the container under `ContainerUser`. A Job Object (inside the worker or at the container-host boundary) still supplies stage cancellation and cleanup; container teardown is additional defense, not a substitute for runner lifecycle semantics. | Highest operational cost: compatible Windows host/base-image versions, image lifecycle, workspace mounts, least-privilege account setup, and explicit Copilot authentication forwarding. Process isolation retains host-escape risk; Hyper-V isolation adds VM startup/resource cost and must be selected for hostile or multi-tenant stages. W3 fits tier-3 workers; under this option local Windows daemons remain explicitly at W0. |

**Initial Windows decision: W0.** The exact operator-facing posture is:
**"none yet — trusted local only, logged."** Deterministic and, if #647 permits them,
agentic stages execute with the daemon user's filesystem and network authority. Existing
capability admission and credential non-injection still limit which credentials Goobers
materializes, but they do not constrain what the subprocess can do with the user's ambient
authority.

W0 is a named Windows policy, not an automatic downgrade. A node must advertise/select
that posture explicitly and warn before dispatch. If W1, W2, or W3 is configured but
cannot be established, execution fails closed; the runner never falls back to W0. This
preserves S0's unavailable-mechanism rule while allowing the curated Windows honest floor
to be represented deliberately. W1 is the next practical hardening spike, W2 is the
preferred local authority boundary if harness compatibility and ACL lifecycle prove
tractable, and W3 is reserved for containerized tier-3 workers. A process-isolated W3
does not supersede W2's authority boundary; only a Hyper-V-isolated W3 may be treated as
the strongest rung.

### 3.2 Job Objects: lifetime is not authority

P2 (#623) supplies process-tree **lifetime control** on Windows: descendants are assigned
to a Job Object so cancellation, timeout, daemon failure, and explicit termination reap
the whole tree. It does not sandbox filesystem or network access and does not make W0 a
restricted posture.

Every Windows rung composes both mechanisms:

1. Create the child with the rung's authority token/boundary (daemon token for W0,
   restricted token for W1, AppContainer token for W2, or the container boundary for W3).
2. Assign it to the stage Job Object before it can run and spawn unowned descendants.
3. Resume it only after both setup steps succeed. Authority setup failure and Job Object
   assignment failure both fail closed.

The `internal/sandbox.Sandbox` seam continues to own authority confinement while
`internal/platform/proc` owns start, wait, cancel, and tree termination. Neither layer may
swallow or replace the other's guarantees.

### 3.3 P10 harness verdict and degradation rule

As of 2026-07-26, P10 (#647) has **no final compatibility verdict**. Its earlier
`NATIVE_WINDOWS_UNAVAILABLE` escalation was superseded by the decision to use a
GitHub-hosted `windows-latest` runner, but the unauthenticated mechanics and authenticated
Copilot probes have not yet produced the required findings document. Windows is therefore
supported for deterministic workloads; agentic support remains provisional.

The P10 result is consumed as follows:

- If Copilot CLI is supported (with or without documented caveats) when run bare under
  the Job Object model, trusted-local nodes may run agentic stages at W0 with the same
  warning and journal record as deterministic stages.
- If Copilot CLI cannot run on Windows at all, Windows remains deterministic-only.
  Admission rejects or routes agentic stages before launch; it must not invoke a broken
  harness or report W0 as successful isolation.
- If the harness runs at W0 but fails a future W1/W2 probe, that stronger rung is
  unavailable for agentic stages. The runner may use W0 only when the node explicitly
  selected the trusted-local policy; otherwise it blocks or routes the stage. It never
  silently drops from a configured stronger rung.

### 3.4 Observable posture

Each Windows stage attempt, deterministic or agentic, records one
`runner.isolation.posture` event before its subprocess starts. This existing
runner-namespaced event is excluded from cross-runner conformance but remains part of the
append-only run journal. Its `runner` payload records at least:

```json
{
  "platform": "windows",
  "posture": "unsandboxed-warning",
  "mechanism": "none",
  "authority": "daemon-user",
  "filesystem": "ambient",
  "network": "ambient",
  "lifetime": "job-object",
  "trustedLocalOnly": true
}
```

The `stage` and `attempt` event fields identify the execution. W1–W3 replace `posture`,
`mechanism`, and the effective filesystem/network values with the boundary actually
established. The event describes effective state, not requested configuration. Failure to
write it blocks launch, and operator logs emit the same prominent warning for W0:
`Windows isolation: none yet — trusted local only, logged; stage executes with the daemon
user's authority.` No credential value, profile path, or other secret belongs in either
record.

## 4. Missions (dispatchable, single-PR-sized)

### S0 — Sandbox mechanism spike (SEC-Q6) — complete; gates S2/S3/S4
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
  worktree are denied. Depends on **S0**. *(Independent but complementary: #120 fixes the
  executor's own declared-output-file reads — traversal/symlink containment on the
  journal side; the sandbox confines the subprocess. Both are required; neither
  substitutes for the other.)*
- **Seams:** `internal/worktree`, `internal/harness`, S0 sandbox seam.
- **Test plan:** a stage that attempts to read/write outside the worktree is denied and the
  run fails closed with a clear journal event; in-worktree I/O succeeds.

### S3 — Sandboxed agentic execution (SEC-044, core)
- Wrap the harness subprocess launch in S0's mechanism; **fail-closed if the sandbox is
  unavailable** (block the run) with an explicit, logged opt-out for trusted-local
  (dogfood) use. Depends on **S0**. *(Coordinate with #119: the sandbox wrapper must
  preserve the process-group timeout/kill semantics that issue adds — a sandbox that
  swallows the group kill reintroduces the hung-harness bug.)*
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

- **OQ-1 — SEC-Q6 mechanism: Resolved.** Adopt OS-native Seatbelt/macOS and
  bubblewrap/Linux as rung 2 with containers deferred, accepting the residual risks
  recorded in [ADR 0001](../../adr/0001-agentic-sandbox-mechanism.md).
- **OQ-2 — sandbox-unavailable behavior:** fail-closed/block (recommend) vs. warn-and-proceed?
  *(Recommend: fail-closed, with a logged trusted-local opt-out.)*
- **OQ-3 — credential granularity:** per-goober (SEC-045) sufficient for V1, or per-goober
  **and** per-gaggle now? *(Recommend: per-goober in V1; per-gaggle rides on #34.)*
- **OQ-4 — egress:** document-only for V1, or ship opt-in enforcement if cheap under the
  chosen mechanism? *(Recommend: document-only unless S0 finds egress control is low-cost.)*

## 8. Appendix: draft Windows follow-up issues

These are planning drafts only: they are **not filed or approved work**. All are gated on
S0's accepted outcome — native authority mechanisms behind `internal/sandbox.Sandbox`,
process lifecycle left with the harness runner, and no silent downgrade when a configured
mechanism is unavailable. If ADR 0001 or that seam changes, these drafts must be
re-evaluated before dispatch.

### P11-I1 — Implement the named W0 posture and observability

- Add an explicit Windows `unsandboxed-warning` node policy rather than treating
  `sandbox.ErrUnsupported` as permission to continue.
- Emit the §3.4 posture event and operator warning for every Windows stage attempt,
  including deterministic stages; refuse launch if the event cannot be journaled.
- Preserve fail-closed behavior for a configured W1–W3 posture and preserve the existing
  `run.network: none` policy; W0 is not evidence of network enforcement.
- Gate agentic acceptance tests on #647's verdict. Deterministic fake-harness coverage is
  independently dispatchable once the S0 seam is confirmed.

### P11-S1 — Spike W1, then W2, with the real Windows harness

- Prototype restricted-token/low-integrity confinement first; measure host read exposure,
  out-of-worktree write denial, ACL cleanup, child-token inheritance, and Job Object
  composition.
- Attempt AppContainer only if W1's residual read/network authority is insufficient for
  the intended risk tier. Exercise profile/SID lifecycle, inventory package-wide and
  capability-SID access, evaluate LPAC where those ambient grants exceed the required
  boundary, test narrow worktree/git and network grants, and clean them up after
  cancellation.
- Run #647's authenticated Copilot parity probe inside each candidate. A bare-Windows P10
  success is necessary but not sufficient; failure under a candidate marks that rung
  unavailable rather than authorizing fallback.
- Produce a decision update before any production backend. This spike is gated on both
  S0's seam and the final #647 findings.

### P11-I2 — Containerized Windows worker isolation

- Define W3 only with the tier-3 worker/runtime work: compatible Windows base images,
  minimal workspace mounts, a least-privilege `ContainerUser`, authentication forwarding,
  egress policy, and container teardown composed with Job Object lifecycle.
- Select and record process versus Hyper-V isolation from the worker threat model.
  Process isolation must retain the documented shared-kernel/host-escape residual risk;
  hostile or multi-tenant stages require Hyper-V isolation.
- Gate implementation on the S0 contract still applying at the worker seam, the final
  #647 harness verdict, and an approved tier-3 Windows worker issue. It is not a
  prerequisite for trusted-local Windows deterministic support.
