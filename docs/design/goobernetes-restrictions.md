# Goobernetes restrictions — the effect-based isolation model

Status: approved — Goobernetes v1 design. Encodes the PO decision record in
goobernetes-decisions.md (2026-08-22).

This document defines the v1 restrictions model for all three execution modes: what a
restriction *is* (an effect, never a mechanism), who may introduce one (runner, stage,
instance), where it is actually enforceable in v1 (Linux pods; honestly, nowhere else in
full), and what happens when it cannot be enforced. It encodes decision-record **D7**
(with hooks into D2, D3, D4, and D12) and supersedes the surfaces named in §8.

**What this is not.** It is not the BYO test-sandbox contract (#675 — environments a
stage's *tests* point at). It is not credential scoping — `capabilities:` grants are
untouched (decision record D2). It is not the external call-out egress gateway, which is
its own contract (external-call-out-stages.md D22) and composes with, rather than rides
on, the restrictions here.

---

## 1. Decisions

| # | Decision | Why |
| --- | --- | --- |
| D1 | **Closed, effect-based list, five entries in v1**: `network:none`, `network:allowlist`, `fs:readonly-except-workspace`, `tmp:ephemeral`, `env:default-deny`. Restrictions name *effects*; seccomp, bubblewrap, Seatbelt, NetworkPolicy, Squid, LPAC appear only as bindings | Mechanisms differ per (OS, host-kind) and are revisitable (decision record D0); the effect is the contract the DSL, the journal, and the portal can talk about. A closed list is validatable; an open one is a stringly-typed wish |
| D2 | **One model, three inputs**: a restriction is a runner *property* (`runners[].restrictions`), a stage *requirement* (`runsOn.restrictions`), and an instance *mandate* (`isolation.mandates`). All three name entries from the same closed list | Today's analogues are scattered across four surfaces with different owners (§8). Three inputs into one solver replaces a competing-config-surface collision (the #2301/#2302 shape) with one resolution rule |
| D3 | **Strengthen-only, SEC-021 preserved**: mandates and requirements may only *add* restrictions relative to what the resolved set would otherwise be; nothing a gaggle or stage writes can remove a mandate. An unsatisfiable intersection is an **apply-time error** — no schedule, no runtime surprise | `EffectiveAgenticSandbox` already encodes this trust hierarchy (internal/instance/sandbox.go:43-54): instance.yaml is the operator trust root; less-privileged writers strengthen, never weaken |
| D4 | **Linux-pod-only enforcement in v1.** Only a `host: image`/`deployment` Linux runner may *declare* `network:none`, `network:allowlist`, `fs:readonly-except-workspace`, or `tmp:ephemeral`. `env:default-deny` is declarable everywhere (it is already enforced everywhere, §2.5). Windows runners declare no restrictions in v1; local `self` runners declare only what a runtime probe proves (§3) | Every mechanism in the tree is per-OS with different fidelity; outside Linux pods the honest matrix is mostly empty (§3). A restriction a runner cannot enforce must be undeclarable, or apply-time validation produces confident PASSes on unenforced substrate — the `checkNetworkPolicySupport` lesson (internal/k8spreflight/checks.go:62-88) |
| D5 | `network:allowlist` is **CIDR-NetworkPolicy-backed**, per the standing #2898 PO ruling (2026-08-16). The proxy graduates later as the FQDN/audit layer under #1307. This design does **not** reopen that ruling | The #3278 live deployment's proxy-only-route-out shape (#3301) is *evidence the proxy works*, not the product contract. CIDR NetworkPolicy is portable across CNIs; FQDN/proxy is an audit-granularity upgrade, explicitly deferred by the same ruling |
| D6 | **Per-restriction failure mode, from the three existing idioms** (§4): (a) apply-unsatisfiable, (b) fail-closed at schedule, (c) journaled de-isolation marker. Every (restriction × cell) in §3 resolves to exactly one. **Mode 3 never uses (c)**: a distributed run is never silently de-isolated | All three idioms already exist and are proven: apply-time constraint solve (decision record D4), `SandboxEnforced` fail-closed (internal/harness/executor.go:387), and the #2034 Windows marker (internal/executor/network_windows.go:12,22). A list without named failure modes is meaningless — the runtime must know whether to refuse or to confess |
| D7 | **Pod-level restrictions are applied by the pod creator** — the dispatcher stamps `securityContext` (readOnlyRootFilesystem, RuntimeDefault seccomp, runAsNonRoot, no privilege escalation) and the workspace/tmp `emptyDir` mounts on every stage pod it creates. **Network-level restrictions ship as rendered per-runner-class reference manifests** the adopter applies, verified by `doctor --k8s` negative controls, with #3301's rendered-together CI assertion in deploy-validate | The operator deliberately holds no `networking.k8s.io` RBAC (#2898, citing internal/operator/gaggle_controller.go:63-67,123-134) and this design does not grant it: the dispatcher owns what lives *inside* the pod spec; the cluster operator owns the network fabric, with the product supplying rendered truth and verifying it instead of applying it. #3301 proved the failure class is *composition* — hence render-together, assert-cross-base |
| D8 | **In-pod process mechanisms are not the mode-3 enforcement layer.** Stage pods run under PSS `restricted` with RuntimeDefault seccomp, which denies the unprivileged userns clone that backs local `network:none` (CLONE_NEWUSER\|CLONE_NEWNET, internal/executor/network_linux.go:25) — the tree's own test support exists to detect exactly this denial (test/testsupport/testdep/userns.go:36-77). In mode 3, `network:none` and `network:allowlist` are pod-boundary effects (NetworkPolicy class), never in-process namespaces | Layering the local mechanism inside a hardened pod fails at runtime on conformant clusters. The pod *is* the isolation boundary in mode 3; re-isolating inside it is both unnecessary and impossible under restricted PSS |
| D9 | **The per-gaggle sandbox preview folds in** (decision record D2/D7): `featureGaggleSandbox` (#1305) becomes 3.0 sugar for the mandate/requirement `fs:readonly-except-workspace` on agentic stages; the preview feature is frozen in 2.0 and does not exist in 3.0. `EffectiveAgenticSandbox` becomes the local *binding* of that restriction, not a parallel model | One isolation surface, not two fighting ones. The most-restrictive-wins rule it implements is exactly D3's rule, so the fold is semantic-preserving; the journaled posture event (`runner.isolation.posture`, internal/journal/event.go:64-70) generalizes to the full restriction set |
| D10 | **`DeterministicRun.Network: none` folds in and migrates**: `goobers fix --to 3.0` rewrites it to `runsOn.restrictions: [network:none]` (dsl-3.0.md D16, migration rule 4); the 2.0 field stays valid only in the frozen 2.0 interpreter. Local binding stays the shipped process mechanism (Linux userns, macOS `(deny network*)` Seatbelt); mode-3 binding is placement onto a `network:none` runner class | One spelling per effect in 3.0 — two spellings for one effect is exactly the drift shape the #659 supersession closed. What changes semantically is that the requirement now *resolves through the same solver*, so a `network:none` stage on an inventory with no capable runner is an apply-time error, not a runtime surprise |
| D11 | **Windows sandboxing is its own epic** — nice-to-have for local, eventual (not distant) must-have for cloud. Candidate contents: the #651 isolation-posture decision (now narrowed to *intra-container* posture, since decision record D1 resolves Windows to container pods); retiring the #2034 `GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE` escape hatch; LPAC / job-objects / Windows-container network-policy research (LPAC has zero implementation in the tree today) | Windows can currently enforce no network or filesystem restriction natively: `sandbox.New` returns `ErrUnsupported` (internal/sandbox/native_other.go), `network:none` fails closed unless de-isolated with a journaled marker, and k8s rejects the Linux `securityContext` hardening on Windows pods. Pretending otherwise in v1 would make the closed list a lie on half the smoke cluster |
| D12 | **Enforcement honesty is part of the contract**: `network:*` restrictions on a runner class are verified by an in-cluster negative-control probe (a probe pod that must *fail* to reach a canary endpoint, with denial distinguished from DNS failure), surfaced through `doctor --k8s`. Absent a probe result, doctor reports the restriction **UNVERIFIED**, never PASS | #2898's acceptance criteria verbatim. `checkNetworkPolicySupport` is API-discovery only and passes on clusters with zero dataplane enforcement — the existing PASS-on-nothing bug this design must not reproduce |

---

## 2. The closed list

Each entry: the effect, the v1 binding, and the failure mode(s) it uses (idioms defined
in §4).

### 2.1 `network:none`

**Effect.** The stage process (and descendants) can open no outbound connection at all —
DNS included. The base runner contract's provider/credential traffic is the runtime's,
not the stage's; a `network:none` stage therefore cannot be an agentic stage that needs a
model endpoint (apply-time error via requirement derivation, decision record D2).

**Bindings.** Mode 3: placement onto a runner class whose namespace carries the
deny-all-egress NetworkPolicy (the shipped `default-deny-all` shape,
deploy/reference gaggle-namespace base) with *no* allow rule selecting that class's
pods beyond journal/artifact mounts. Modes 1/2 (deterministic stages only, as today):
Linux one-ID userns (internal/executor/network_linux.go:25), macOS Seatbelt
`(deny network*)`. Windows local: the #2034 marker path, mode 1 only, until D11's epic.

**Failure modes.** Unsatisfiable requirement → idiom (a). Runner claims it but the class
probe fails → idiom (b). Windows mode-1 escape hatch → idiom (c), the only (c) cell left.

### 2.2 `network:allowlist`

**Effect.** Egress only to a named destination set: git/backlog provider, model endpoint,
the gaggle's declared sandbox targets — the k8s-infra-shape §5 posture, now declarable
and solvable.

**Bindings.** Mode 3 only in v1: CIDR NetworkPolicy per the #2898 ruling (D5), rendered
per runner class (D7). The allowlist CIDR set is instance-operator-supplied
configuration rendered into the manifests — the CHANGE-ME documentation CIDRs in
deploy/reference are placeholders the render step refuses to ship unfilled. Not
enforceable on any local host in v1: bubblewrap never passes `--unshare-net`
(internal/sandbox/native_linux.go:53-69) and Seatbelt's agentic profile allows network
by design — so a `self` runner cannot declare it (D4).

**Failure modes.** Idioms (a) and (b). Never (c).

### 2.3 `fs:readonly-except-workspace`

**Effect.** All filesystem writes outside the stage workspace and its declared writable
roots are refused.

**Bindings.** Mode 3: `readOnlyRootFilesystem: true` + workspace/tmp `emptyDir` mounts,
stamped by the dispatcher (D7) — the same block deploy/reference already CI-asserts on
its own Deployments (cmd/goobers/deploy_reference_test.go:177-179). Modes 1/2: the
shipped internal/sandbox layer — bubblewrap `--ro-bind / /` + workspace binds on Linux,
Seatbelt `(deny file-write*)` + workspace allow on macOS (Policy carries exactly
Workspace and WritableRoots, internal/sandbox/sandbox.go). Windows: none until D11.

**Failure modes.** Idioms (a) and (b) — (b) is literally today's `SandboxEnforced`
contract, kept verbatim.

### 2.4 `tmp:ephemeral`

**Effect.** Temp space is stage-attempt-private and destroyed with the attempt; nothing
written to it survives to, or is visible from, any other attempt or stage.

**Bindings.** Mode 3: structurally free — fresh never-reused pods (decision record D1)
with `emptyDir` tmp; declaring it on a pod runner is asserting the substrate, and the
dispatcher enforces it by construction. Modes 1/2: per-stage TMPDIR under the stage
workspace (procenv already pins TMPDIR into the allowlisted env); deletion rides stage
cleanup. It is v1-declarable on `self` runners because the binding is pure daemon-side
behavior, no OS mechanism needed.

**Failure modes.** Idiom (a) only (a runner kind that cannot provide it simply cannot
declare it; there is no runtime discovery step to fail).

### 2.5 `env:default-deny`

**Effect.** The stage environment is the explicit allowlist and injected `GOOBERS_*`
contract vars — no ambient daemon/pod environment leaks in.

**Bindings.** Already enforced on every mode by internal/procenv (#736): default-deny
with `runner.envPassthrough` as the explicit opt-in hatch
(internal/procenv/procenv.go:45,97,112). Listing it makes it *mandatable*: an
`env:default-deny` **mandate** additionally refuses `envPassthrough` entries on covered
runners at apply time — the hatch closes when the operator says so.

**Failure modes.** Idiom (a) (mandate vs. `envPassthrough` conflict at apply). The base
enforcement itself has no failure mode; it is unconditional code.

---

## 3. Enforceability matrix (v1, honest)

"Enforced" = the binding exists in-tree or in this design's v1 scope and is verified
(probe or CI assertion). "Declarable" = a runner of that cell may carry the restriction.
Cells not marked declarable make the restriction **undeclarable** there (D4) — a stage
requiring it simply cannot match such a runner, and apply says so.

| Restriction | Linux pod (mode 3) | Windows pod (mode 3) | Linux/macOS `self` (modes 1/2) | Windows `self` (mode 1) |
| --- | --- | --- | --- | --- |
| `network:none` | **Enforced** — deny-all NetworkPolicy class, probe-verified (D12). In-pod userns is unavailable under restricted PSS (D8) and is not used | Not declarable (D11 epic) | **Enforced**, deterministic stages — userns / Seatbelt, `ProbeNoNetwork` preflight (internal/executor/network.go) | Marker-only: #2034 de-isolation with journaled `unsupported-windows` marker; retired by D11 |
| `network:allowlist` | **Enforced** — CIDR NetworkPolicy per class (D5), probe-verified | Not declarable | Not declarable — no local mechanism (bwrap keeps host network; Seatbelt agentic profile allows network) | Not declarable |
| `fs:readonly-except-workspace` | **Enforced** — dispatcher-stamped `securityContext` + mounts | Not declarable — k8s rejects the Linux securityContext fields on Windows pods | **Enforced**, agentic stages — internal/sandbox (bwrap/Seatbelt), smoke-run preflight (internal/sandbox/native_linux.go:19-42) | Not declarable — `sandbox.New` is `ErrUnsupported` |
| `tmp:ephemeral` | **Enforced by construction** — fresh pod + emptyDir | Enforced by construction once Windows pods exist, but not independently *declarable* until D11 defines its verification | Declarable — daemon-side TMPDIR scoping | Declarable — same daemon-side binding |
| `env:default-deny` | **Enforced** (procenv, unconditional) | Enforced | Enforced | Enforced |

Reading the matrix: **v1 full enforcement is Linux pods only** (decision record D7). The
Linux/macOS `self` column is real but partial (per-idiom, probe-gated, and split across
deterministic/agentic stage classes); the Windows columns are nearly empty and say so.
macOS never appears as a pod column: there is no macOS cloud substrate (decision record
D1) and the k8s reference knows only Linux and Windows pools.

---

## 4. Failure-mode idioms

The three idioms, all shipped today, each with a named owner in this model:

- **(a) Apply-unsatisfiable.** The constraint solve (decision record D4, checkpoint 1)
  intersects stage requirements, runner properties, and instance mandates. An empty
  intersection — stage requires a restriction no runner enforces; stage's derived needs
  (e.g. model-endpoint egress for an agentic stage) conflict with a mandate;
  `os: windows` + any non-`env`/`tmp` restriction — is an **error** when a `runners:`
  inventory is declared, warning otherwise (the #3497 severity fix). Nothing schedules.
- **(b) Fail-closed at schedule.** Apply-time truth rots. A runner class that declared a
  restriction but whose mechanism is absent at dispatch (probe stale, policy deleted,
  bubblewrap missing) refuses the stage with a bounded, named diagnostic — the
  `SandboxEnforced` contract (internal/harness/executor.go:387) generalized: *a runner
  that cannot establish a mandated restriction fails the run, it never degrades*. This is
  also the honest landing for the trusted-claims model (decision record D3/RRQ-1): a
  false restriction claim degrades to exactly this named runtime error.
- **(c) Journaled de-isolation marker.** Run anyway, confess loudly: the #2034 pattern —
  host-global opt-in env var, `unsupported-windows` marker stamped into the stage's
  `ResultEnvelope` outputs, visible in journal and portal. In v1 exactly **one** cell
  uses it (Windows `self`, `network:none`, mode 1), it requires the explicit opt-in, and
  D11's epic retires it. Mode 3 never de-isolates: a distributed attempt either runs
  restricted or does not run.

Restriction provenance in mode 3 — which restrictions were in force for an attempt, and
via which binding — is recorded as journal events in the `runner.*` namespace
(non-conformance-normative, decision record D5), generalizing the shipped
`runner.isolation.posture` event (#1305, internal/journal/event.go:64-70).

---

## 5. Ownership: one model, three inputs

```yaml
# instance.yaml
isolation:
  mandates:
    - match: { stageClass: agentic }        # operator mandate: strengthen-only
      restrictions: [network:allowlist]
runners:
  - name: tiny-linux
    host: ghcr.io/…/goobers-base:v0.3.0
    provides: { os: linux, cpu: 2000m, memory: 4Gi }
    restrictions: [network:allowlist, fs:readonly-except-workspace, tmp:ephemeral]

# gaggle DSL 3.0
tasks:
  risky-refactor:
    runsOn:
      os: linux
      restrictions: [fs:readonly-except-workspace]   # stage requirement
```

- **Runner property** — what this runner *is*. A stage placed here runs under these
  restrictions whether or not it asked; the property is enforced, not advisory.
- **Stage requirement** — `runsOn.restrictions` constrains placement to runners that
  enforce the named effects. Requiring, like every `runsOn` field, is
  explicit-complete: unspecified = no requirement (decision record D2).
- **Instance mandate** — the operator's floor. The decision record's shared-instance
  example is the canonical case: *an instance owner in a shared instance imposes
  "agentic stages get an egress allowlist" on gaggle authors who never asked.* The
  mandate rewrites nothing in the gaggle; it narrows the set of runners those stages may
  match to ones carrying `network:allowlist`.

Resolution: effective set = union(mandates matching the stage, stage requirements);
eligible runners = those whose properties ⊇ the effective set *and* whose properties
don't conflict with the stage's derived needs. Strengthen-only (D3): no input can
subtract. Conflict ⇒ unsatisfiable ⇒ idiom (a). A stage needing broad egress on an
instance whose only runners are `network:allowlist` is an apply-time error naming the
stage, the restriction, and the mandate that imposed it — not a hung schedule and not a
runtime mystery.

---

## 6. Mechanism alignment: NetworkPolicy now, proxy later

- The **#2898 PO ruling (2026-08-16) stands**: portable CIDR NetworkPolicy, opt-in per
  declared restriction — not default-deny-everything, not proxy-based. `network:allowlist`
  is *defined* as CIDR-backed in v1 (D5). Goobernetes is the missing "tier-3 stage-pod
  deploy path" #2898 was parked on; that issue unblocks against this design.
- The **proxy is the later audit/FQDN layer** under #1307 (trust-boundary-hardening
  TBH-3): per-goober FQDN allowlists and a journaled network audit trail. When it
  arrives it becomes a stronger *binding* for the same `network:allowlist` effect — the
  DSL and inventory surfaces do not change, which is the payoff of D1's
  effects-not-mechanisms rule. The #3278 six-policy proxy shape (443-only egress,
  RFC1918 and 169.254.0.0/16 excepted so it can never pivot to IMDS) is recorded as the
  reference implementation candidate for that layer.
- **Composition is additive, so grants are per-class ONLY** (delivery decision 004 —
  a measured finding, not a preference): Kubernetes NetworkPolicy has no deny rule and no
  precedence — effective egress is the union of every policy selecting the pod — so a
  generic role-wide egress allow makes every narrower per-class policy a no-op. The
  namespace baseline is exactly `default-deny-all` + `allow-dns` ("no inbound to stage
  pods, ever" stands); **every** egress grant selects exactly one
  `goobers.dev/runner-class` label, and the reference base's former generic
  `allow-stage-egress` policy is removed. SEC-021 strengthen-only is implemented by
  NARROWING renders — a stricter mandate renders smaller per-class grant sets — never by
  adding policies. The dispatcher stamps exactly one `goobers.dev/runner-class` label on
  every stage pod (zero or two breaks the model; binding on #3513).

## 7. Who applies what

| Layer | Applied by | Verified by |
| --- | --- | --- |
| Pod `securityContext` + workspace/tmp mounts (`fs:*`, `tmp:*`) | **Dispatcher**, at pod creation — the pod creator owns the pod spec | Unit-level spec assertions (the deploy_reference_test pattern) + PSS `restricted` admission on the namespace |
| NetworkPolicies per runner class (`network:*`) | **Cluster operator applies rendered reference manifests** — the product renders per-runner-class manifests from the `runners:` inventory (allowlist CIDRs filled from instance config; render refuses CHANGE-ME placeholders) | `doctor --k8s` negative-control probe pods (D12); deploy-validate's rendered-together cross-base assertion (#3301) — every `goobers.dev/role` label rendered anywhere must be matched by a policy rendered somewhere, both bases composed |
| Stage env (`env:default-deny`) | **Runtime**, unconditionally (procenv) | Existing procenv tests; apply-time mandate-vs-passthrough check |
| Local process sandbox (modes 1/2 bindings) | **Runner**, at stage launch (internal/sandbox, network_linux) | Mechanism preflights: bubblewrap smoke-run, `ProbeNoNetwork` |

The Goobers operator's RBAC does not grow a `networking.k8s.io` grant in v1 (D7). The
runtime never applies cluster networking; it renders, and it verifies. Nothing in this
table is reachable by agents: `runners:`, mandates, and rendered manifests are
operator-owned surface (CT-1, external-call-out-stages §3).

## 8. Superseded and folded surfaces

- **`featureGaggleSandbox` (#1305) — folded** (D9). 2.0: stays frozen preview,
  unchanged. 3.0: the migrator maps `gaggle.spec.sandbox` to the equivalent
  `fs:readonly-except-workspace` mandate/requirement; the feature flag does not exist in
  the 3.0 interpreter.
- **`DeterministicRun.Network` — folded and migrated** (D10): valid in 2.0 only; the
  migrator rewrites it to `runsOn.restrictions: [network:none]` in 3.0.
- **`internal/instance/sandbox.go` posture model — becomes a binding**, not a parallel
  surface; its SEC-021 most-restrictive-wins rule is promoted to the whole model (D3).
- **`runner.envPassthrough` — subordinated** to the `env:default-deny` mandate (§2.5).
- **mixed-platform-cloud-nodes.md §3 Windows persistent-VM posture — superseded** by
  decision record D1 (Windows container pods); #651 narrows to intra-container posture
  inside D11's epic.
- **Not superseded**: the #2898 CIDR ruling (D5 encodes it); #1307 (deferred, owns the
  proxy layer); #675 (different axis); k8s-infra-shape §5 (baseline this composes with).

## 9. Windows sandboxing epic (new sub-epic per decision record D12)

Charter: make at least `fs:readonly-except-workspace` and one `network:*` restriction
declarable on Windows runners. Nice-to-have for local Windows, **eventual — not distant —
must-have for cloud**, because a smoke cluster with un-restrictable Windows pods (D4)
is an accepted v1 asymmetry, not an acceptable steady state. Candidate contents:

1. **#651** — the isolation-posture decision, re-scoped from "VM vs container" (settled:
   container pods) to *intra-container* posture on Windows Server 2022 images.
2. **Retire the #2034 escape hatch**: delete `GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE`
   once a real Windows `network:none` binding exists; until then the marker idiom stays
   mode-1-only.
3. **Research spikes**: LPAC (zero implementation today), job objects, and NetworkPolicy
   enforcement fidelity on Windows nodes with the reference CNI — each producing an
   enforceability-matrix cell update with a probe, per D12's honesty rule.

## 10. Acceptance criteria (falsifiable)

1. Apply with a declared `runners:` inventory where a stage requires `network:allowlist`
   and no runner enforces it → **error** naming stage, restriction, and (if mandate-
   induced) the mandate. Same document with no inventory → warning.
2. A stage requiring any restriction other than `tmp:ephemeral`/`env:default-deny`
   together with `runsOn.os: windows` → apply-time error in v1.
3. In the D11 smoke cluster: a probe pod in a `network:none` runner-class namespace
   fails to reach an in-cluster canary endpoint, and the diagnostic distinguishes
   policy denial from DNS failure (#2898 acceptance).
4. `doctor --k8s` on a cluster with the NetworkPolicy API but no enforcing dataplane
   reports the `network:*` restrictions **UNVERIFIED** — never PASS.
5. Every stage pod the dispatcher creates carries `readOnlyRootFilesystem: true`,
   RuntimeDefault seccomp, `runAsNonRoot`, and emptyDir workspace/tmp mounts when its
   runner declares the corresponding restrictions; asserted by unit test on the rendered
   pod spec.
6. deploy-validate renders goobers-system and gaggle-namespace bases **together** and
   fails if any rendered `goobers.dev/role` label is matched by no rendered policy
   (#3301's class fix).
7. A runner-class probe failure at dispatch produces a bounded, named runtime failure
   (idiom b) — the run journal shows the diagnostic; the attempt is classified infra,
   not stage failure.
8. A mode-1 Windows `network:none` de-isolated run stamps the `unsupported-windows`
   marker into the stage envelope and it is visible in journal and portal; the same
   opt-in env var has no effect in mode 3.
9. An `envPassthrough` entry on a runner covered by an `env:default-deny` mandate →
   apply-time error.
10. A 2.0 gaggle using the sandbox preview feature behaves exactly as before;
    `goobers fix --to 3.0` migrates it to the D9 mapping with a semantically equivalent
    resolved restriction set (asserted by resolving both and comparing).
11. Restriction provenance appears as `runner.*` journal events on mode-3 attempts, and
    conformance verification is unaffected (non-normative namespace).

## 11. Issue cross-references

#2898 (CIDR ruling encoded; unblocked by this design as its deploy path), #1307
(deferred proxy/audit layer), #3301 (rendered-together assertion adopted, AC-6), #1305
(sandbox preview folded), #2034 (marker idiom scoped and slated for retirement), #651
(re-scoped into the Windows epic), #647 (Windows harness spike — closed 2026-08-11,
harness confirmed working on Windows; cited as evidence, not a gate), #3497 (severity fix, AC-1), #736
(procenv baseline), #675 (out of scope), #2889 (umbrella epic).

## 12. Open implementation points

Deliberately open — none reopens a decided question:

- **CIDR set stewardship**: who maintains provider/model-endpoint CIDR sets in instance
  config, and the re-render/re-apply cadence when providers shift ranges (the CHANGE-ME
  problem transfers to every runner namespace).
- **Render granularity**: one manifest set per distinct restriction-set (runner class)
  vs. per named runner — affects namespace/label layout and probe count.
- **Probe cadence and ownership**: doctor-invoked probe pods on demand vs. a periodic
  dispatcher-side re-verification feeding idiom (b); how stale a probe result may be
  before dispatch refuses.
- **`self`-runner declaration ceremony**: whether local runners auto-derive their
  declarable set from mechanism preflights at startup or the operator writes it and
  preflight verifies (declare-then-verify vs. probe-then-advertise).
- **Mandate matching vocabulary**: v1 ships `stageClass: agentic|deterministic`; whether
  match also needs gaggle/goober selectors, and what a mandate means on an inventory
  (e.g. macOS-only local) where no runner can ever satisfy it — permanent apply error
  vs. inventory-aware warning.
- **`tmp:ephemeral` vs. pinned-workspace mode** (ARCHITECTURE §5): the local
  pinned-workspace lease is node-local persistent state; define whether it is simply
  incompatible with the restriction or scoped out of "tmp".
- **`env:default-deny` mandate strictness**: refuse all `envPassthrough` on covered
  runners, or permit an operator-side intersection allowlist.
- **Windows epic sequencing**: LPAC vs. job objects vs. Windows NetworkPolicy fidelity —
  which spike runs first. (The harness itself is settled — #647 closed with the harness
  confirmed working on Windows.)
