# External call-out stages

**Status:** design of record, pending owner rulings (§9).
**Owns:** #1087. Supersedes the deferral in `docs/design/large-repo-execution-model.md` §8.
**Detail lives in the gated issues (§10), not here.**

A workflow declares a stage that is a pointer to something outside the instance. The runner hands
work out over HTTP, gets information back, and the response becomes a graded artifact plus a declared
scalar projection. Targets are operator-configured and named from the DSL; credentials are scoped to
one target; the call is correlated into the run's existing trace.

This is not the test-sandbox contract (#670 — the environment a stage's tests *point at*), and not
capability-routed execution (#1087's full form — where a stage's *own compute* runs). It generalises
`external-telemetry`, the shipped kind that already calls out under host governance.

---

## 1. Decisions

| # | Decision | Why |
| --- | --- | --- |
| D1 | Typed `run.kind`, landing TBH-1 (`docs/stage-contract.md:452-488`), with `external-call` as its first value | `inputs.kind` is runtime-overridable: `run.go:3071-3088` overlays `inputsFrom` onto `env.Inputs` and `dispatch.go:105-117` selects the executor from the overlaid map, while admission reads only the static literal (`v_next/compile.go:455,462`). A stage can read as `shell` to a reviewer and dispatch a call-out |
| D2 | `inputsFrom` binding `kind` becomes a v_next **error**; a warning on frozen v_current | Once a call-out exists upstream, the *endpoint* chooses a downstream stage's executor. No honest use remains once `run.kind` is typed |
| D3 | DSL **2.0 preview feature**, not a 2.1 bump | A minor costs a copied interpreter (v_current 8,459 lines / v_next 9,648), a hand-edited arm, a migration edge, and a 3-release support commitment validated at package init (`supportpolicy.go:11-16`) |
| D4 | `external-call` is **refused on the frozen interpreter** — the kind and the scoped grant both | A shared vocabulary is not a shared feature. None of the guards that make a call-out safe exist in v_current. Contract-preserving: it refuses a document no 1.4 author could have written |
| D5 | **v1 may mutate the far side** | Owner ruling. Read-only was contradictory with submit-then-poll (a submit *is* state creation) and was enforced by nothing — no safe-method constraint, and one bearer token per target authorises writes exactly as reads |
| D6 | A deterministic **call epoch** = runID + branch + stage + entry, minted inside `runTask` (`run.go:2531`) from a run-scoped ledger threaded as `stepBudget` already is | Both tier-1 dispatch arms funnel through `runTask` exactly once per intended dispatch. Nothing minted in an activity survives a Temporal re-dispatch — the engine has no `workflow.SideEffect`. Held constant across attempts, advancing on a repass |
| D7 | The reconcile decision **splits across the seam**: the classifier returns `reconcileNext`, the dispatch closure acts on it | `attemptFailureClass` runs inside the workflow function with only the task and error available; `effect` lives in `instance.yaml`, which the in-workflow re-compile (`engine/engine.go:204-207`) must not read |
| D8 | An explicit **ambiguous outcome** (`callout_outcome_unknown`) | `ResultStatus` is success/failure/blocked/no-work (`envelope.go:174-197`). Today an unknown must masquerade as failure, which a gate acts on |
| D9 | The kind owns its error: `Retryable` constant `false`, `Code` from a closed `callout_*` vocabulary | `AutomatedInputs` re-stamps the error triple *after* the outputs copy (`gate/automated.go:103-110`) and `failure-class` forks on it — a second door the reserved-output rule does not reach |
| D10 | **Sync + submit-then-poll**, modelled on `ci-poll`. No async callback | No landing zone: `config.go:2244-2253` refuses non-loopback bind without TLS+OIDC with no override; `k8s-infra-shape.md:84` "No inbound to stage pods. Ever."; `ResultBlocked` is terminal, not resumable; the engine refuses human gates outright |
| D11 | **HTTP only.** SSH is a separate design | No `crypto/ssh` import, no `HostKeyCallback`, no `known_hosts` anywhere in the tree — and `test/nophonehome` monitors only net/http/smtp/grpc/OTLP, so an `ssh.Dial` target is invisible to the merge gate (verified by execution) |
| D12 | **No new journal event types.** `stage.*` + `artifact.recorded` + `ref.touched` | `IsConformanceNormative` ends `default: return true` (`journal/event.go:429-431`), so a new type must be emitted byte-identically by both runners — impossible against a live endpoint. And `projectableEventTypes` is a closed 9-entry whitelist (`engine/projection.go:25-35`) validated over *all* ops before `journal.Create`, so an unknown type yields **no journal at all** |
| D13 | Callee identity is structured `(provider, kind, id)`, never a URL | `ExternalRef.URL` is dropped from the conformance projection (`journal/conformance.go:68-72`) |
| D14 | The response is a graded artifact plus a **declared, type-checked scalar projection**. Never a wholesale `Outputs` merge | Mirrors `external-telemetry`, which emits three named scalars rather than the body |
| D15 | A **produced-integrity floor**, as a runner change | `run.go:2754-2755` unconditionally overwrites the executor's grade, and `inputsfrom.go:352-354` returns `IntegrityTrusted` for a stage with no graded input — the common call-out shape. Without the floor the far side's scalars reach downstream stages labelled trusted |
| D16 | Targets live in `instance.yaml`, referenced by name; **`auth.mode: none` is deleted** | Every target carries a credential, so authority is real rather than self-granted (§3) |
| D17 | Target-scoped grants `callout:invoke@<target>` | Follows `credentials.RepoScopedCapability` (`credentials/scoping.go:69-78`), already opaque to the injector. A flat capability would make operator curation an all-or-nothing switch |
| D18 | Egress is **one contract, two bindings**: in-process on tier 1, a deployed gateway on tier 3 | Same declaration, swappable substrate — the framing #1087 uses for execution routing. Converges with #1307 and #2898, both approved |
| D19 | Tracing: **correlate now, stitch later** | The run ID *is* the 32-hex trace ID (`telemetry/id.go:34-53`), so correlation is free. `traceparent` from a `SpanKindClient` span needs one small change (`telemetry.Span` has no `SpanContext` accessor). Stitching is not promised: no OTLP receiver, foreign parents are rejected, and tier 3 emits no spans at all (#2865) |
| D20 | A far-side `Retry-After` is **clamped** by the kind | Nothing clamps it today: `run.go:2799-2808` returns it verbatim and tier 3 turns it into a durable `workflow.Sleep` (`engine/retry.go:118`). A hostile `503` + far-future `Retry-After` parks the stage past its own timeout |

---

## 2. The surface

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

Seven rules hold at **every** compile root, decidable from the document plus build-time constants —
no injected option, no nil guard, so they cannot be silently absent where a root forgot to wire them:
`externalCall` present iff the kind matches; the kind's minimum DSL version (D4); target *shape* only;
the response projection well-formed; the grant biconditional; no projected output naming a reserved
gate key; and `workspace: scratch` with a non-zero timeout.

Target and operation **existence** is deliberately not a compile rule — the compiler has no
`*instance.Config`. A tier-1 daemon cross-reference pass (`cmd/goobers/daemon.go`) checks it at load.

`run.network: none` is refused on a call-out: an in-process kind is outside `procenv`, the sandbox and
the network mode, all of which shape subprocesses only. Silently meaningless isolation is worse than none.

---

## 3. Trust boundary

**CT-1, stated as an assumption, not implied:** agents cannot edit `instance.yaml` or apply cluster
changes. The owner's reasoning, recorded because it is the load-bearing part — an actor who could do
both could equally spin up pods and manipulate the cluster, so the target list is not the marginal risk
in that world.

**Defence in depth, secondary:** credentials are refs, not values. A `TokenRef` is exactly one of
env/file/keychain/store, resolved out-of-band, so naming a target does not mint a credential for it.
This is why `auth.mode: none` is deleted (D16) — an unauthenticated target has no such backstop, and
egress arbitrates *destination*, not *which stage is asking*.

**If CT-1 is false**, an actor can add a target but still cannot mint its secret; what they gain is the
ability to point an existing credential at a new path on an already-configured host. That is the residual
the per-target path-prefix constraint exists to bound.

---

## 4. Egress, the gateway, and the escape hatch

A **gateway, not a special ingress/egress stage pod.** The pod idea recreates what
`k8s-infra-shape.md:84` forbids (stage pods run agent-authored code), has the wrong lifetime (a callback
must outlive the stage), and has no stable identity (pods are capability-routed). Note `:77` already
specifies an HTTPS ingress for API and portal — a gateway is the component §5 assumes, not an exception
to it. Inbound, when it comes, is **#648** and never a second door.

Inheriting `external-telemetry`'s allowlist verbatim is insufficient: it is a hostname string compare
with resolution happening afterward inside the `RoundTripper`, so no dial-time IP check and no rebinding
defence; no port constraint; and one credential per *connector* rather than per host. This design
requires scheme+host+port+path-prefix per target, dial-time IP checking with link-local/RFC1918/loopback
denied unless opted in, and one credential bound to exactly one target.

**The escape hatch is governed, not absent.** #2034 (`network:none` de-isolating globally with no
visibility) is the precedent: the hatch was not wrong to exist, it was wrong for being broad and silent.
Required — target-scoped never wildcard, backed by an out-of-band credential, journaled at use with
structured callee identity, surfaced in the portal.

**`no-phone-home` is not the egress control.** `test/nophonehome/main.go:227-240` scans `.yml`/`.yaml`
only under `/.github/workflows/`, so `instance.yaml` and every reference workflow are out of scope. It
protects the repo from maintainer-shipped destinations and says nothing about runtime config.

---

## 5. Threats

| Threat | Mechanism | Disposition |
| --- | --- | --- |
| Gate verdict forgery | `AutomatedInputs` copies subject `Outputs` over the runner-set `status` (`gate/automated.go:98-105`) | **Prerequisite #2925.** Closed on the outputs door; the error triple is a second door closed by D9. Residual: `errorMessage` still carries far-side text |
| Prompt injection downstream | A response artifact becomes a `ContextPointer` a model reads as instructions | D14 + D15. SEC-047-equivalent; stated, not solved |
| Kind substitution at runtime | The overlay chooses the executor from `env.Inputs` | Closed for `external-call` by D1/D2/D4 |
| SSRF / credential cross-delivery | Hostname-only allowlist, no port or IP pinning; one credential across an allowlist | §4. Note the cross-host leak is a *latent* type-level hazard — the shipped ADX adapter targets one fixed cluster |
| Credential leakage | `envPassthrough` guard is MCP-scoped; tier-3 activities run with a nil scrubber and unscrubbed arguments | **Prerequisites #2926, #2927** |
| Duplicate far-side effect | At-least-once with full re-execution, no compensation; every Temporal timeout classified as retryable infra | D6 + D7. A repass mints a *new* epoch by design, so an ordered-repass duplicate is not suppressible — stated residual |
| Far-side availability denial | Unclamped `Retry-After` | D20 |

---

## 6. Prerequisites

Four, all landing before the transport:

1. **#2925** — gate verdict forgery. Live and exploitable today by an agentic stage.
2. **#2926** — `envPassthrough` guard scoped to MCP grants only.
3. **#2927** — engine `Activities` built with a nil scrubber. Tier-3 gate.
4. **The `TimeoutError.TimeoutType()` split** — a prerequisite rather than a follow-up, because a
   mutating call-out plus blind infra retry is a duplicate-submit generator.

Plus D15's integrity floor, which repairs a live defect independent of this feature.

---

## 7. Scope

**Ships:** the typed surface and its compile rules; HTTP transport, sync and submit-then-poll; targets
and target-scoped credentials; the call epoch and the receiving-side contract; the response projection
and its grading; run-ID correlation and `traceparent`; the in-process egress binding.

**Deferred, with owners:** enforced egress (#1307/#2898) · async return leg (#648) · stitched tracing
(#2865) · SSH transport · tier-3 placement by egress reachability.

**Out of scope:** provisioning, scheduling, cost machinery; the test-sandbox cluster (#670); any second
inbound surface.

---

## 8. Residuals

| | |
| --- | --- |
| Orphaned far-side work | Cancellation orphans by design at both tiers; no reaper reaches the network. Mitigated only by the far side's TTL lease |
| Ambiguous outcomes are real | D8 names them; it does not eliminate them |
| Repass-ordered duplicates | The gate that orders a repass reads the call-out's own outputs |
| `errorMessage` carries far-side text | Reaches a gate's inputs; only `errorCode` and `Retryable` are constrained |
| Fan-out rule 1 has no runtime backstop | Epoch uniqueness rests on a compile rule; `branch` is retained against a future relaxation |
| Tier 3 is invisible in flight | The journal is authored at completion; there is no run directory while a call hangs |

---

## 9. Open questions for the owner

Five change other people's code or this document's shape:

| # | Question |
| --- | --- |
| Q1 | Is tier-3 heartbeating a v1 prerequisite, or does reconcile mode make `START_TO_CLOSE` survivable without it? |
| Q2 | Is deleting `auth.mode: none` (D16) accepted? It is the cheapest way to make authority real, but every target then needs a credential |
| Q3 | Is the `StageContractVersion` bump to `v1alpha9` accepted, for `ResultEnvelope.ExternalRefs`? |
| Q4 | Is demoting existing provider-builtin, `ci-poll` and external-telemetry stage grades from `trusted` accepted? D15 touches shipped behaviour well beyond this feature |
| Q5 | Does D4 survive the freeze policy? It is the one place this design touches `v_current` |

Eleven more are tuning — budget defaults, clamp placement, whether an operator rerun advances the
epoch, naming of `CallEpoch`, and the scoping of duplicate-token-ref refusal. They are recorded on the
phase-1 issue rather than here.

---

## 10. Follow-up issues

| Scope | Gate |
| --- | --- |
| Promote #1087; fold #2896 in, including its two stale-fact corrections | Owner approval |
| Prerequisites #2925 / #2926 / #2927 | Filed, approved |
| `TimeoutType` split + tier-3 heartbeats | Q1 |
| Integrity floor (D15) | Q4 |
| Phase 1 — the surface: typed `run.kind`, the seven rules, preview gating, the scoped capability, the daemon cross-reference pass | Carries the phase-1 acceptance criteria and the eleven tuning questions |
| Phase 2 — the transport: executor, host, epoch, clamp, projection, tracing | Carries the phase-2 acceptance criteria |
| Phase 3 — tier 3 | Gated on #2927, the `TimeoutType` split, and the worker runtime-wiring extraction (#2904) |
| ADR 0002 extension for the third capability spelling | — |
| Enforced egress consumption (#1307 / #2898) · async return leg (#648) · stitched tracing (#2865) · SSH transport | Deferred |

**Proving rig:** an Azure Container App target, `minReplicas=0`, in one dedicated resource group so
teardown is a single `az group delete` — real TLS, real auth headers, real cold-start latency, plus
misbehaving modes (slow, oversized, wrong content type, 5xx, hostile `Retry-After`) to prove the limits
and the blocked-vs-failed disposition.
