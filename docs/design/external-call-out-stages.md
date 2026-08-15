# External call-out stages

**Status:** design of record, pending the owner rulings in §9.
**Verified against** `origin/main` @ `e447359f`.

A workflow declares a stage that hands work to an HTTP endpoint outside the instance and brings
information back. Targets are operator-configured and named from the DSL; credentials are scoped to one
target; the response becomes a graded artifact plus a declared scalar projection; the call is correlated
into the run's existing trace.

**What this is not.** It is not the test-sandbox contract (#670 — the environment a stage's tests
*point at*). It is not #1087, whose two forms are shipping source to a remote host over SSH/WinRM and
routing a stage to a capability-matched worker — both about where a stage's *own compute* runs, and both
outside this design (D11 rules SSH out entirely). The deferral in
[large-repo-execution-model.md](large-repo-execution-model.md) §8 is #1087's and stays #1087's. What this
generalises is `external-telemetry`, the shipped kind that already calls out under host governance.

**Relationship to #737 / #744.** #737 ("dynamic stage-kind registration seam") and its epic #744 own the
generic pluggable-kind seam. That seam has since landed as `executor.KindRegistry`; `external-call`
registers on it as a fourth kind rather than re-inventing it, and this design does not supersede #744's
broader custom-stage scope.

---

## 1. Decisions

| # | Decision | Why |
| --- | --- | --- |
| D1 | Typed `run.kind`, landing TBH-1 ([stage-contract.md](../stage-contract.md) §"runner-owned deterministic run kinds"), with `external-call` as its first value | `inputs.kind` is runtime-overridable: `internal/runner/run.go:3071-3088` overlays `inputsFrom` onto `env.Inputs` (writes at `:3074`, `:3087`) and `internal/executor/dispatch.go:108` selects the executor from the overlaid map, while admission reads only the static literal (`internal/workflow/v_next/compile.go:455,462`). A stage can read as `shell` to a reviewer and dispatch a call-out |
| D2 | `inputsFrom` binding `kind` becomes a v_next **error**; a warning on frozen v_current | With a call-out upstream, the *endpoint* chooses a downstream stage's executor. No honest use remains once `run.kind` is typed |
| D3 | **Every artefact is Task-scoped** | `FeaturesForGaggle`, `FeaturesForGoober` and `CheckFeatureSupport` call `vcurrent.*` unconditionally (`internal/workflow/features.go:97,102,107`), so a v_next-only feature at goober or gaggle scope is invisible to introspection. Task scope avoids the asymmetry rather than fixing it; routing those functions through `interpreterForDefinition` is a prerequisite for any later goober-scoped surface (filed, §10) |
| D4 | DSL **2.0 preview feature**, not a 2.1 bump | A minor costs a copied interpreter (v_current 8,459 lines / v_next 9,648), a hand-edited arm, a migration edge, and a 3-release support commitment validated at package init (`internal/supportmatrix/supportpolicy.go:11-16`) |
| D5 | `external-call` is **refused on the frozen interpreter** — both the kind and the scoped grant | A shared vocabulary is not a shared feature. None of the guards below exist in v_current. Contract-preserving: it refuses a document no 1.4 author could have written |
| D6 | **v1 may mutate the far side** | Owner ruling. Read-only was contradictory with submit-then-poll (a submit *is* state creation) and enforced by nothing — no safe-method constraint, and one bearer token per target authorises writes exactly as reads |
| D7 | A deterministic **call epoch** = runID + branch + stage + entry, minted inside `runTask` (`internal/runner/run.go:2531`) from a run-scoped ledger threaded as `stepBudget` already is | Both tier-1 dispatch arms funnel through `runTask` exactly once per intended dispatch. Nothing minted in an activity survives a Temporal re-dispatch — the engine has no `workflow.SideEffect`. Constant across attempts, advancing on a repass |
| D8 | The **receiving-side contract** is operator-declared and enforced at config load (§3) | A dedup contract nobody validates is a wish. Config load refuses every shape that would configure a duplicate-effect generator |
| D9 | The reconcile decision **splits across the seam**: `attemptFailureClass` returns `reconcileNext`, the dispatch closure acts on it | `attemptFailureClass` (`internal/engine/retry.go:161`) runs inside the workflow function with only the task and error available; `effect` lives in `instance.yaml`, which the in-workflow re-compile (`internal/engine/engine.go:208`) must not read |
| D10 | An explicit **ambiguous outcome** (`callout_outcome_unknown`) | `ResultStatus` is success/failure/blocked/no-work (`api/v1alpha1/envelope.go:182-206`). Today an unknown must masquerade as failure, which a gate acts on |
| D11 | The kind owns its error: `Retryable` constant `false`, `Code` from a closed `callout_*` vocabulary | `AutomatedInputs` fills the error triple from `subject.Error` (`internal/gate/automated.go:97-122`), and `failure-class` forks on it. The landed #2925 fix closes the *Outputs* door; it does not touch this one |
| D12 | **Sync + submit-then-poll**, modelled on `ci-poll`. No async callback | No landing zone: `internal/instance/config.go:2251` refuses a non-loopback bind with "there is no insecure override"; [k8s-infra-shape.md](k8s-infra-shape.md) §5 "No inbound to stage pods. Ever."; `ResultBlocked` is terminal, not resumable; the engine refuses human gates outright |
| D13 | **HTTP only.** SSH is a separate design | No `crypto/ssh` import, no `HostKeyCallback`, no `known_hosts` anywhere in the tree — and `test/nophonehome` monitors only net/http/smtp/grpc/OTLP, so an `ssh.Dial` target is invisible to the merge gate (verified by execution) |
| D14 | **No new journal event types.** `stage.*` + `artifact.recorded` + `ref.touched` | `IsConformanceNormative` ends `default: return true` (`internal/journal/event.go:429-431`), so a new type must be emitted byte-identically by both runners — impossible against a live endpoint. `projectableEventTypes` (`internal/engine/projection.go:28-39`) is a closed ten-entry whitelist validated over *all* ops before `journal.Create`, so an unknown type yields **no journal at all**. `artifact.recorded` is exempt: the projection's artifact op writes it through the journal writer rather than as an op |
| D15 | `ResultEnvelope` gains **`ExternalRefs`**, giving `ref.touched` a non-lossy producer | The only stage-level producer today is the `mutations.jsonl` sidecar, appended best-effort. Best-effort is defensible for a fact the runner also observed through an exit code; it is indefensible as the *only* record of a call that left the instance |
| D16 | Callee identity is structured `(provider, kind, id)`, never a URL | `ExternalRef.URL` is dropped from the conformance projection (`internal/journal/conformance.go:68-72`) |
| D17 | The response is a graded artifact plus a **declared, type-checked scalar projection**. Never a wholesale `Outputs` merge | Mirrors `external-telemetry`, which emits three named scalars rather than the body |
| D18 | A **produced-integrity floor**, as a runner change | `internal/runner/run.go:2754-2755` unconditionally overwrites the executor's grade, and `internal/runner/inputsfrom.go:352-354` returns `IntegrityTrusted` for a stage with no graded input — the common call-out shape. Without the floor the far side's scalars reach downstream stages labelled trusted |
| D19 | Targets live in `instance.yaml` referenced by name; **`auth.mode: none` is deleted**; three config-load rules make one-credential-per-target true (§3) | Every target carries a credential, so authority is a refusal rather than a convention |
| D20 | Target-scoped grants `callout:invoke@<target>` | Follows `credentials.RepoScopedCapability` (`internal/credentials/scoping.go:69-78`), already opaque to the injector. A flat capability would make operator curation all-or-nothing |
| D21 | The kind **bounds its own call volume** per target (`maxCallsPerHour`, `maxCallsPerRun`) | `policy.maxAttempts` × the hard-coded infra budget of 2 × repasses × poll ticks multiply without a ceiling. `ProviderQuotaState` cannot be reused: an arbitrary target advertises no quota |
| D22 | Egress is **one contract, two bindings**: in-process on tier 1, a deployed gateway on tier 3 | Same declaration, swappable substrate. Converges with #1307 and #2898 (§4) |
| D23 | Tracing: **correlate now, stitch later** | The run ID *is* the 32-hex trace ID (`internal/telemetry/id.go:15-26`), so correlation is free. `traceparent` from a `SpanKindClient` span needs one small change (`telemetry.Span` has no `SpanContext` accessor). Stitching stays deferred because no OTLP receiver exists and foreign parents are rejected — **not** because tier 3 lacks spans, which stopped being true when #2865 landed |
| D24 | A far-side `Retry-After` is **clamped** by the kind | Nothing clamps it today: `internal/runner/run.go:2799-2808` returns it verbatim and tier 3 turns it into a durable `workflow.Sleep` (`internal/engine/retry.go:117`). A hostile `503` + far-future `Retry-After` parks the stage past its own timeout |

---

## 2. The DSL surface

```yaml
- name: ask-the-oracle
  run:
    kind: external-call          # typed; inputsFrom cannot rewrite it
    workspace: scratch
    timeoutSeconds: 120
    externalCall:
      target: pricing-oracle     # names an instance.yaml target
      operation: quote
      responses:
        - field: verdict
          output: riskVerdict
          type: string
  capabilities: [callout:invoke@pricing-oracle]
```

**Eight rules hold at every compile root**, decidable from the document plus build-time constants — no
injected option and no nil guard, so they cannot be silently absent where a root forgot to wire them.
This matters because `internal/engine/registry.go` compiles with only `WithPreviewFeatures`, so any
option-gated rule is simply absent on the tier-3 registration path.

| | Rule |
| --- | --- |
| XC-A1 | `externalCall` present **iff** `run.kind == external-call`, and the kind is a member of the compiled-in vocabulary |
| XC-A2 | The kind satisfies its **minimum DSL version** — so `external-call` is refused on the frozen interpreter (D5) |
| XC-A3 | `externalCall.target` matches `^[a-z][a-z0-9-]{0,62}$` — **shape only; existence is not checked here** |
| XC-A4 | `responses` is non-empty; each `type` is `string\|number\|boolean`; no duplicate `field`, no duplicate `output` |
| XC-A5 | **Grant biconditional.** A task declares a `callout:invoke@…` grant **iff** its kind is `external-call`; exactly one, and its suffix equals `externalCall.target` |
| XC-A6 | No `responses[].output` names a reserved automated-gate key |
| XC-A7 | `run.workspace` is `scratch`, `run.network` is unset, `timeoutSeconds` is non-zero |
| XC-A8 | An agentic stage whose `contextFrom` selects a call-out stage in the same document declares `minimumIntegrity` |

Target and operation **existence** is deliberately not a compile rule — the compiler has no
`*instance.Config`. A tier-1 daemon cross-reference pass checks it at load.

`run.network: none` is refused on a call-out: an in-process kind sits outside `procenv`, the sandbox and
the network mode, all of which shape subprocesses only. Silently meaningless isolation is worse than none.

---

## 3. Targets, credentials, and the trust boundary

A target declares scheme, host, port and path prefix, its credential ref, per-operation `method` and
`path`, `effect`, the call budget (D21), and response ceilings. The DSL names it and nothing else.

**The receiving-side contract.** A target declaring `effect: mutating` must honour the epoch as an
`Idempotency-Key`, retain the mapping for a declared `retention`, TTL-expire abandoned work per a
declared `lease`, and expose a status read addressed by the epoch alone — the last being the only thing
that makes an ambiguous outcome recoverable. Config load refuses `effect: mutating` with `mode: request`,
with an incomplete idempotency block or `honoured: false`, or without `{epoch}` in `poll.path`. Refusing
to *configure* a duplicate-effect generator is the enforcement point; there is no runtime one.

**Three config-load rules** make "one credential per target" a refusal rather than a convention: the
grant key is derived from the target name (no separate `auth.capability` to diverge from it); exactly one
matching grant must exist per target and a dangling `callout:invoke@x` is a load error, because a grant
nobody can audit is worse than none; and two call-out grants carrying an identical `TokenRef` are
refused. The last is syntactic equality only — two refs naming different sources that resolve to the same
secret are not detectable.

**CT-1, stated as an assumption.** Agents cannot edit `instance.yaml` or apply cluster changes. The
owner's reasoning, which is the load-bearing part: an actor who could do both could equally spin up pods
and manipulate the cluster, so the target list is not the marginal risk in that world.

**CT-1's second clause is weaker than it reads.** "Under sandboxing they lack the reach" is not true of
the shipped tier-1 default — sandbox config absent or zero-valued is disabled, and a deterministic shell
stage receives no filesystem confinement on any platform. So CT-1 holds on tier 1 by *deployment
obligation*, not by a mechanism in the tree. The interim control is a load-time writability check on the
instance root and `instance.yaml`; TBH-3 discharges it properly.

**Defence in depth.** Credentials are refs, not values — a `TokenRef` is exactly one of
env/file/keychain/store, resolved out-of-band — so naming a target does not mint a credential for it.

**The granularity ceiling.** A target-scoped grant bounds *which target* a stage reaches, not *which
workflow* may reach it: deterministic grants are runner-owned and gaggle-wide, so every stage in the
gaggle declaring the grant can invoke the target. **The unit of call-out isolation is the gaggle.** A
per-target `allowedWorkflows` list is enforceable in the executor with no new plumbing, and is deferred.

---

## 4. Egress, the gateway, and the escape hatch

A **gateway, not a special ingress/egress stage pod**. The pod idea recreates what
[k8s-infra-shape.md](k8s-infra-shape.md) §5 forbids (stage pods run agent-authored code), has the wrong
lifetime (a callback must outlive the stage), and has no stable identity. That same section already
specifies an HTTPS ingress for API and portal, so a gateway is the component it assumes, not an exception.

Inheriting `external-telemetry`'s allowlist verbatim is insufficient: it is a hostname string compare
with resolution happening afterward inside the `RoundTripper` — no dial-time IP check, no rebinding
defence — with no port constraint and one credential per *connector* rather than per host. This design
requires scheme+host+port+path-prefix per target, dial-time IP checking with link-local/RFC1918/loopback
denied unless opted in, one credential bound to one target, and **an explicitly declared proxy**:
`http.DefaultTransport` is `Proxy: ProxyFromEnvironment`, so a daemon started under `HTTPS_PROXY` would
route through an undeclared proxy — and the dial-time IP check would inspect the *proxy's* address and
pass. In-process enforcement is the whole control at tier 1, so it has to be complete.

**The escape hatch is governed, not absent.** #2034 is the precedent: a host-global
`GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE` opt-out on Windows, stamped into child process env and never into
journal, verdict or portal. The hatch was not wrong to exist — it was wrong for being broad and silent.
Required here: target-scoped never wildcard, backed by an out-of-band credential, journaled at use with
structured callee identity (D15 makes that record non-lossy), and surfaced in the portal.

**`no-phone-home` is not the egress control.** `test/nophonehome/main.go:234` gates `.yml`/`.yaml`
scanning on `/.github/workflows/`, so `instance.yaml` and every reference workflow are out of scope.

---

## 5. Threats

| Threat | Mechanism | Disposition |
| --- | --- | --- |
| Gate verdict forgery via `Outputs` | `AutomatedInputs` used to copy subject `Outputs` over the runner-set `status` | **Closed on `main`.** #2925 landed: outputs are copied first, reserved keys stamped after, and a collision returns an error (`internal/gate/automated.go:97-122`) |
| Gate steering via the error triple | `errorCode`/`errorMessage`/`errorRetryable` come from `subject.Error`, which a call-out fills from the far side; `failure-class` forks on it | D11's closed vocabulary and constant-`false` `Retryable`. Residual: `errorMessage` still carries far-side text |
| Prompt injection downstream | A response artifact becomes a `ContextPointer` a model reads as instructions | D17 + D18 + XC-A8. SEC-047-equivalent |
| Kind substitution at runtime | The overlay chooses the executor from `env.Inputs` | Closed for `external-call` by D1/D2/D5 |
| SSRF, credential cross-delivery, ambient proxy | Hostname-only allowlist, no port or IP pinning, one credential across an allowlist, inherited proxy | §4, plus §3's three config-load rules. The cross-host leak is a *latent* type-level hazard — the shipped ADX adapter targets one fixed cluster |
| Credential leakage on tier 3 | Engine `Activities` are built with no registry scrubber, falling back to a seven-regex pattern net that matches no opaque vault secret; activity arguments are never scrubbed at all | #2930 landed the scrubber; the argument half is **#2931**, which is `goobers:needs-human` |
| Duplicate far-side effect | At-least-once with full re-execution, no compensation; every Temporal timeout classified as retryable infra | D7 + D8 + D9. Residual: a gate-ordered repass mints a *new* epoch by design, so a repass duplicate is not suppressible |
| Far-side availability denial | Unclamped `Retry-After` | D24 |

---

## 6. Prerequisites

Two of the original four **landed while this document was being written** — #2925 (gate forgery) and
#2926 (envPassthrough guard). What remains:

1. **#2931** — unscrubbed Temporal activity arguments. Tier-3 gate; `goobers:needs-human`, so the gate is
   an owner ruling, not filed-and-approved work. (#2927 is the tracking parent; #2930, the scrubber half,
   has landed.)
2. **The `TimeoutError.TimeoutType()` split** — a prerequisite rather than a follow-up, because a mutating
   call-out plus blind infra retry is a duplicate-submit generator (`internal/engine/retry.go:161-175`).
3. **D18's integrity floor** — repairs a live defect independent of this feature, and touches the grades
   of existing provider-builtin, `ci-poll` and external-telemetry stages (Q4).

---

## 7. Scope

**Ships:** the typed surface and its eight compile rules; HTTP transport, sync and submit-then-poll;
targets, target-scoped credentials and the three config-load refusals; the call epoch and the
receiving-side contract; the per-target call budget; the response projection and its grading; run-ID
correlation and `traceparent`; the in-process egress binding with its explicit proxy.

**Deferred, with owners:** enforced egress (#1307 / #2898 — both approved but parked on
`goobers:needs-human` pending the enforcement-baseline choice between CIDR NetworkPolicy, Cilium FQDN
policy and proxy-only, which this design's tier-3 binding assumes an answer to) · the single inbound door
(#648, itself parked) · per-workflow target isolation · instance-wide call attribution (UNOP-5, partially
deferred) · SSH transport · tier-3 placement by egress reachability.

**Deferred and unowned** — needs filing: stitching a far-side span into the run trace (#2865 closed, so
the old owner is gone); an async return leg, which needs a resumable stage state that neither #648 nor
`ResultBlocked` provides.

**Out of scope:** provisioning, scheduling, cost machinery; the test-sandbox cluster (#670); any second
inbound surface.

---

## 8. Residuals

| Residual | Why it stays |
| --- | --- |
| The dedup contract is an operator assertion Goobers cannot verify | Config load refuses malformed *declarations*; nothing proves the far side honours the epoch. The largest single residual |
| Orphaned far-side work | Cancellation orphans by design at both tiers; no reaper reaches the network. Mitigated only by the declared TTL lease |
| Ambiguous outcomes are real | D10 names them; it does not eliminate them |
| Repass-ordered duplicates | The gate that orders a repass reads the call-out's own outputs |
| `errorMessage` carries far-side text | Only `errorCode` and `Retryable` are constrained |
| Duplicate-token detection is syntactic | Two refs resolving to one secret are not detectable |
| CT-1 rests on deployment discipline on tier 1 | Sandbox is off by default and wraps a harness argv only |
| Isolation granularity is the gaggle | A grant bounds which target, not which workflow |
| Fan-out rule 1 has no runtime backstop | Epoch uniqueness rests on a compile rule; `branch` is retained against a future relaxation |

---

## 9. Open questions

Twenty were recorded during drafting. Five change other people's code or this document's shape and are
kept here; the other fifteen — budget defaults, clamp placement, whether an operator rerun advances the
epoch, `CallEpoch` naming, duplicate-ref scoping, and the rest — are recorded on the phase-1 issue
(#2976).

| # | Question |
| --- | --- |
| Q1 | Is tier-3 heartbeating a v1 prerequisite, or does reconcile mode make `START_TO_CLOSE` survivable without it? |
| Q2 | Is deleting `auth.mode: none` (D19) accepted? It makes authority real, but every target then needs a credential |
| Q3 | Is the `StageContractVersion` bump accepted, for `ResultEnvelope.ExternalRefs` (D15)? |
| Q4 | Is demoting existing provider-builtin, `ci-poll` and external-telemetry stage grades from `trusted` accepted? D18 touches shipped behaviour well beyond this feature |
| Q5 | Does D5 survive the freeze policy? It is the one place this design touches `v_current` |

Three of the fifteen are rulings rather than tuning and are flagged as such on the issue: tier-3's
per-worker budget ceiling (N workers ⇒ N × `maxCallsPerHour`), whether refusing an in-flight run on
upgrade needs a drain, and whether completion-time tier-3 observability is acceptable for a bounded-wait
call-out.

---

## 10. Follow-up issues

Filed alongside this document.

| # | Scope | Gate |
| --- | --- | --- |
| #2975 | Tracking issue for this design (this PR closes it) | — |
| #2976 | Phase 1 — the surface: typed `run.kind`, the eight rules, preview gating, the scoped capability, `externalCallTargets` config and its refusals, the daemon cross-reference pass. Also carries the fifteen open questions §9 does not keep | Q1–Q5 |
| #2977 | Phase 2 — the transport: executor, host, epoch, receiving contract, budget, clamp, projection, tracing. Also fixes the two `external-telemetry` defects it would otherwise inherit as a template | #2976 |
| #2978 | Phase 3 — tier 3 | #2931, #2980, and the executor-artifact adoption channel (`internal/workerhost/artifacts.go`, spike-only today) |
| #2979 | Produced-integrity floor (D18) | Q4 — it demotes existing stage grades |
| #2980 | `TimeoutType()` split + tier-3 heartbeats | Q1 |
| #2981 | Route the five `vcurrent`-pinned feature-router functions through `interpreterForDefinition` (D3) | Prerequisite for any later goober-scoped surface |
| #2982 | ADR 0002 extension for the third capability spelling | — |
| #2983 | Stitch a far-side span into the run trace; add the `SpanContext` accessor `traceparent` needs | Unowned since #2865 closed |
| — | Async return leg — not filed, because it needs a resumable stage state that neither #648 nor `ResultBlocked` provides. File it when that exists | #648 |

**Proving rig:** an Azure Container App target, `minReplicas=0`, in one dedicated resource group so
teardown is a single `az group delete` — real TLS, real auth headers, real cold-start latency, plus
misbehaving modes (slow, oversized, wrong content type, 5xx, hostile `Retry-After`) to prove the limits
and the blocked-vs-failed disposition.
