# Design: External call-out stages — a deterministic stage kind that points outside the instance

> Status: **design pass complete; owner promotion gate pending — prescriptive** · Area prefix: `XC` ·
> Milestone for the v1 deliverable: **Custom & Generic Stages** (where #737's merged `KindRegistry` seam
> landed), *not* V2. Owning issue: [#1087](https://github.com/Agent-Clubhouse/Goobers/issues/1087) —
> OPEN, **not** `goobers:approved`, under a standing 2026-07-26 "keep deferred" ruling. This document
> recommends its promotion and folds in #2896; it authorizes nothing else. Origin:
> `docs/design/large-repo-execution-model.md:489-503`, which defers "an external-target executor … it
> needs its own generic design pass". This is that pass.
> **Rev 2 (2026-08-13) reverses rev 1's read-only posture on an owner ruling: v1 may mutate the far
> side.** Every consequence is designed here, not deferred.
> Builds on (all landed): #737 (`internal/executor/dispatch.go`), the external-telemetry connector,
> capability-scoped credential non-injection (`internal/credentials`), journal conformance.
> Companions: `docs/design/trust-boundary-hardening.md`, `docs/design/k8s-infra-shape.md`,
> `docs/adr/0002-provider-neutral-capability-namespaces.md`, `docs/design/dsl-version-lifecycle.md`.

A workflow can reach outside the instance today only through code Goobers itself wrote, or by an agentic
stage shelling out to `curl` — unbounded, uncredentialed, unjournaled. This design adds one deterministic
stage kind, `external-call`, selected by a typed `run.kind`, that hands a declared request to a named
operator-configured target, waits under a bounded budget, and brings back a graded, projected result.
The target is named in `instance.yaml` and referenced from the DSL by handle only; the credential is a
reference the stage never sees; the authority is a target-scoped capability, `callout:invoke@<target>`;
the response is recorded as an unapproved artifact and reaches stage outputs only through a declared,
type-checked projection.

**v1 may mutate the far side.** A mutating operation is operator-declared, carries a deterministic **call
epoch** as its idempotency key, is refused at config load unless its target asserts a receiver-side dedup
contract, and reconciles rather than blindly re-submits after a lost worker. A workflow cannot name an
HTTP method at all. v1 is HTTP-only, adds no journal event types, promises trace *correlation* rather
than stitched spans, and ships on tier 1 with tier-3 dispatch gated on four named landings. Four live
defects unrelated to this feature are prerequisites (§1.6).

## Table of contents

1. [Context, decision, and scope](#1-context-decision-and-scope) — the capacity, governance, the D1–D22
   decision table, what ships, what is deferred, and the prerequisite/tier-3 gates
2. [The DSL surface](#2-the-dsl-surface) — typed `run.kind`, `run.externalCall`, closing dynamic
   selection, per-root admission, versioning, and the lockstep checklist
3. [Execution contract and semantics](#3-execution-contract-and-semantics) — the seam, the two call
   shapes and the `operations` block, the call epoch, the receiving-side contract, deadlines, retry and
   reconcile mode, the ambiguous outcome, and return-leg bounds
4. [Targets, credentials, and the trust boundary](#4-targets-credentials-and-the-trust-boundary) — the
   target registry, where mutation is refused, CT-1, one-credential-per-target, capability grants,
   injection, and confinement
5. [Egress, the gateway, and the governed escape hatch](#5-egress-the-gateway-and-the-governed-escape-hatch)
6. [Journal, conformance, and tracing](#6-journal-conformance-and-tracing) — no new event types, what the
   cross-runner diff compares, callee identity, the integrity floor, tracing, disclosure
7. [Security model and threat analysis](#7-security-model-and-threat-analysis)
8. [Implementation plan and follow-up filing](#8-implementation-plan-and-follow-up-filing) — the phase
   sequence and the gated issues that carry each phase's acceptance criteria
9. [Residuals recorded during drafting](#9-residuals-recorded-during-drafting)
10. [Open questions for the owner](#10-open-questions-for-the-owner)

## 1. Context, decision, and scope

### 1.1 The capacity, and the three features it is not

A workflow today reaches outside the instance only through code Goobers wrote: the git/backlog provider,
the model endpoint, and one read-only telemetry connector. An inventory service, a deployment tracker, a
licence server, an approval-of-record system, a customer's own CI — all reachable only by an agentic
stage shelling out, invisible to review and unrepresented in the journal. This design adds a
deterministic stage kind that is a **pointer to something outside the instance**. The transport in v1 is
HTTP; the surface is transport-agnostic by construction, because the second transport (SSH/WinRM to a
statically-addressed host) is #1087's near-term form and must land on the same declaration.

| Axis | The question it answers | Owner |
| --- | --- | --- |
| **Test sandbox** (#670 cluster) | *What environment do this stage's tests point at?* | #670, V2, unapproved |
| **Capability-routed execution** (#1087 full form, generalizing #659) | *Which machine runs this stage's own compute?* | #1087/#659 |
| **External call-out** (this document) | *What does this stage fetch from — or ask of — outside the instance, under whose authority, and how is the answer graded?* | this document, #1087's near-term deliverable |
| **External telemetry** (shipped) | The row above, for one connector class with a fixed query language | `internal/executor/externaltelemetry.go` |

External telemetry is the precedent this generalises, and it demonstrates the whole shape: a kind
registered at one composition root (`cmd/goobers/runnerwiring.go:2153-2179`), a connector named in
`instance.yaml` and referenced by name only (`api/schemas/instance.schema.json`
`$defs.externalTelemetryConnector`: *"Workflows reference it by name only"*), a capability enforced at
compile time (`internal/workflow/v_next/compile.go:462`), a response artifact hardcoded to
`IntegrityUnapproved` (`internal/executor/externaltelemetry.go:111-116`), and three named scalar outputs
projected instead of the body (`:121-124`, `:145-147`). This design takes that shape, removes the fixed
query language, and fixes two defects the precedent shipped with: a nil progress hook (§3.5) and a
private credential resolver that bypasses the injector (§4.6).

One DSL confusion matters. A stage carries two capability-shaped fields: `Task.Capabilities`
(`api/v1alpha1/workflow_types.go:172`) are credential grants validated against `internal/capability`;
`Task.RequiredCapabilities` (`:195-206`) are free-form runner claims matched at schedule time. The
call-out's authority is the former.

### 1.2 Governance: which issue owns this

**#1087** is the surviving owner: OPEN, on V2, not `goobers:approved`, under a standing 2026-07-26
ruling ("keep deferred … No concrete triggering need has materialized"), with a merged design doc as its
acceptance shape. **#2896** was closed `NOT_PLANNED` as its duplicate and holds the better research — it
is grounded in the spike's placement mechanism and enumerates what `DeterministicRun` cannot express
(`api/v1alpha1/workflow_types.go:331-363`: no target field). This document recommends **promoting #1087
and folding #2896 into it**, with the v1 HTTP call-out filed as its first child on *Custom & Generic
Stages* — where #737's `KindRegistry` seam landed (`internal/executor/dispatch.go:28-39`) — not V2.

Three #2896 records survive the fold rather than being lost to it. Its **"#2860 first, without
exception" dependency is deliberately overridden, not overlooked**: that ruling is scoped to the
routing/admission binding and #2860 governs `Task.RequiredCapabilities`, which a call-out never
declares — recorded as an overridden ruling so the next reader sees a decision. Its recommendation to
open the routing-key vocabulary *before* adding target kinds is carried to §8's filing list. And its two
stale-fact corrections are filed with the promotion: #1087's body claims no capability field exists
(false since #1101/`RequiredCapabilities`), and #1102 claims agentic stages cannot be containerized.

This document does **not** authorize implementation; nothing here overrides the 2026-07-26 ruling.

### 1.3 Decision summary

Every row is settled and normative. **D1–D22 are the document's only decision identifiers.**

| # | Decision | Load-bearing reason | § |
| --- | --- | --- | --- |
| D1 | **Selection is a typed `run.kind`**, with `external-call` its first value | `inputs.kind` is runtime-overridable via the `inputsFrom` overlay while compile-time admission matches only the static literal, so a stage reads as `shell` and dispatches a call-out | §2.2 |
| D2 | **Dynamic kind selection is closed in the same change**; `inputsFrom` binding `kind` becomes a v_next **error** | The binding also exempts a stage from `CheckStageTimeoutCoherence` (`internal/workflow/v_next/timeoutcoherence.go:20-22`); in-tree blast radius is zero | §2.3 |
| D3 | **DSL 2.0 as a `preview` feature — no 2.1 bump** | A minor costs a third copied interpreter, a migration edge and a three-minor support commitment (`internal/supportmatrix/supportpolicy.go:10-18`) | §2.5 |
| D4 | **Every artefact added is Task-scoped**, avoiding the v_current-pinned feature router | `FeaturesForGaggle`/`FeaturesForGoober`/`CheckFeatureSupport` call `vcurrent.*` unconditionally (`internal/workflow/features.go:97,102,107`) | §2.5 |
| D5 | **v1 MAY mutate the far side.** Mutation is operator-declared per operation, never nameable from a workflow | "Read-only + submit-then-poll" was self-contradictory — a submit *is* far-side state creation — and read-only was never enforced anywhere: no safe-method constraint existed, and one bearer token per target authorizes a write exactly as it authorizes a read | §3.2, §4.2 |
| D6 | **The call epoch is the idempotency key**: one deterministic string per *intended call*, minted in walk code and never in an executor or activity — inside `runTask` from one run-scoped ledger on tier 1, from the replayed per-run map on tier 3 — constant across attempts, advancing on a gate repass | The engine has no `workflow.SideEffect` (verified: zero hits over non-test `internal/engine`), so any activity-minted value is fresh on every re-dispatch — the case dedup covers. `runID+stage` alone collides across repasses, making a legitimately-intended second call a silent far-side no-op | §3.3 |
| D7 | **`attemptFailureClass` must read `TimeoutType()` before tier-3 dispatch**, and the reconcile decision splits across the seam: the workflow signals *"this re-dispatch may be a repeat"*, the executor decides whether a repeat is safe | It classifies every `*temporal.TimeoutError` as `journal.AttemptInfra` without reading the type (`internal/engine/retry.go:161-174`, timeout arm at `:169-171`) and `dispatchWithRetry` retries it. `START_TO_CLOSE` means the worker was lost possibly *after* the far side committed. The workflow cannot key on the *effect posture*: that is per-operation `instance.yaml` data, and the in-workflow re-compile (`internal/engine/engine.go:204-207`) must read nothing instance-derived | §3.6 |
| D8 | **An ambiguous outcome is `blocked` + reserved code `callout_outcome_unknown`**, not a fifth `ResultStatus` | Neither `taskOutcome` switch has a `default:` arm (`internal/runner/run.go:2114-2246`, `internal/engine/engine.go:368-384`), so an unhandled status falls through to `switch t.Next` and **advances as if it succeeded** | §3.7 |
| D9 | **Bounded request/response or submit-then-poll, both runner-driven. No async callback** | No landing zone: off-loopback API bind is refused without TLS+OIDC with no override (`internal/instance/config.go:2246-2253`), the webhook listener is loopback-only (`:364`), tier 3 forbids inbound to stage pods (`k8s-infra-shape.md:84`) | §3.2, §5.2 |
| D10 | **HTTP only in v1** | `golang.org/x/crypto` is indirect-only (`go.mod:101`), no `crypto/ssh` import, no host-key trust model, and the merge gate monitors only net/net-http/net-smtp/gRPC/OTLP (`test/nophonehome/main.go:26-81`) — an `ssh.Dial` target is invisible to it | §1.5 |
| D11 | **No new journal event types in v1** | `IsConformanceNormative` ends `default: return true` (`internal/journal/event.go:429-431`), so a new type joins the diff silently; `projectableEventTypes` is a closed nine-entry whitelist (`internal/engine/projection.go:25-35`) validated over every op before `journal.Create`, so an unknown type yields no journal directory at all | §6.1 |
| D12 | **Callee identity is `(provider, kind, id)`, never a URL**; `id` is the call epoch | `ExternalRef.URL` is deliberately dropped from the conformance projection (`internal/journal/conformance.go:130`) | §6.3 |
| D13 | **`ref.touched` gets a real producer: `ResultEnvelope.ExternalRefs`** | Its only stage-level producer today is the `mutations.jsonl` sidecar (`internal/runner/run.go:3108`), whose tier-1 append is explicitly best-effort (`:2501-2510`). Under a mutating v1 that record *is* the audit of a real side effect | §6.3 |
| D14 | **No wholesale merge into `Outputs`** — a declared, type-checked projection only, and `responses[].output` may not name a reserved gate key | The stock automated checks read **seven** well-known keys, three of which (`ciStatus`, `landOutcome`, `queueOutcome`) select non-boolean compile-enforced gate branches | §2.2, §6.4 |
| D15 | **A stage's produced `Integrity` is floored** by its recorded artifacts and the executor's declared grade | The runner overwrites the executor's grade unconditionally (`internal/runner/run.go:2754`) with `producedIntegrity`, which returns `IntegrityTrusted` for an empty input-grade list (`internal/runner/inputsfrom.go:352-354`); without the floor the far side's scalars finish `trusted` | §6.4 |
| D16 | **Targets live in `instance.yaml`, referenced by name only**, under named assumption CT-1 | Workflow and goober YAML are agent-authored here; `instance.yaml` is not (`internal/instance/config.go:1320-1341`) | §4.1, §4.3 |
| D17 | **Every target is credentialed: `auth.mode: none` is deleted**, and the grant key is derived, not declared | For an unauthenticated target the whole authority chain is a string the author wrote: `t.Capabilities` reaches the envelope verbatim (`internal/runner/run.go:2993` → `:3907`) and the runtime re-check tests only that list | §4.4 |
| D18 | **Authority is the target-scoped capability `callout:invoke@<target>`**, written in that spelling by the author | Follows `RepoScopedCapability`'s `base@owner/name` keys, already opaque to `Injector`/`Set` (`internal/credentials/scoping.go:69-79`) | §4.5 |
| D19 | **Egress is one contract with two bindings** — in-process on tier 1, a deployed gateway with NetworkPolicy behind it on tier 3 | A CIDR cannot express `https://host:443/v1/estimate`, so NetworkPolicy can never *be* the target contract; two declarations would drift with nothing to catch it | §5.1 |
| D20 | **Ingress, when it comes, is #648**; a dedicated ingress/egress stage pod is rejected | `k8s-infra-shape.md:84` forbids inbound to stage pods; their lifetime is per-attempt and they have no stable identity | §5.2 |
| D21 | **Tracing: correlate now, stitch later** | The run ID already *is* a valid 32-hex OTel trace ID (`internal/telemetry/client.go:137-149`); stitching needs an OTLP receiver that does not exist and a relaxed trace-id invariant (`internal/telemetry/id.go:34-44`) | §6.5 |
| D22 | **The escape hatch is governed, not absent**: target-scoped, credentialed, journaled at every use, portal-surfaced, expiring | #2034 is the cautionary precedent — the hatch was not wrong to exist, it was wrong for being broad and silent | §5.4 |

### 1.4 What v1 ships

One deterministic stage kind on DSL 2.0 behind the preview annotation, carrying a `run.externalCall`
block: a target handle resolved against `instance.yaml`, a host-declared operation name, a mode, and an
explicit projection from named response fields to named stage outputs. It is authorized by
`callout:invoke@<target>`, receives its credential by reference through the existing injector, and never
sees the credential value in workflow-visible state.

It runs as a bounded request/response, or as submit-then-poll modelled on `ci-poll`'s budget discipline.
A submit-then-poll operation may declare `effect: mutating`, in which case the target must also declare
an idempotency contract or config load refuses the instance. Every dispatch carries a deterministic call
epoch; the far side is obliged to dedup on it, TTL-expire abandoned work, and expose an epoch-addressable
status read that makes reconciliation possible. The kind self-enforces its deadline, reports progress on
every tick, clamps far-side `Retry-After`, and bounds its own call volume per target. It records the
response as an unapproved artifact whose digested bytes are reproducible, projects only declared scalars,
and journals through the existing vocabulary with a structured callee identity whose `id` is the epoch.
**Tier 1 ships first**; tier-3 dispatch is gated on §1.6's four landings.

### 1.5 Deferred, and out of scope

**Deferred:** SSH/WinRM transport, and with it #1087's near-term external-target binding, on D10's
evidence — the highest-value follow-up, because it is what `large-repo-execution-model.md:489-503`
wants. **Async callback / far-side push**, on D9's evidence, until #648 lands the single authenticated
inbound door, at which point the callback shape is a delivery into that door. **Stitched traces**,
behind #2865, and with them widening `SpanRecord` to persist an OTel span kind and links
(`internal/telemetry/journalspan.go:223-237`). **Enforced egress**, to #1307 and #2898, which own it —
the declaration is shaped so they enforce it when they land rather than requiring a second declaration.
**Instance-wide call attribution**: §3.8 bounds one target, and spend across targets is not measured, so
UNOP principle 5 ("External quota is a budget, not a surprise",
`docs/design/unattended-operation.md:46-47`) is **partially deferred** — named here rather than left
silent, with a filing row in §8. **Per-workflow target isolation**: §4.4 records the granularity ceiling
and the deferred refinement whose enforcement point is already identified.

**Out of scope entirely:** routed remote execution (#659 and #1087's full form own placement); the
test-sandbox contract (#670); any scheduler, provisioning, or cost/lifecycle machinery; a
workflow-authored validator DSL for response contracts (trusted Go code validates, per TBH-1); and any
new inbound network surface. **A call-out is not placement-routed in v1**, and egress reachability is
assumed uniform across workers — a decision, not an oversight: the spike's
`stageTaskQueue`/`platformQueueSuffix` recognise only `os=` capability tokens
(`spike internal/engine/engine.go:690-693`), so the constraint has no expression today.

### 1.6 Prerequisites, in-change fixes, and the tier-3 gate

**This subsection is the single enumeration of prerequisite work**; `P0-a`…`P0-d` are its only labels.
Each is filed as its own issue and merged before the kind is dispatchable, and each is a live defect
today. The call-out does not create them — it widens the set of actors who can reach them from "our own
agent" to "an arbitrary endpoint".

- **P0-a — the automated-gate `status` forge.** `gate.AutomatedInputs` writes the runner-owned status key
  at `internal/gate/automated.go:99` then copies the subject's `Outputs` over it (`:100-102`). The three
  error keys are re-stamped *after* the copy (`:103-105`, unconditionally, including when
  `subject.Error` is nil), so **only `status` is currently forgeable** — P0-a is a one-key repair plus
  making the reservation explicit rather than an accident of statement order. Both tiers call it
  (`internal/runner/run.go:3575`, `internal/engine/engine.go:525`).
- **P0-b — capability grants escape the `envPassthrough` check.** `internal/instance/config.go:1658-1665`
  refuses a `token.env` stages can also read, but only when `cg.MCP != ""`. Dropping that condition
  checks every `credentials:` entry, keeping the CFG-009/SEC-010 diagnostic shape at `:1654-1657` (§4.6).
- **P0-c — tier 3 has no registry scrubber, and activity arguments are never scrubbed.**
  `bootstrap.EngineDeps` carries no `Scrubber` field (`internal/bootstrap/worker.go:21-26`) and
  `RegisterEngine` constructs `engine.Activities` without one (`:31-39`), so the fallback is a
  seven-regex pattern net (`internal/engine/activities.go:295-299`, `internal/journal/scrub.go:146-159`)
  matching no opaque vault secret. Only activity *return values* pass through it (`:266-281`);
  `InvokeGoober` and `RunDeterministic` take an envelope (`:185`, `:222`) that reaches Temporal history
  verbatim. Both files exist on `main`, so this lands with the others.
- **P0-d — the integrity floor.** The runner overwrites whatever grade an executor set
  (`internal/runner/run.go:2754`) with `producedIntegrity`, which returns `IntegrityTrusted` for an empty
  grade list (`internal/runner/inputsfrom.go:352-354`). Provider-builtin stages already record
  `unapproved` result artifacts (`internal/executor/shell.go:952-1007`) and finish `trusted`; `ci-poll`
  (`internal/executor/cipoll.go:407`) and external-telemetry do the same. Specified at §6.4.

Three fixes ship **inside** the call-out change rather than ahead of it, because it is built directly on
the code they touch: the external-telemetry executor's nil `Progress` wiring
(`cmd/goobers/runnerwiring.go:887-889` against `internal/externaltelemetry/host.go:24`, `:81-83`, so
today's one shipped outbound-call stage emits no heartbeats at all); an explicit `Proxy` on the transport
(§9.4); and the `ExternalRefs` channel (D13).

**Four landings gate the tier-3 binding**, not the change as a whole: the spike's worker runtime-wiring
slice (`cmd/goobers/workerwiring.go` exists only on `spike/goobernetes`, verifiably not an ancestor of
`main`), owned by **#2904**; **P0-c**; the executor-artifact adoption channel
(`spike internal/workerhost/artifacts.go:41-83`; `main/internal/workerhost/` has no `artifacts.go`); and
**the `TimeoutType()` split (D7)**, because under a mutating contract collapsing `START_TO_CLOSE` into
the retryable infra class is a duplicate-submit generator. That last is a prerequisite, not a follow-up.
## 2. The DSL surface

A call-out is the first stage kind whose *presence in a workflow* is a security fact. A reviewer, an
operator and the preview gate all need to answer "does this document reach outside the instance, and may
it change something there?" from the document's static text.

### 2.1 What exists today

A stage kind is a magic string in an open map: `Task.Inputs` is `map[string]string`
(`api/v1alpha1/workflow_types.go:166`) and the dispatcher reads `env.Inputs["kind"]`, defaulting to
`shell` (`internal/executor/dispatch.go:107-117`). Four consequences are load-bearing.

**The compiler has no vocabulary for kinds** — it ships `WithKnownChecks` and `WithKnownHarnesses`
(`internal/workflow/compile.go:99`, `:107`) but no equivalent, and the only kind-aware compile rules in
the tree are two hardcoded literals (`internal/workflow/v_next/compile.go:455`, `:462`).

**Kind selection is runtime-overridable.** `dispatchTask` overlays every `inputsFrom` key onto
`env.Inputs` before dispatch (`internal/runner/run.go:3071-3088`, writes at `:3074`, `:3087`), with no
exclusion for `kind`; the engine does the same (`internal/engine/engine.go:459-468`). Feature attribution
meanwhile reads only the static literal, in a switch with **no `default:` arm**
(`internal/workflow/v_next/features.go:933-940`), so an unrecognised kind contributes no feature and
never meets the preview gate. The codebase routes around the hole rather than closing it —
`CheckStageTimeoutCoherence` bails outright when the kind is dynamic
(`internal/workflow/v_next/timeoutcoherence.go:20-22`). The same hole exists one level down and is
shipped: `external-telemetry` reads its connector handle out of `env.Inputs`
(`internal/executor/externaltelemetry.go:95`, const at `:24`) with nothing in `internal/workflow`
restricting the key, so `inputsFrom: {connector: …}` re-points a shipped outbound-network stage at
runtime. This design must not add a second instance of that class.

**Every non-shell kind must declare a decorative command it never runs**, because the schema requires
`oneOf: [{required:[command]},{required:[script]}]` (`api/schemas/workflow.schema.json:212-215`),
mirrored by CEL (`api/v1alpha1/workflow_types.go:329`) and re-asserted at the engine registry boundary
(`internal/engine/registry.go:86`); neither `goobers ci-poll` nor `goobers external-telemetry` is a real
subcommand. **And a new kind costs nothing to add and therefore gets nothing** — commit `84369f6c` added
external-telemetry across 42 files touching none of the type, schema, CRD or deepcopy files, which is
also the path with no schema validation, no `goobers explain` surface and no CRD contract at tier 3.

### 2.2 A typed `run.kind` and a `run.externalCall` block

**D1.** `run.kind` is a new optional field on `DeterministicRun` (`api/v1alpha1/workflow_types.go:331`),
mutually exclusive with `command` and `script`; the two-way exclusivity becomes three-way in the schema
`oneOf`, the CEL rule and `internal/engine/registry.go:86`. `ci-poll` and `external-telemetry` gain
`run.kind` spellings, retiring the decorative-command wart rather than adding a fourth instance of it.
**`external-call` is never registered as an `inputs.kind` value**, so the dynamic path cannot select it.

**The call declaration rides `DeterministicRun`, not the task and not `Inputs`.** This is forced by the
seam: `invoke.Deterministic.Run(ctx, env, run)` (`internal/invoke/invoke.go:66-68`, byte-identical to
`executor.KindExecutor`, `internal/executor/dispatch.go:28-30`) takes two values and `apiv1.Task` is
neither, on either tier (`internal/runner/run.go:3105`; `internal/engine/engine.go:493` →
`internal/engine/activities.go:221`, `:255`) — so a task-level block has no transport at all. `env.Inputs`
is disqualified outright (§2.1). `DeterministicRun` is the author-declared, compile-time, CRD-published
description of what a stage runs; it is `*t.Run` off the compiled machine, and **nothing on either
dispatch path mutates it** (verified: the only non-test writers to a `Run` field are
`internal/instance/guided.go:684-685` and `internal/instance/gagglecapability.go:54-55`, both rewriting a
definition during config authoring).

```yaml
- name: request-release-build
  type: deterministic
  run:
    kind: external-call
    workspace: scratch
    externalCall:                 # inside run:, not a task-level sibling
      target: build-oracle        # instance.yaml handle; NEVER a URL
      operation: request-build    # host-declared operation (§4.1)
      mode: submit-poll           # request | submit-poll
      responses:                  # required, non-empty, closed, duplicate-free
        - {field: outcome, output: buildOutcome, type: string}
  inputs: {timeout: 900s}         # bounded-wait budget; NOT the kind selector
  inputsFrom: {ref: headSha}      # may still change what the call ASKS
  capabilities: [callout:invoke@build-oracle]
  timeoutSeconds: 1200
  next: assess
```

Request parameters still ride the open scalar map and may still be threaded by `inputsFrom` — the
overlay can change what the call *asks*. What it can no longer change is *what executes*, *which target
is reached*, *which operation runs*, or *which fields project onto which outputs*. **The workflow also
cannot name an HTTP method**: `externalCall` has `target`, `operation`, `mode` and `responses` and
nothing else, and methods exist only in `instance.yaml` (§4.2).

The properties compiled at every root are §2.4's closed enumeration XC-A1…XC-A7; the substance is here.
**`target`** is a name matching the shipped handle pattern `^[a-z][a-z0-9-]{0,62}$` — **shape only, never
existence** (§2.4). **`operation`** names a host-declared operation, but neither its existence nor the
projection's agreement with its declared response is a compile rule of the language: both need operator
config and are therefore a tier-1 daemon cross-reference diagnostic plus a runtime fail-closed (§2.4).
**`responses`** is the only path from the far side into stage outputs (D14): one field, one output, one
of `string|number|boolean` per entry, duplicates rejected on either side, and **`output` may not name a
reserved key**. That set is a **build-time constant, not an injected option** — the seven well-known input
keys the registered automated checks read (`status`, `errorCode`, `errorMessage`, `errorRetryable`,
`ciStatus`, `landOutcome`, `queueOutcome`), lifted into the stdlib-only leaf package `internal/gatekeys`
so both interpreters can import them without the cycle that forced the shadow tables (§6.4 owns the
derivation and its drift gate). Because it is a constant the rule is written with no option and no nil
guard, so it holds at every compile root rather than wherever a composition root remembered to wire it.
**`capabilities`** carries the grant **biconditionally** (XC-A4): a task declares a `callout:invoke@…`
grant iff its `run.kind` is `external-call`, and when it does, exactly one, whose suffix is
`externalCall.target`. That is a two-field cross-check inside one document, needing no registry, which is
why §2.4 can claim it globally, and it subsumes §4.5's kind restriction. **Timeouts** reuse the
existing bounded-wait input vocabulary (`internal/boundedwait/boundedwait.go:12-13`) so
`CheckStageTimeoutCoherence` needs a vocabulary extension rather than a new mechanism, with both
interpreter copies gaining an `external-call` arm reading `task.Run.Kind` in preference to
`task.Inputs["kind"]`; a stage must declare `timeoutSeconds`, since the tier-3 fallback is
`activityTimeout = 1h` (`internal/engine/engine.go:41-44`). **`run.workspace: scratch`** is required —
every attempt provisions a workspace before dispatch on both tiers and a call-out needs no repository.
**`run.network: none` is refused**, because it is silently meaningless for an in-process kind (§4.7).
One further rule is owned by the security analysis: an agentic stage selecting a call-out response
artifact through `contextFrom` must declare `minimumIntegrity` (§9.6 records what that does not cover).

### 2.3 Closing dynamic kind selection

A typed `run.kind` is immune to the overlay by construction. The vocabulary as a whole is closed in the
same change, on three fronts.

**The kind vocabulary becomes an unconditional constant, not a wired option.** Unlike targets, kinds are
build-time constants: `KindRegistry.Register` is called exactly three times with three literals,
unconditionally (`cmd/goobers/runnerwiring.go:2153-2179`), and `NewCIPollKindExecutor` accepts a nil
executor precisely so registration stays unconditional (`internal/executor/dispatch.go:123-128`). So
`internal/boundedwait` becomes the single canonical vocabulary — retiring four duplicates across
`internal/executor`, both interpreters and `internal/dslmigrate` — and both interpreters check
`task.Run.Kind` against it with **no option and no nil guard**, above the `goobers == nil` early return
at `v_next:472-474`. Drift against the runtime registry is caught by a parity test in the
`TestSchemaEnumsMatchGoConsts` shape rather than by whether a composition root remembered an option:
less machinery than a `WithKnownKinds` option, and unlike it, root-independent.

**A shared vocabulary is not a shared feature: the frozen interpreter refuses `external-call`
outright.** Membership in one constant list makes a kind *spellable* on both interpreters; it must not
make this one *usable* on the frozen one, because none of the guards that make a call-out safe live in
`v_current` — not XC-A4's grant biconditional, not XC-A5's reserved-output rule, not D2's `inputsFrom`
error (a warning there, §2.3). A 1.4 document reaching dispatch would therefore obtain a target's bearer
token on an unguarded stage and select an executor from an overlaid map. So the vocabulary constant
carries a per-kind minimum DSL version, and `v_current` rejects `run.kind: external-call` with a
diagnostic naming `2.0` and `goobers fix --to 2.0` — the same shape `DVL020` already uses for a
deprecated pin (`api/validate/validate.go:66-84`). This is a refusal, not documentation: **v_next-only
is an enforced property or it is not a property.** It is also the one place the design deliberately
touches the frozen interpreter, which is contract-preserving under the freeze policy — it refuses a
document that no 1.4 author can have written, since the field does not exist in 1.4.

**The missing `default:` arm** lands in both kind-attribution switches
(`internal/workflow/v_next/features.go:933-940`, `v_current/features.go:856`), so an unrecognised kind is
attributed to a catch-all feature and `goobers features --used` stops lying.

**`inputsFrom` may not bind `kind` — a compile error in v_next, a warning in frozen v_current (D2).**
Four reasons, descending: the v_next/v_current split *is* the deprecate window
`docs/design/dsl-version-lifecycle.md:126-130` requires, obtained free from the two-interpreter
architecture and leaving an author the working escape of pinning 1.4; measured blast radius is zero (no
`.yaml`/`.yml` in the tree binds `kind` this way; ten files use `inputsFrom` at all); there is in-tree
precedent in the *frozen* interpreter, since #124 widened the canonical-capability loop to cover
deterministic tasks and its own comment records that documents which previously compiled stopped
compiling, with no version bump, as a correction to a shared defect
(`internal/workflow/v_current/compile.go:511-512`); and no honest use remains, the one thing the binding
uniquely buys being the timeout-coherence exemption. It is **not** put in the schema —
`workflow.schema.json` is version-blind, one file for every `dslVersion`, so a `propertyNames` ban there
would refuse 1.4 documents too. Two costs recorded rather than papered over: there is no `goobers fix`
edge (`internal/dslmigrate/migrate.go:33-38` holds exactly one and this change adds no version), so the
diagnostic carries the remedy inline; and `internal/engine/engine.go:204-207` re-compiles inside the
workflow function on every replay, so a document refused after the upgrade fails an **in-flight** run —
drain or confirm before rolling the worker.

### 2.4 Admission is a per-root property, not a language guarantee

Rev 1 claimed "a stage naming an unconfigured target fails compilation". That overstates what the
pattern it copies delivers, in exactly the way that pattern's author was careful not to.
`WithKnownChecks`/`WithKnownHarnesses` do not fail closed — `compileConfig` carries a `*Set` boolean
beside each value and forwards each option only when set (`internal/workflow/compile.go:76-84`,
`:146-178`), the interpreter checks are nil-guarded (`v_next/compile.go:505`, `:658`), and the doc
comment states the intent: "nil performs no such check (the default)" (`:640-644`). A second gate
compounds it: `admissionProblems` bails with `if goobers == nil { return problems }` (`v_next:472-474`)
*above* the canonical-capability loop at `:525-529`, so any root passing no goober map performs no
capability-name validation at all.

Of six admission surfaces (identical on `main` and the spike), only the tier-1 daemon holds operator
config at all. `api/validate` structurally cannot: it lints a config-as-code directory and imports no
instance type (`api/validate/validate.go:1-5`), and `CheckWorkflowAdmission` hardcodes
`admissionProblems(def, goobers, nil, false)` (`internal/workflow/v_next/checks.go:111-113`). The
read-model inventory and `goobers workflow show` could with plumbing but **do not** in v1 — a false pass
there renders a graph, never runs a stage. The tier-3 pair cannot and must not: `Registry.Compile` passes
only `WithPreviewFeatures` (`internal/engine/registry.go:97-100`) inside a scheduler process that reads no
`instance.yaml` (`cmd/scheduler/main.go:70-88`), and `internal/engine/engine.go:204-207` compiles
**inside the Temporal workflow function**, where a result depending on live operator config would be
non-determinism on the one path where non-determinism corrupts a durable run.

**The sorting principle, stated once.** A compile rule this design claims globally must be decidable from
the workflow document plus build-time constants alone. Such a rule is written with **no option and no nil
guard**, in `admissionProblems` above its `if goobers == nil { return problems }` early return
(`internal/workflow/v_next/compile.go:472-474`), so it runs at every `workflow.Compile` root and from
`CheckWorkflowAdmission` (`checks.go:111-113`), which is how `api/validate` reaches it
(`api/validate/validate.go:1722`). **A rule that needs operator config is not a compile rule of the
language at all**: it is a tier-1 daemon cross-reference diagnostic, and its authoritative enforcement is
the runtime resolution in the kind executor — where the shipped precedent already puts it
(`internal/externaltelemetry/host.go:41-48` returns a failed artifact and an error for an absent
connector), costing no new machinery and true on both tiers. An option-gated compile rule is never a third
category: the pattern is skip-when-unset by construction, and inverting one option to fail closed would
make five of the six surfaces refuse every call-out.

**XC-A1…XC-A7 are the complete set of rules this document claims globally; nothing else is.**

| # | Rule | Decidable from |
| --- | --- | --- |
| **XC-A1** | `run.externalCall` is present **iff** `run.kind == external-call`; `run.kind` is a member of the compiled-in `internal/boundedwait` kind vocabulary (§2.3) | document + build constant |
| **XC-A1b** | `run.kind` satisfies the vocabulary's per-kind minimum DSL version — so `external-call` is **refused on the frozen interpreter** with a diagnostic naming `2.0`. Membership makes a kind spellable on both; this rule makes it usable only where its guards exist (§2.3) | document + build constant |
| **XC-A2** | `externalCall.target` matches `^[a-z][a-z0-9-]{0,62}$` — **shape only; existence is not checked here** | document + literal |
| **XC-A3** | `responses` is non-empty; each `type` is one of `string\|number\|boolean`; no duplicate `field`; no duplicate `output` | document |
| **XC-A4** | **Grant biconditional.** A task declares a `callout:invoke@…` grant **iff** its `run.kind` is `external-call`; when it does, exactly one such grant is present and its suffix equals `externalCall.target` | document |
| **XC-A5** | No `responses[].output` names a key in `gatekeys.Reserved()` | document + build constant |
| **XC-A6** | `run.workspace` is `scratch`; `run.network` is unset; `timeoutSeconds` is non-zero | document |
| **XC-A7** | An agentic stage whose `contextFrom` selects a call-out stage in the same document declares `minimumIntegrity` (§9.6 owns its residual) | document |

The **preview gate** stays root-independent by a separate mechanism, unchanged: `CheckWorkflowFeatureSupport`
takes no option object and is called from `v_next.Compile` (`:183`) and `api/validate/validate.go:1580`.

**Explicitly not global, because each needs operator config:** target existence; `operation` existence;
and the projection's field/type agreement with the operation's declared response (`poll.response` for
`submit-poll`, `response` for `request`). All three are a **tier-1 daemon cross-reference diagnostic plus
a runtime fail-closed**, and there is no `WithKnownTargets` compile option. The check runs one frame above
the compile roots, where the config already is: a pass over the returned `machines` at
`cmd/goobers/daemon.go:568-573`, with `cfg` in scope (already read at `:569`). The obvious alternative —
wiring a target list into `compiledMachinesWithWarnings` (`cmd/goobers/runnerwiring.go:2612`) — does not
have the data: that root receives `*instance.ConfigSet`, whose fields are Manifest/Gaggles/Goobers/Workflows
and three source maps (`internal/instance/configdir.go:40-49`), no `*instance.Config`, and neither does its
caller `compiledMachinesWithGooberDigestsAndWarnings` (`cmd/goobers/gooberidentity.go:145-153`). Threading a
name list down would change two signatures and still could not carry operation response shapes. Doing it at
`daemon.go` adds no compiler option and no import edge (`internal/workflow` and `internal/instance` import
each other in neither direction, verified), and checks target, operation and projection in one place.

State this in operator-facing docs, because it is the surface agents and CI actually run: **`goobers
validate` validates the *shape* of a call-out and deliberately not its *existence*.** XC-A1…XC-A7 hold
there; XC-A2's target may still name nothing. A workflow naming an unconfigured target passes `goobers
validate`, fails the daemon's cross-reference pass, and fails closed at dispatch on either tier.

### 2.5 Versioning: DSL 2.0 as a preview feature

**D3.** The reflex to bump is understandable — `dsl-version-lifecycle.md:119-122` names MINOR for "New
stage/gate/trigger kinds" — but the code prices a minor far above that line: a whole copied interpreter
(`v_current` 8,459 lines, `v_next` 9,648; 3,279 and 4,362 non-test) selected by a literal two-arm switch
(`internal/workflow/compile.go:207-214`), a hand-written migration edge whose comment says a future bump
registers its own, and a support commitment enforced at package init
(`internal/supportmatrix/supportpolicy.go:10-18`, `supportmatrix.go:76`). 2.0 costs authors nothing: it
is `supported`, 1.4 is `deprecated` with `UnsupportedAfter: "v0.2.0"`, and every shipped workflow already
pins 2.0. Version scope is **v_next only** for the new kind, departing from external-telemetry's
precedent of adding a byte-identical rule to both; the dynamic-kind fixes of §2.3 land in both, because
they are corrections to a shared defect rather than new language.

Preview is a hard compile refusal, not documentation (`internal/workflow/v_next/compile.go:73-92`,
`:142-150`, `:183`), opted into by the instance annotation `goobers.dev/allow-preview-features` and
surfaced at load as `VER002`. Two feature IDs land — `stage.external-call` and `stage.run.kind` — in the
`previewFeatures` map whose doc comment already records the promotion protocol (`features.go:655`). That
gate is **only reachable once the kind is statically determinable**, which is why §2.3 is not a
follow-up: a dynamically-selected kind contributes no feature, and a stage contributing no feature is
never checked against the policy.

**D4** avoids the feature-router asymmetry rather than depending on a fix. `FeaturesForWorkflow`
(`internal/workflow/features.go:88`) routes by interpreter but `FeaturesForGaggle`, `FeaturesForGoober`,
`CheckFeatureSupport`, `FeaturesAtDSLVersion` and `NewFeatureRegistry` call `vcurrent.*` unconditionally
(`:97`, `:102`, `:107`). That breaks neither the compile gate nor the generated matrix; it breaks
introspection, since `goobers features --used` resolves goober features through the pinned path
(`cmd/goobers/features.go:205`). Every artefact here is Task-scoped, so nothing reaches
`FeaturesForGoober`. Routing all five through `interpreterForDefinition` is filed separately; it is a
hard prerequisite for any later goober- or gaggle-scoped call-out surface, and no section proposes one.

### 2.6 Lockstep checklist

Everything below changes in one commit. **Bold** rows are ungated — "CI is green" is not evidence for
any of them.

| # | Artefact | Change | Gate |
| --- | --- | --- | --- |
| 1 | `api/v1alpha1/workflow_types.go` (`:331`) | `Kind StageKind`; `ExternalCall *ExternalCall` + types; three-way CEL replacing `:329`; a second CEL asserting `externalCall` iff `kind == external-call` | compile; `manifests-check` |
| 2 | **`api/v1alpha1/envelope.go`** | `InvocationEnvelope.CallEpoch`, `.CallReconcileOnly`; `ResultEnvelope.ExternalRefs`; `CalloutOutcomeUnknownErrorCode`; `StageContractVersion` `v1alpha8` → `v1alpha9` (`:24`) | compile for the fields; **the bump is ungated** — nothing pins the string; its only non-doc reader is `cmd/goobers/implementcontext.go:187` |
| 3 | **`zz_generated.deepcopy.go`** | scalars covered by `*out = *in` (`:208`); `ExternalCall` and its slices need generated deep-copy | **none — no regenerate-then-diff for `object`** (`Makefile:77-79`) |
| 4 | `config/crd/bases/goobers.dev_workflows.yaml` | regenerated | `manifests-check`; `TestCRDManifestsExposeEveryTypeField` (`api/v1alpha1/crd_completeness_test.go:35`) |
| 5 | `api/schemas/workflow.schema.json` | `run.kind` enum; closed `run.externalCall` `$defs`; three-way `oneOf` (`:212-215`); capability `anyOf` | load-time validation; `TestSchemaEnumsMatchGoConsts` |
| 6 | `api/schemas/{invocation,result}.schema.json` | the three new properties (roots are `additionalProperties:false`) | `TestSchemaBackedEnvelopeCompleteness` (`api/validate/envelope_completeness_test.go:22`) — self-gating, fails loudly |
| 7 | **`api/schemas/instance.schema.json`** | `externalCallTargets`; capability `anyOf` | **none at runtime** — referenced by no non-test Go code; the real gate is `(*Config).Validate()` (row 13) |
| 8 | every new schema property | a `description` on each node incl. nested `$defs` | `TestDescriptionCoverage`; these roots are author-facing, so 100% required (`api/schemas/description_coverage_test.go:26-32`) |
| 9 | `v_current/schema_enum_registry_test.go` | classify the new enums; **repoint all five capability `path` strings** (`:99-103`) to the `anyOf/0/enum` position — `walkEnums` indexes arrays positionally | `TestSchemaEnumsMatchGoConsts`, `TestEverySchemaEnumIsClassified` (fails on stale *and* unclassified) |
| 10 | **`internal/capability/capability.go`** | `callout:invoke` const; entry in the hand-written 22-entry `All()` (`:139-146`); `Base()` splitting on the first `@`; scope-aware `Known`/`StageDeclarable` (`:149-163`) — 16 non-test call sites inherit it | **none directly** — no test asserts `All()` is complete; an omission surfaces only at row 9 |
| 11 | `internal/boundedwait`; **`internal/gatekeys` (new)** | `boundedwait`: `type StageKind string` + the four kinds as canonical vocabulary (an explicit type is required by row 9's const extractor). `gatekeys`: a stdlib-only leaf importing nothing in-tree, holding the seven reserved gate input keys and `Reserved()`; `internal/gate/automated.go:36-39` re-declares its four `InputKey*` consts as aliases (untyped string consts, so no call site changes) and the three literals at `:278`, `:291`, `:308` become `gatekeys.*` | new parity test; the AST drift gate (§6.4) |
| 12 | `internal/workflow/{v_next,v_current,compile.go}` | XC-A1…XC-A7, feature IDs, timeout coherence. **No new compile option**: `WithKnownTargets` and `WithReservedOutputKeys` are both deleted before they are written (§2.4) | interpreter tests; `TestFrozenGoldenChangeRequiresAcknowledgedPatch` if a golden moves |
| 13 | **`internal/instance/config.go`** | `externalCallTargets` + `validateExternalCallTargets()` from `(*Config).Validate()` (`:1354`); the D-C2/D-C3 credential rules; accept the scoped capability at `:1630-1640` | **none — a runtime load failure.** Add load tests |
| 14 | **`cmd/goobers/{runnerwiring,daemon}.go`** | fourth `kinds.Register` (`runnerwiring.go:2153-2179`); the target/operation/projection cross-reference pass over the returned `machines` at `daemon.go:568-573`, with `cfg` already in scope at `:569` (§2.4); `credentialGrantKey` (`:625-635`) learns the scoped form; **fix the nil `Progress` at `:887-889`** | **none for either** — a runtime load failure, and a bug invisible until a slow far side |
| 15 | `internal/{calloutepoch,callout}/` (new) | `calloutepoch`: `Derive(runID, branch, stage, entry)` plus the run-scoped `Ledger` (`Seed`, `Entry`, mutex-guarded `map[key]int` keyed by `(branch, stage)`). `callout`: the host, the `KindExecutor`, the unexported `calloutErrorCode` vocabulary and the `calloutFailure` constructor that sets `Retryable: false` unconditionally (§3.6) | new unit tests |
| 16 | `internal/runner/{run,resume,parallel_run}.go`, `internal/engine/{engine,journal,retry}.go` | epoch minting **inside `runTask`** (`run.go:2531`), with the ledger threaded exactly as `stepBudget` already is (`run.go:1027` → `parallel_run.go:172` → `:273` → `:452`) so the concurrent branch worker (`parallel_run.go:586`) mints too; one seeding site (`resume.go:690`, `rerun.go:188`); journaling; the integrity floor; `attemptFailureClass(t, err) (class, reconcileNext, error)` with the `dispatch func(ctx, bool)` closure param at `engine.go:476`/`:494`; the tier-1 `reconcileFirstAttempt` bool at `run.go:1340-1347` | cross-runner epoch-parity test; the three §3.3 ledger tests; dual-runner conformance |
| 17 | `internal/engine/{registry,activities}.go` | `shapeProblems` three-way mirror (`:86`); the runtime fail-closed becomes "neither kind nor command/script" (`:227-229`) | tier-3 registration and activity tests |
| 18 | `docs/feature-matrix.md`, introspection goldens | regenerated | `TestFeatureMatrixDocUpToDate`, `TestFeatureMatrixCoversEveryFeature` (`cmd/goobers/featuresdoc_test.go:13`, `:37`), `TestFeaturesJSONContract` |
| 19 | **`docs/stage-contract.md:5,791,802`; `skills/goobers-dsl-author/references/dsl-reference.md`** | the version string and the new fields | **none — no drift gate references either file's content** |

`portal/src/api/*.generated.ts` is **not** a row, verified: `internal/apicontract/wiretypes.go` declares
no task types and `internal/readservice` never reads `Task.Run`, so `make generate` should produce an
empty portal diff — if it does not, something unintended reached the read model.
`docs/provider-capability-matrix.md` does not move either: it is generated from
`providers.AllCapabilities()`, a different provider-facing enum. Nine of nineteen rows are ungated or
weakly gated (§9.1).

## 3. Execution contract and semantics

A call-out is a deterministic stage kind, not a new execution path: the attempt loop, journal writes,
artifact recording, integrity grading and gate branching apply unchanged. What this design adds *inside*
the seam are five invariants the shipped kinds do not all hold — a self-enforced deadline, a mandatory
progress hook, a declared response projection, a deterministic call epoch, and a bounded return leg.

### 3.1 The seam

`KindRegistry.Register` rejects an empty kind, a nil executor and a duplicate
(`internal/executor/dispatch.go:44-62`). **The call-out is a fourth `Register` call in the daemon's
per-run `NewDeterministic` closure (`cmd/goobers/runnerwiring.go:2102`, `:2153-2179`) and nothing more on
the dispatch side.** Credentials are bound at construction, never carried on the wire —
`InvocationEnvelope` has no credential field (`api/v1alpha1/envelope.go:43-123`).

Routing is the one part this design changes, with explicit precedence:

```go
kind := string(run.Kind)                               // typed field wins; unreachable from inputsFrom
if kind == "" { kind = stringInput(env, InputKind) }   // legacy path, ci-poll/telemetry only
if kind == "" { kind = KindShell }
```

`external-call` fails closed if selected any other way: the compiler and closed schema enforce that
`externalCall` exists iff the kind does, so an `inputs.kind: external-call` arrives with a nil
`ExternalCall` and the executor refuses. That is the natural fail-closed, not a bolted-on guard — there
is no representable way to reach the network without the typed declaration. The executor's first act
before any network I/O is the runtime capability re-check against
`callout:invoke@<run.ExternalCall.Target>`, mirroring `ci-poll` (`internal/executor/dispatch.go:130-134`)
and external-telemetry (`internal/executor/externaltelemetry.go:90-94`) — and unlike both precedents,
**both operands are `inputsFrom`-immune**: `env.Capabilities` is set once in `buildEnvelope`
(`internal/runner/run.go:3907`, called at `:2993`) and `buildInvocation`
(`internal/engine/engine.go:598`) and never written afterwards, and `run` is the compiled value. So it is
a genuine invariant rather than a check over mutable data.

Two consequences land in the same change: the tier-3 activity's refusal of a run block declaring neither
command nor script (`internal/engine/activities.go:226-230`) is relaxed to "neither a kind nor a
command/script", preserving the property rather than dropping it; and the stage declares
`run.workspace: scratch`.

**One registration serves both tiers — once tier 3 can dispatch at all.** `RunDeterministic` calls
`a.Det.Run` after provisioning a workspace (`internal/engine/activities.go:256`), but on `main` that seam
is nil (`cmd/goobers/worker.go:143-149`) so every deterministic stage fails closed. The spike closes
exactly that hole in the shape this design needs: `workerDet.Run` resolves the gaggle's seams and calls
`g.cfg.NewDeterministic` (`spike cmd/goobers/workerwiring.go:206-225`) from the *same* `buildRunnerConfig`
the daemon uses (`:121-136`). "Works on tier 3" is therefore a dependency, not a design choice, and its
four components are §1.6's gate.

### 3.2 Two call shapes; the `operations` block

| Shape | Picked when | Executor's loop | Far side must |
| --- | --- | --- | --- |
| **`mode: request`** | The answer arrives in seconds to minutes and there is no job resource | One request, one bounded wait | Answer within the budget; tolerate a repeated identical request |
| **`mode: submit-poll`** | Work takes minutes, or the far side already exposes a job/status resource | Submit, then poll with capped backoff until terminal or budget exhausted, reporting progress each tick | Honour the epoch; expose an **epoch-addressable** status read; TTL-expire abandoned work (§3.4) |

Submit-then-poll is modelled directly on `ci-poll`, the shipped implementation of this exact loop
(`internal/executor/cipoll.go:297-390`, `:623-634`), which the call-out reuses rather than reinventing.

Each operation previously declared exactly one `method` and one `path`, so the submit leg had no
representation in the schema of record. The block becomes discriminated:

```yaml
operations:
  request-build:
    mode: submit-poll
    effect: mutating                    # read-only | mutating; default read-only
    idempotency:
      header: Idempotency-Key           # where the epoch is sent on the submit
      honoured: true                    # the OPERATOR's assertion that the far side dedups
      retention: 24h                    # receiver's minimum epoch -> outcome retention
      lease: 2h                         # receiver's TTL for abandoned work
    submit:
      method: POST
      path: /builds
      params: {ref: string, profile: string}
      response: {handle: string}        # optional; never the only way to poll
    poll:
      method: GET
      path: /builds/by-epoch/{epoch}    # MUST be addressable by {epoch} alone
      terminal:
        field: state
        committed: [succeeded, failed]
        pending: [queued, running]
        absent: [not_found]
        expired: [expired]
      response: {buildId: string, outcome: string}
```

Rules the shape carries: `mode` is closed and the leg keys are required-and-forbidden accordingly, under
`additionalProperties: false` throughout; `effect: mutating` requires `mode: submit-poll` and a complete
`idempotency` block, because a mutating bounded request/response has nowhere to put a reconcile read; a
mutating `poll.path` must contain `{epoch}` (`{handle}` is permitted *in addition*, never instead); both
legs' paths are checked against the target's path prefix at build time and again at dispatch, by the
private `http.RoundTripper` shape §4.1 specifies; and the DSL projection is type-checked against
`poll.response` for `submit-poll` and `response` for `request`.

**The poll leg addresses the far side by our epoch, not by their id.** A far-side-returned identifier is
unapproved content, and interpolating it into the next request's URL is letting unapproved content choose
a destination. Where a protocol genuinely requires echoing a server-assigned id, it must be a typed,
pattern-constrained field on the operator-owned operation declaration, validated against that pattern
before use — the pattern is the enforcement point, and without it this design does not get to claim the
poll URL is safe.

### 3.3 The call epoch

**D6.** A call epoch is one opaque deterministic string identifying one *intended call* — not one
attempt, not one request.

```
epoch = "gce1:" + hex(sha256(runID ‖ 0x1F ‖ dec(branch) ‖ 0x1F ‖ stage ‖ 0x1F ‖ dec(entry)))
```

`runID` is `InvocationEnvelope.RunID` (`api/v1alpha1/envelope.go:47-49`). `branch` is the parallel branch
ordinal, already conformance-normative as `journal.Event.Branch` (`internal/journal/conformance.go:106`),
already threaded to `runTask` (`internal/runner/run.go:2531`) and already stamped on the stage span
(`:2630`), so it costs one integer and no plumbing. **It is not what makes the epoch collision-free, and
this document does not claim it is.** Fan-out rule 1 — disjointness — refuses any state reachable from two
branch bodies, checked against an ownership map built across *every* parallel in the machine
(`internal/workflow/v_next/parallel.go:399-407`, map at `:247-259`), gated by
`TestParallelRejectsSharedBranchState` (`v_next/parallel_test.go:193-198`); and DSL 1.4 has no fan-out
construct and rejects a `parallels:` block (`v_current/compile.go:282-283`), so `v_next` is the only
compiler that emits a parallel and it always runs rule 1. `(runID, stage, entry)` is already unique per
branch today. The component is kept because rule 1 is a **compile** rule with no runtime backstop: relax
it, or construct a `*workflow.Machine` by some future path that skips `v_next` validation, and a
stage-only epoch would start silently deduping two intended calls into one far-side no-op — the worst
failure mode this design has. Branch ids are themselves deterministic: assigned by declaration order from
1 in `newParallelExec` (`internal/runner/parallel.go:58-71`, whose own comment states the reproducibility
requirement), never from launch order, and rebuilt from the compiled spec on resume
(`internal/runner/resume.go:1071-1101`). `stage` is the task name. `entry` is the
dispatch ordinal of this stage within this run and branch. The digest rather than a readable composite,
because it is fixed-length and therefore safe in a header or path segment without escaping, it discloses
no stage names across the organizational boundary (keeping disclosure to the one decision §6.6 makes),
and it is trivially comparable; the four components are journaled in the clear beside it, so the value
stays auditable locally. The derivation lives in **one exported function in one package both tiers
import**, a sibling of `internal/boundedwait`, which exists for exactly this purpose — so "both runners
derive it identically" is true by construction for the function, and what remains is that both walks feed
it the same `entry`.

**Minted in the walk, never in the executor and never in an activity.** Verified: `internal/engine`'s
non-test files contain no `workflow.SideEffect` and no `workflow.GetVersion`, so any activity-minted
value is fresh on every re-dispatch. On tier 3 an `entries := map[string]int{}` joins `RunWorkflow`'s
per-run mutable state beside `gateAttempts` (`internal/engine/engine.go:257-264`), incremented at the
task arm (`:281-282`) and passed into `runTask` (`:428`), which sets it on the envelope built at `:438` —
outside `dispatchWithRetry`'s attempt loop, so it is structurally constant across it, and Temporal
replays the walk and rebuilds the map identically.

**Tier 1 has three dispatch sites, not one, so the mint goes inside `runTask`.** The sequential walk
reaches `runTask` at `internal/runner/run.go:1364` (task arm from `:1279`), and a *concurrent* parallel
runs a second walk entirely — `runParallelBranch` (`internal/runner/parallel_run.go:439`), one goroutine
per branch launched at `:270`, with its own loop, its own gate evaluator (`:473-479`), its own attempt
bookkeeping (`:498-544`), its own journal wrapper (`:465-471`) and its own `runTask` call at `:586`. A
call-out is *legal* there — `validateConcurrentParallelWorkspaces` admits `scratch`
(`parallel_run.go:129-132`) and §2.2 requires it — so a concurrent branch is a natural home for a
call-out, not a corner. Minting at the walk arms would therefore leave a branch call-out with **no epoch
at all**. `runTask` is called exactly once per intended dispatch on every arm (its attempt loop is
internal, `run.go:2608`), already holds `in.RunID`, `t`, `branch` and `startAttempt` (`:2531`), and
already owns both the `stage.started` append that journals the epoch (`:2631`) and the `dispatchTask`
call (`:2646`) that builds the envelope (`:2993`) — so `env.CallEpoch = epoch` is one assignment and
`buildEnvelope`'s signature (`:3878`) does not change.

The one thing `runTask` cannot own is the counter, which must outlive it. It gets a **run-scoped
`calloutepoch.Ledger` threaded exactly as `stepBudget *atomic.Int64` already is** (§2.6 row 16), so there
is no new plumbing decision to make and the shape is already gated in-tree by
`TestRunnerConcurrentBranchesShareRunStepBudget` (`internal/runner/parallel_test.go:925`).
`Entry(branch, stage, continuing)` returns `n[k]` unchanged when `continuing` and `n[k] > 0`, otherwise
`n[k]++`; a fresh run starts empty, so a first entry is `1`. **One shared ledger, not a map per branch
worker**: a per-worker map restarts at 1, and rule 1 does not forbid two *different* parallels' branches
carrying the same id, so parallel A branch 1 and parallel B branch 1 would each mint `entry=1` for their
own stages. **Concurrency does not make it nondeterministic**: keys are `(branch, stage)` and by rule 1
each stage name is touched by exactly one branch worker, so two goroutines never contend for a key and no
interleaving can change a value — the mutex buys memory safety only. `continuing` needs no new resume
plumbing, because each site already computes it: the two conditions tested at `run.go:1286` and `:1292` on
the sequential arm (in practice the resume half alone, since `validateRerunTarget` refuses a deterministic
stage, `internal/runner/rerun.go:255-259`, and a call-out is `type: deterministic`); `startAttempt > 1` in
the branch worker (`parallel_run.go:586`, exceeded only via the interrupted-attempt arm at `:540`); and
always false on tier 3, where Temporal replays the walk and there is no continuation concept.
**A replayed stage draws no entry**, automatically, because both replay paths skip `runTask` entirely
(`parallel_run.go:581-584`; `resume.go:584-613`).

**Do not use `steps`**: on tier 1 it is a shared `atomic.Int64` (`run.go:1027`) incremented concurrently
by parallel branches (`parallel_run.go:570`), so it is interleaving-dependent by construction — neither a
deterministic per-stage ordinal nor reproducible on tier 3.

**Tier 3 has no branches at all.** `internal/engine` contains zero occurrences of "Parallel" and a
parallel state reaches `unknown state %q` (`internal/engine/engine.go:353`), so tier 3 always feeds
`branch = 0` and a cross-runner fixture can never contain one. Two consequences, stated rather than
implied: the `branch` component is **tier-1-only by construction** and creates no tier-3 divergence risk,
since a workflow that runs on both tiers has no parallels; and the cross-runner epoch-parity test below
**cannot cover the branch path**, which is gated by tier-1 tests alone.

| Walk event | `entry` | Why |
| --- | --- | --- |
| First entry into the state | `1` | — |
| **Gate repass** | **+1** | A repass is a new intent; `startAttempt` resets to 1 (`internal/runner/run.go:1280`), and a constant epoch would make the intended second call a silent no-op |
| Policy or infra retry | unchanged | Same intent, second attempt (`:2608`; `internal/engine/retry.go:62`) |
| Temporal re-dispatch after worker loss | unchanged | Same walk position, replayed to the same value — the case dedup exists for |
| Tier-1 crash resume | unchanged | The resume path synthesizes the interrupted attempt terminal (code `interrupted`, `:821`) and re-dispatches at `attempt+1` (`:1341`) — same stage entry |
| Operator rerun of a stage | unchanged | `startAttempt = int32(rerun.attempt)` (`:1288`) continues one dispatch's attempt numbering, so a rerun is a retry of one intent. An operator wanting a genuinely new call starts a new run. A decision, not an accident — the alternative lets a human convert a possibly-committed call into a certainly-duplicated one (§10) |

**Resume seeding, tier 1: one site, all three arms.** `calloutEntrySeed(events)` takes, per
`(Event.Branch, Event.Stage)`, the last `stage.started` event's `Runner["calloutEntry"]`, and is called at
the two places that already seed walk-local state from the journal — `internal/runner/resume.go:690` (from
`segment`) and `rerun.go:188` (from `seedEvents`) — with the seeded ledger passed into `walk` alongside
`gateAttempts`; a fresh `Start` (`run.go:683`) passes an empty one. That single site covers the concurrent
branches too, because `branchJournal` stamps `ev.Branch = j.branch` on every append
(`parallel_run.go:27-32`) and `resume.go` seeds from the whole run segment — deliberately *unlike*
`gate.Evaluator`, which is rebuilt per worker from branch-filtered history (`parallel_run.go:477-478`,
filter at `:885-899`), because the evaluator is per-walk state and the ledger is per-run. **Do not copy
`gateRepassSeed`'s shape**: it keys on `e.Gate` alone (`resume.go:1454-1470`), which is safe only because a
repass budget is per gate name, while the epoch seed must key on `(branch, stage)`. The concurrent resume
path needs nothing further — `resume.go` deliberately builds no `resumeContext` for a concurrent pending
parallel (`concurrentParallelResume`, `:426`, guarding `:511` and `:569`) and the branch worker owns
interrupted-attempt recovery itself (`parallel_run.go:501-544`), so the ledger seed plus the worker's own
`startAttempt > 1` is the complete story there. **Tier 3 needs no seeding** — replay rebuilds the map —
and that asymmetry is the design's one real divergence risk.

**Journaling and parity.** On `stage.started`, both tiers append
`Runner: {calloutEpoch, calloutEntry, calloutTarget, calloutOp}`. `Event.Runner` is already
`map[string]any` (`internal/journal/event.go:292-294`) with an unconstrained schema property, so this
costs zero schema artefacts: tier 1 extends the existing append at `internal/runner/run.go:2631` (there
is precedent — `routeRetryDecision` writes `repassAttempt`, `failureCode` and `target` at `:2848-2857`),
and tier 3 gives `runJournal.stageStarted` (`internal/engine/journal.go:239-241`) a `Runner` argument
exactly as `executorError` (`:266-272`) and `mutations` (`:230-235`) already have one. The shipped
precedent for this record shape is the intervention path, which stores `idempotencyKey` and `fingerprint`
in `event.Runner` and refuses a replay whose fingerprint differs (`cmd/goobers/interventions.go:892-941`).
Because `Runner` is excluded from the conformance projection (`projectNormative` copies no `Runner`
field, `internal/journal/conformance.go:104-142`; the schema annotates it `"x-conformance": "excluded"`),
**`TestConformanceDualRunnerJournalParity` does not prove epoch parity and this design must not imply it
does.** What proves it: one shared implementation; a cross-runner test running a fixture *with a repass*
on both runners and asserting the ordered `runner.calloutEpoch` sequence is equal; a tier-1 resume
test asserting a re-dispatch carries the same epoch while a subsequent repass carries `entry+1`; and three
tier-1-only tests for the branch path that fixture structurally cannot reach —
`TestCalloutEpochMintedInEveryConcurrentBranch` (a two-branch concurrent parallel, each branch holding a
call-out against the same target and operation: both `stage.started` events carry a non-empty
`runner.calloutEpoch`, the two differ, and each equals `Derive(runID, branchID, stage, 1)` recomputed in
the test — without the ledger reaching the branch worker the first assertion fails outright),
`TestCalloutEpochStableAcrossConcurrentBranchResume` (drain mid-branch and resume on the pattern of
`TestRunnerResumeConcurrentParallelDoesNotRepeatFinishedStages`, `parallel_test.go:969-1026`: the
re-dispatched stage carries the same epoch and entry, the sibling branch is untouched, an in-branch repass
produces `entry+1`), and `TestCalloutEpochLedgerIsOrderIndependent` (N goroutines on disjoint keys in
shuffled order under `-race`, produced map equal to the sequential result). Rule 1 stays gated where it
already is — cited, not restated. (The
epoch is in the diff by a second route — it is `ExternalRef.ID`, which is normative, §6.3 — so a
divergence surfaces as a `ref.touched` diff.) Making it a first-class normative field was priced and
declined: five artefacts and a normative-surface widening for pure machine state, against zero artefacts
on the `Runner` route.

**It reaches the executor on the envelope, not on `DeterministicRun`**, which is compiled and identical
for every dispatch; stuffing a runtime-minted value into the CRD type would publish a field authors must
never set, and `TestCRDManifestsExposeEveryTypeField` would faithfully publish it as authorable. The
envelope is explicitly excluded from controller-gen (`api/v1alpha1/envelope.go:41-42`), so `CallEpoch
string` costs no deepcopy, CRD or workflow-schema change — only the `invocation.schema.json` property and
the `StageContractVersion` bump the other envelope additions ride too. **The split rule, stated once so
later fields land correctly:** author-declared and statically determinable → `DeterministicRun`;
runner-minted and per-dispatch → `InvocationEnvelope`; `env.Inputs` is not a third option for anything a
stage's behaviour or target depends on.

### 3.4 The receiving-side contract

A target declaring any mutating operation asserts the following. This is a **contract on the receiver**,
not a mechanism Goobers can enforce — nothing in this codebase can verify it at runtime. What the
codebase can do is refuse to dispatch a mutating operation whose target has not asserted it (§4.2).

**Honour the epoch as an idempotency key.** It arrives as `Idempotency-Key: <epoch>`, the header spelling
the repo already uses for its own inbound intervention API (`cmd/goobers/interventions.go:456`). The
first submit carrying `E` creates the unit of work; every later request carrying `E` creates nothing and
returns the first outcome; a later request whose body differs is refused with a stable typed error rather
than silently accepted (the intervention path's fingerprint check is the shipped model, `:929-941`); and
the mapping is retained for at least the declared `retention`, since a dedup window shorter than the
run's own retry horizon is a duplicate-submit generator with extra steps. None of this is novel —
`providers/http.go:113-129` already encodes the same reasoning in-repo, admitting GET/HEAD/PUT/DELETE for
blind retry and excluding POST and PATCH because a write surface "has no transport-level dedup marker to
make a blind retry of those safe". The epoch is exactly that missing marker, supplied by the caller.

**TTL-expire abandoned work.** *Goobers cannot cancel the far side, on either tier, and no reaper can
reach it.* Tier 1: the stall watchdog waits `StalledCancellationGrace = 5s` (`internal/runner/run.go:66`,
applied at `internal/runner/stalled.go:481`) then claims takeover and finalizes the run while the
executor keeps running — `withActiveRunCleanup` deliberately runs the owner in a separate goroutine "so a
watchdog takeover can return even if an invocation ignores cancellation" — with the inactivity threshold
at `DefaultStalledRunTimeout = 45 * time.Minute` (`internal/runcontrol/runcontrol.go:17`). Tier 3:
`stageActivityOptions` sets only `StartToCloseTimeout` and `RetryPolicy{MaximumAttempts: 1}`
(`internal/engine/engine.go:686-695`); no stage activity sets `HeartbeatTimeout` and none calls
`RecordHeartbeat` — the only heartbeating activity in the tree is `ReconcileSchedules`
(`internal/engine/schedule.go:197-198`) — and without a heartbeat channel Temporal cannot deliver
cancellation to an in-flight activity at all. The orphan reaper cannot substitute: it keys on local PID
and PID start time, reclaims worktrees rather than remote state, and never runs under `goobers worker`.
So the receiver expires abandoned work keyed by the epoch on its own timer, per the declared `lease`.
**"We cancelled it" is not achievable from this codebase and this design does not say it is.**

**Expose a status/reconcile read addressed by the epoch alone**, returning `absent` (no unit of work was
ever created — the submit did not land), `pending`, `committed` (with the declared response fields
present), or `expired` (created, then TTL-expired unclaimed; any partial effect is the receiver's to have
unwound). This is not an extra endpoint an operator must build: **it is the poll leg.** Requiring it to
be epoch-addressable rather than handle-addressable is what makes reconciliation after worker loss
possible at all, and it is the single most load-bearing constraint in the receiving contract — a poll
keyed only on a far-side-minted handle leaves a lost submit response permanently unresolvable.

**No inbound callback.** The receiver must not require Goobers to expose an inbound endpoint; that door
is #648 and only #648 (§5.2).

### 3.5 Deadlines and progress

**The dispatcher imposes no deadline.** `TaskExecutor.Run` passes the caller's context straight through
(`internal/executor/dispatch.go:107-117`), and the attempt context carries none — built with
`context.WithCancelCause` over `context.WithoutCancel` (`internal/runner/stalled.go:127`), cancelled only
by an interrupt path. **Each kind must self-enforce, and the shipped kinds disagree about how.**
`ShellExecutor` does (`internal/executor/shell.go:354`) and `ci-poll` does, deriving a budget that leaves
margin and applying its own timeout (`internal/executor/cipoll.go:192-198` via `boundedwait.CIPollBudget`;
`context.WithTimeout` at `:323`). `external-telemetry` also bounds itself, but from connector policy only
(`internal/externaltelemetry/host.go:69`) — it ignores `env.Limits` entirely, so a stage's declared
`timeoutSeconds` means nothing to it.

The call-out follows `ci-poll`. The host-configured target carries the ceiling and a workflow may only
tighten it — the shipped tighten-only rule `effectiveLimits`
(`internal/externaltelemetry/host.go:475-493`), copied verbatim including its deliberate asymmetry: a
requested timeout, attempt count or byte cap is taken only when *lower*, a requested retry backoff only
when *higher*, so no workflow can increase load on someone else's endpoint.
`env.Limits.MaxDurationSeconds` (`api/v1alpha1/envelope.go:117-118`) is the authoritative outer bound,
and the executor derives its budget from it with a `CIPollBudget`-shaped margin so a typed timeout result
crosses the stage boundary *before* the enclosing kill. Every network wait selects on `ctx.Done()`.
`CheckStageTimeoutCoherence` — the one static check that catches this class of bug — must learn the kind
in **both** interpreter copies, which are identical apart from the package clause (verified by diff).

**Progress is a construction invariant.** The runner installs a coalesced reporter on every attempt
context (`internal/runner/run.go:2430-2433`) and skips a heartbeat tick that saw no progress
(`:2447-2449`), so a silent stage emits nothing and the 45-minute watchdog escalates it;
`invoke.ReportProgress` is a no-op with no reporter installed (`internal/invoke/invoke.go:30-34`).
`shell` reports on every output write and `ci-poll` after every poll (`internal/executor/cipoll.go:350`);
external-telemetry has the hook and the composition root leaves it nil
(`cmd/goobers/runnerwiring.go:887-889`). The call-out executor's constructor **refuses to build without a
progress hook**, the same fail-closed shape `NewTaskExecutor` uses to refuse a registry with no shell
executor (`internal/executor/dispatch.go:75-81`), and deliberately unlike `NewTelemetryQueryExecutor`,
which validates only host and recorder and so lets a nil `Progress` ship (`:78-86`). It reports at four
points: request sent, response headers received, each poll tick, each transport retry. The shipped wiring
bug is fixed in the same change, because it is the template people copy. The invariant is **enforceable
on tier 1 and inert on tier 3** (§9.2).

### 3.6 Retry semantics, the timeout-type split, and reconcile mode

`Task.Retry` fires only on a dispatch-level Go error — the branch is gated on `if dispatchErr != nil`
(`internal/runner/run.go:2678`), so a returned `Status: failure` falls straight through to
`stage.finished`. Two budgets exist: the declared policy budget (default 1 attempt, constant backoff with
no jitter by design, `api/v1alpha1/workflow_types.go:293-307`) and a hard-coded infrastructure budget of
2 that is **not** DSL-configurable (`internal/runner/run.go:56-58`, applied at `:2685-2691`) and is
shared with the engine (`internal/engine/retry.go:57`). Classification is structural, never message
matching: the infrastructure marker is an error type (`internal/invoke/invoke.go:89-110`) and
`executor.StageFailure(code, err)` attaches a machine-readable code
(`internal/executor/stageerror.go:28-36`) while the journaled `Error.Code` stays the
conformance-normative `executor_error` (`internal/runner/run.go:2704-2712`).

| What happened | The executor returns | Runner behaviour |
| --- | --- | --- |
| Transport failure, 5xx, 429 before any answer | `invoke.InfrastructureFailure(executor.StageFailure("infra_net_callout_failed", err))`, or `…Until` for an **in-budget** `Retry-After` (§3.8) | Infra budget: at most one more dispatch, class `infra`, excluded from conformance |
| Far side answered and refused (4xx, typed error body) | `ResultFailure` whose `Code` is drawn from the kind's own closed `callout_*` vocabulary, never the far side's string | No retry; the workflow branches on it at a gate |
| Budget exhausted while still waiting | `ResultFailure`, code containing `timeout` | No retry — the same choice `ci-poll` (`internal/executor/cipoll.go:623-634`) and `shell` make |
| `Retry-After` beyond budget, or per-target budget exhausted | `ResultFailure`, `Retryable: false`, `callout_retry_after_exceeds_budget` / `callout_target_budget_exhausted` | Terminal (§3.8) |
| Effect neither confirmable nor excludable | `ResultBlocked` + `callout_outcome_unknown` | The run parks for a human or a reconcile stage (§3.7) |
| Undeclared capability, unknown target or operation, malformed inputs | Go error, no infra marker | Policy-classed dispatch failure; fails the stage closed |

**The timeout-type split (D7).** `attemptFailureClass(err error)` matches `*temporal.TimeoutError` and
returns `journal.AttemptInfra` at `internal/engine/retry.go:169-171` **without reading `TimeoutType()`**,
and `dispatchWithRetry` retries it (`:104-109`). The function's own doc justifies this by asserting
`StartToClose` "only fires when the worker was lost before the stage produced a verdict" (`:154-158`) —
true, and *before a verdict* is not *before a side effect*. That gap is the entire feature. The SDK
exposes the discriminator: `TimeoutError.TimeoutType()` (`go.temporal.io/sdk@v1.47.0/internal/error.go:776-778`;
`temporal/error.go:127` is the type alias, and `temporal.NewTimeoutError`, `:252`, makes one constructible
in a test). Every activity dispatches under `StartToCloseTimeout: limits + stageTimeoutGrace` with
`MaximumAttempts: 1` (`internal/engine/engine.go:686-695`), which is what makes `START_TO_CLOSE` mean
"worker lost".

**The workflow cannot key on the effect posture, and does not need to.** `effect: mutating` is declared
per operation in `instance.yaml` (§3.2, §4.1); the compiled `run.externalCall` block carries only
`target`, `operation`, `mode` and `responses` (§2.2); and `internal/engine/engine.go:204-207` re-compiles
inside the workflow function, where a value read from live operator config is exactly the non-determinism
§2.4 forbids. `mode` is not a stand-in either: `effect: mutating` requires `mode: submit-poll` (§4.2) but
not the converse, so keying on `mode` would send read-only submit-poll operations into a reconcile read
their declaration need not support. **The decision splits across the seam.** The workflow needs one bit —
*"this re-dispatch may be a repeat"* — derivable from the compiled task alone; the question that bit
raises, *"is a repeat safe?"*, belongs to the executor, which must resolve target and operation against
the host registry to issue any request at all and which runs inside the activity where reading operator
config is ordinary. Concretely: `attemptFailureClass(t apiv1.Task, err error) (journal.AttemptClass, bool, error)`,
the extra return being `reconcileNext`, reading `TimeoutType()` and `t.Run.Kind` — both deterministic, the
timeout type off the error Temporal recorded in history and `t` off the definition the workflow
re-compiles. `dispatchWithRetry` already holds `t apiv1.Task` (`:46`) and is the sole caller (`:98`); its
closure parameter becomes `dispatch func(workflow.Context, bool) (stageActivityResult, error)`, with two
call sites — `internal/engine/engine.go:476` (agentic, ignores it) and `:494` (deterministic, copies `env`
and sets `CallReconcileOnly` before `ExecuteActivity`). The signature change is compiler-enforced; no
root, no option, no wiring.

The classifier table is therefore **type-keyed only**:

| `TimeoutType()` | Committed? | Every other stage (unchanged) | `run.kind: external-call` |
| --- | --- | --- | --- |
| `SCHEDULE_TO_START` | **No** — never dispatched; no request left any worker. Unset on `main` (Temporal's default is unlimited); 15m on the spike (`spike internal/engine/engine.go:726`) | `AttemptInfra`, retry | `AttemptInfra`, plain retry |
| `START_TO_CLOSE` | **Unknown** — the worker was lost; the grace guarantees self-enforcement wins the race *only while the worker is alive* | `AttemptInfra`, retry | `AttemptInfra`, retry with `CallReconcileOnly` |
| `SCHEDULE_TO_CLOSE`, `HEARTBEAT` | unreachable (never set on either checkout) | `AttemptInfra` (unchanged) | unclassifiable — fail closed |

That last row's tightening is **scoped to the call-out kind**, so the blast radius for shipped stages is
literally zero while the function's stated posture — "a projection error, never a silent default to
'infra'" (`:159-160`) — is preserved on the one path that can mutate someone else's state.

**Reconcile mode** is what the executor does with the flag, and the posture decides. **Mutating**: perform
only the epoch-keyed poll leg, never the submit — `committed` → `success` with the projection taken from
the status body, at a cost of one poll request; `absent` → the submit demonstrably never landed, so the
ordinary infra retry is now safe and is taken, still bounded and still carrying the same epoch; `pending`
→ resume the poll loop within the remaining budget; `expired` → `failure` with a code saying the work was
abandoned, an honest terminal outcome rather than an unknown; the reconcile read itself failing, or a
target declaring no epoch-addressable poll leg → §3.7. A mutating operation always *has* an
epoch-addressable poll leg, because config load refuses every other shape (§4.2), so reconcile is always
available where it is needed. **Read-only**: a repeat of a read is not a repeat of an effect, so the flag
is a no-op and the ordinary call is made — byte-for-byte today's behaviour. The attempt class is
`AttemptInfra` with `Runner["calloutReconcile"] = true`; **do not mint a fourth `AttemptClass`** — the
three values are pinned to the Go consts by a schema-enum test, and `AttemptInfra` is already the class
that keeps the attempt out of the conformance set (`internal/journal/event.go:407-409`), which is right
for a worker-loss recovery. Priced against the alternative — declaring `effect` in the workflow YAML and
cross-checking it at dispatch — this adds **zero** DSL fields, schema enums, compile rules and composition-root
options, where that alternative adds one of each and makes the *authoritative* posture a copy that can
drift from `instance.yaml`. **Residual, stated:** the posture is read from the worker's registry at
dispatch time, so an operator who flips an operation from `mutating` to `read-only` between the lost
attempt and the retry gets the read-only path on the retry. That is CT-1 territory (§4.3) and is not
defended against here.

**Tier 1 is the same shape.** A crashed daemon re-dispatches the interrupted attempt wholesale at
`attempt+1` with class `infra` (`internal/runner/run.go:1340-1347`) — the tier-1 analogue of
`START_TO_CLOSE` — so the same place sets a `reconcileFirstAttempt` bool when
`t.Run != nil && t.Run.Kind == external-call`, threaded to `env.CallReconcileOnly` for that first attempt
only. One bool through two internal frames, no new type, and again no posture on the caller's side.
Policy and infra retries *within one live dispatch* stay plain retries leaning on §3.4, because there the
executor is still in-process and knows whether its own request was ever sent. A watchdog takeover orphans
the call outright; only the far-side TTL reclaims it.

**The kind owns its error; the far side never writes it.** Two constants on the returned `ErrorInfo`, not
projections. **`Error.Retryable` is always `false`** — every `ResultFailure` the kind returns is terminal
by construction, since `Task.Retry` is gated on `if dispatchErr != nil` (`internal/runner/run.go:2678`)
and the tier-3 loop classifies dispatch errors only (`internal/engine/retry.go:98`), so a `true` there
would be a lie before it was an attack. **`Error.Code` is drawn from a closed `callout_*` vocabulary owned
by the kind** — an unexported named type `calloutErrorCode` with package consts in `internal/callout`, so
a far-side string cannot reach the field without an explicit conversion. Namespacing the far side's own
code is *not* enough: `telemetry.ClassifyError` substring-matches
(`internal/telemetry/errorclass.go:109-133`, `Contains(lower, "rate_limit")` at `:111`, `"timeout"` at
`:121`), so a far side choosing `..._rate_limit` would steer the attempt's telemetry class and the
nomination queries built on it; the prefix is also what makes collision with `"nonzero_exit"`
(`internal/gate/automated.go:323`) and with `ISSUE_OVER_SCOPE`/`NEEDS_DECOMPOSITION`
(`internal/runner/run.go:3362-3363`) structurally impossible. The mechanism, so this is a rule rather than
a convention: the kind constructs no `apiv1.ErrorInfo` literal at all, only
`calloutFailure(code calloutErrorCode, msg string) apiv1.ResultEnvelope`, which sets `Retryable: false`
unconditionally and whose named parameter type makes a far-side string a compile error at every call site.
The far side's own code is not lost — it goes in the journaled `error` event's `Runner` map
(`internal/journal/event.go:292-294`) for diagnosis, and an author wanting to *branch* on it takes it
through a declared `responses` projection under an author-chosen output name, which is the params-driven
arm §7 labels open by design. Why this matters is §7 row 1: `errorRetryable` reaches `failure-class`
through `subject.Error`, not through outputs, so the reserved-key rule does not touch it.

**At-least-once is the honest semantics, and the epoch is what makes it survivable.** Every path above
can repeat the call: a policy retry, the infra budget, a crash re-dispatch, a lost worker, a watchdog
takeover. None is opt-out and no compensation hook exists anywhere in either runner. A read-only call-out
repeats a read; a mutating one repeats a *submit*, and the only thing between that and a duplicate effect
is the receiver's obligation to dedup on the epoch — an obligation the operator asserts at config load
and Goobers cannot verify. This design's position is that it is acceptable *only* because the assertion
is explicit, the epoch is deterministic and journaled, `START_TO_CLOSE` reconciles rather than
re-submits, and an unresolvable outcome parks the run honestly rather than reporting a failure that did
not happen.

### 3.7 The ambiguous outcome

`ResultStatus` has exactly four values (`api/v1alpha1/envelope.go:178-198`, `IsValid()` at `:461-467`),
mirrored by `result.schema.json`'s enum, so an unknown currently masquerades as a failure — which a
downstream `status-equals` or `ci-status` gate acts on as a definite negative. **D8: v1 represents it as
`Status: blocked` with `ErrorInfo.Code: "callout_outcome_unknown"`.** No new status, no schema change.

1. **A fifth status fails open, silently, in both runners.** Neither `taskOutcome` switch has a
   `default:` arm — `internal/runner/run.go:2114-2246`, `internal/engine/engine.go:368-384` — so an
   unhandled status falls through to `switch t.Next` and **the stage advances as if it had succeeded**.
   Adding one means adding `default:` arms to both: a behaviour change to the success path of every stage
   in the product, to express one call-out's uncertainty.
2. **The blast radius is wider than the two switches**: `IsValid()` is enforced at runtime on the tier-3
   container path (`internal/gooberruntime/runtime.go:188`), plus the schema enum, the harness prompt's
   literal result-shape hint (`internal/harness/prompt.go:169-171`), the read model's disposition
   vocabulary (`internal/readmodel/project.go:27`), and the read service's status comparisons.
3. **`blocked` already means what an unknown outcome means** — "the stage cannot proceed without external
   intervention; the runner halts the run pending it" (`api/v1alpha1/envelope.go:182-184`) — and it is a
   first-class producer value on both tiers: tier 1 journals the cause, notifies and runs the
   instance-level parking handler before terminalizing at `PhaseEscalated` (`:2115-2162`), tier 3 maps it
   to `StatusEscalated` (`internal/engine/engine.go:369-371`). Crucially it does not hand a downstream
   gate a false `failure` to branch on.

`ErrorInfo.Code` is an open string in the schema, so the reserved code costs nothing there; it gets a Go
home in `api/v1alpha1` beside `IntegrityAdmissionErrorCode` (`api/v1alpha1/integrity.go:63`). Target,
operation and epoch reach a parking handler or reconcile stage **through the journal, not through
`Outputs`** — §6.4 owns that rule and §3.9 restates it: a call-out's `Outputs` carry the declared
response projection and nothing else. Re-exporting the epoch as an output would put a runner-owned
identifier into the one map the far side's projection also writes, which is the collision XC-A5 exists
to prevent; and it is unnecessary, because the epoch is a pure function of runner-owned inputs
(§3.3) that a later stage re-derives or reads from `stage.started`.
This also refines the disposition boundary: `blocked` is for policy refusals a rerun cannot fix **and**
for a genuinely unknown effect; a budget refusal is *not* blocked, because a later rerun fixes it once
the window rolls.

### 3.8 Return-leg bounds: `Retry-After` and the per-target call budget

**A far-side `Retry-After` is an unbounded run-parking primitive today.**
`invoke.InfrastructureFailureUntil` stores the caller's time on the marker
(`internal/invoke/invoke.go:96-103`) and `infrastructureRetryDelay` returns `retryAt.Sub(now)` verbatim
whenever it exceeds the declared backoff (`internal/runner/run.go:2799-2809`); the tier-1 wait selects on
`time.After(retryDelay)`, the run context, and an attempt context carrying no timeout (`:2734-2738`), so
the only backstop is the 45-minute watchdog — which escalates the run rather than failing the stage.
**Tier 3 is worse**: the identical value becomes `workflow.Sleep`, a durable timer
(`internal/engine/retry.go:115-120`, `:127-141`), and `TemporalStarter.Start` sets no
`WorkflowExecutionTimeout` or `WorkflowRunTimeout` at all (`internal/engine/starter.go:74-81`). Today's
exposure is bounded only by accident: the sole shipped producer reads `outputs["rateLimitReset"]` from a
Goobers subcommand's own result (`internal/executor/shell.go:754-765`), not raw far-side header bytes.

The codebase already solved this and wrote down why the obvious fix is wrong: `rateLimitPlan`
deliberately does **not** cap the individual server-directed wait, because "the old blanket
`rateLimitBackoffMax` cap turned a 21-minute reset into futile 60s sleeps that could never straddle the
window (#614)" (`providers/github_issues.go:157-162`); the total is bounded by a **budget checked before
sleeping**, and exceeding it is a typed terminal outcome, not a truncation
(`providers/github.go:3065-3075`, budget `defaultRateLimitMaxWait = 5 * time.Minute`,
`providers/ratelimit.go:18`). So the kind honours a `Retry-After` only when
`delay ≤ min(remaining stage budget, target policy.retryAfterMax)` — the remaining budget derived the way
`ci-poll` derives its own — and otherwise returns a terminal `ResultFailure` with
`callout_retry_after_exceeds_budget`, carrying the requested delay in the `Runner` map. Parsing reuses
`retryAfterDelay` (`providers/ratelimit.go:21-39`); do not write a second parser. **Scope, stated
honestly: this clamp binds the call-out kind only.** `infrastructureRetryDelay` stays unclamped on both
tiers and the shipped shell provider path keeps its behaviour —
`TestInfrastructureRetryWaitsUntilDeclaredReset` (`internal/engine/retry_test.go:150-157`) pins today's
verbatim honouring of a 17-minute reset and must keep passing.

**A per-target call budget.** The `ci-poll` loop this design reuses structurally is wired to quota
accounting that sheds when exhausted (`buildCIPollExecutor(…, quota *localscheduler.ProviderQuotaState)`,
`cmd/goobers/runnerwiring.go:856`, called at `:2165`), and the call-out copies the poll structure and not
the budget half — defensible under read-only, not now. But **the correction that changes the
recommendation**: `ProviderQuotaState` is not an operator-declared budget, it is a mirror of what a
provider advertises. `ProviderPollBudget.Known` is documented "false when no active window is known, so
all polls are admitted without inventing a quota" (`internal/localscheduler/providerquota.go:31-33`), and
windows are populated only from observed `X-RateLimit-*` headers. An arbitrary target advertises nothing,
so copying that mechanism verbatim would admit every call forever. A call-out budget must be
operator-declared: `maxCallsPerHour` (per target, per process, rolling window) and `maxCallsPerRun` (per
target, per run id) in the target's `policy` block.

**What is counted is requests, not dispatches**: submit legs, poll legs, reconcile reads, infra retries,
and calls after a gate repass — `maxCallsPerRun` being the field that caps the multiplication this design
worries about (`policy.maxAttempts` × the hard-coded infra budget of 2 × repasses × poll ticks). The
ledger is in-memory and per-target, owned by the `internal/callout` host at the same construction level as
`providerQuotaAccounting`, refusing **before** the request is issued — not the shared `ProviderQuotaState`,
which is keyed by a closed `apiv1.Provider` enum, not a target name. **On tier 1 it is a daemon singleton
and `maxCallsPerHour` is a real instance-wide bound; on tier 3 it is per worker process**, so N workers
means an effective ceiling of N × `maxCallsPerHour` — a genuinely global bound needs a shared store, which
is the machinery §1.5 excludes. `maxCallsPerRun` is exact on both tiers, because one run's stages are
dispatched by one runner or one workflow. A shed is a terminal `ResultFailure` with
`callout_target_budget_exhausted`, **never** an infrastructure marker, since retrying is the thing the
budget exists to stop; it is journaled through the existing `error` event with the budget, window and
target in the `Runner` map — **not `poll.shed`**, whose schema description marks it
instance-journal-only and which is absent from `projectableEventTypes`, so putting it in a run journal
would fail the tier-3 projection for the entire run. **Both fields unset means unbounded**, preserving
behaviour for every existing config, with `goobers validate` *warning* — not failing — on a target with
`hatch:` and no `maxCallsPerHour`. A mandatory budget with no defensible default is how operators learn
to write `999999999`.

### 3.9 Envelope in, result out

The call-out reads a deliberately narrow slice of the envelope: `RunID` (also the run's 32-hex OTel trace
id, `:47-49`), `TaskID` and `Gaggle` for identity, `Capabilities` (`:113-116`) for the re-check, `Limits`
(`:117-118`) for the deadline, `CallEpoch` and `CallReconcileOnly` for §3.3/§3.6, and `Inputs` **for
request parameters and the bounded-wait budget only** — the target, operation, mode and projection come
off `DeterministicRun.ExternalCall`, never off `Inputs`. It sends no workspace bytes and no upstream
result bodies: the envelope carries `ContextPointers`, pointers only (`:106-109`), and a call-out does not
widen that invariant. On tier 3 this restraint is also a confidentiality property, because activity
arguments are history-resident (§7).

What comes back is a `ResultEnvelope` built by the executor, never by the far side: a **status** per
§3.6; **outputs** limited to the declared projection, scalars only; **exactly one artifact** per attempt,
never a stream; **`ExternalRefs`** carrying the callee identity (D13); a typed **error** whose `Code` is
chosen to classify, since `telemetry.ClassifyError` matches a small prefix set and drops everything else
into `unknown` (`internal/telemetry/errorclass.go:110-136`); and **metrics** (latency, attempt count).
## 4. Targets, credentials, and the trust boundary

A call-out is the first stage kind whose blast radius is decided by configuration rather than by code:
every other credentialed capability names an authority Goobers implements, while a call-out reaches
whatever an operator wrote down, and now may change it. So the security argument lives not in the
transport — `internal/externaltelemetry` already demonstrates that credibly — but in **who writes the
address down, what a stage obtains by naming it, and where mutation is refused**.

### 4.1 The target registry

Targets live in a new top-level `externalCallTargets` list in `instance.yaml`, a sibling of the shipped
`externalTelemetry.connectors` list (`internal/instance/config.go:81-84`) and modelled on it. **This
block is the schema of record for every per-target knob in this document.**

```yaml
externalCallTargets:
  - name: build-oracle
    endpoint: https://oracle.corp.example:8443/v1        # scheme+host+port+path prefix
    proxy: ""                                # explicit; never inherited from the environment
    network: {allowLoopback: false, allowPrivate: false, allowLinkLocal: false}
    auth: {mode: bearer-token}               # the only value (D17); the grant key is derived
    policy:
      timeout: 45s
      maxAttempts: 3
      retryBackoff: 5s
      retryAfterMax: 5m                      # §3.8
      maxCallsPerHour: 120                   # §3.8; unset = unbounded
      maxCallsPerRun: 8
    response: {maxBytes: 262144, contentType: application/json}
    sendTraceContext: true                   # per-target traceparent propagation; default on
    operations: { … }                        # §3.2 — methods live here and only here
    hatch: {reason: "legacy TLS terminator on RFC1918", expires: 2026-10-01T00:00:00Z}
```

| Field | Constraint | Why it differs from the telemetry connector |
|---|---|---|
| `name` | `^[a-z][a-z0-9-]{0,62}$`, unique per instance | The only string about a target an author ever writes; it also **derives** the grant key (§4.4) |
| `endpoint` | One absolute URL, `https` (or `http` only for loopback), explicit port or scheme default, path is a **prefix**, no userinfo/query/fragment | `NetworkPolicy` allowlists are bare hostnames — `Validate` rejects entries containing `/`, `:` or `@` (`internal/externaltelemetry/contract.go:418-437`) and `Allows` compares hostnames (`:441-451`) — so an allowlisted host exposes every other service on it |
| `proxy` | Explicit; empty means direct | `http.DefaultTransport` is `Proxy: ProxyFromEnvironment` and is what the existing policy client uses (`internal/externaltelemetry/http.go:20`), so an ambient proxy silently intercepts a "direct" call and a dial-time check would inspect the proxy's address (§9.4) |
| `network.*` | Three explicit opt-ins, default false | An on-prem RFC1918 endpoint and a loopback dev stub are both legitimate — declared one target at a time, never by a global switch |
| `policy` | Host-side ceilings, tighten-only (`effectiveLimits`, `internal/externaltelemetry/host.go:475-493`) | §3.5 and §3.8 own what the kind does with each |
| `response.maxBytes`, `.contentType` | One byte ceiling, one content type | Bounds what the return leg pushes into the journal and, on tier 3, Temporal history. **The single ceiling in the design**: the kind enforces it at the transport and the bounded recorder is the backstop, not a second limit |
| `sendTraceContext` | Boolean, default true | Per-target correlation opt-out (§6.6) |
| `operations` | Closed map keyed by the DSL's `operation` name (§3.2) | Declares methods, paths, parameters, response fields, effect posture and idempotency contract. **The only place a method exists** |
| `hatch` | Optional, mandatory `expires` | §5.4; config load refuses an expired hatch |

**The path prefix is checked at request build time and again at dispatch**, using the shipped shape — a
private `http.RoundTripper` the host wraps around the client so the executor only ever receives the
wrapped doer (`internal/externaltelemetry/http.go:19-42`), with denials reported through `url.Redacted()`
(`:39`). Because the check lives in a `RoundTripper` rather than a one-shot preflight, `http.Client`
re-enters it on **every redirect hop** — a property that must be preserved. State plainly what this is
not: a scheme/host/port/path constraint is an **authorization** control deciding which recorded target a
stage may reach, not SSRF defence, because name resolution happens after the check inside
`next.RoundTrip` (`:40-41`). Dial-time address checking belongs to the egress binding (§5.3).

### 4.2 Where mutation is refused: the enforcement points

> **A workflow cannot name an HTTP method.** Methods exist only in `instance.yaml`, are refused at config
> load unless the operation's declared effect and idempotency contract admit them, and are read at
> dispatch from the operator-supplied registry rather than from any runtime-overridable map. There is no
> safe-method allowlist in v1; the control is that mutation is operator-declared, operator-priced, and
> unreachable from agent-authored YAML.

**Config load is the only real gate for methods.** `instance.LoadConfig` decodes with
`dec.DisallowUnknownFields()` (`internal/instance/config.go:1331`) and calls `cfg.Validate()` (`:1344`),
where every instance-level invariant is enforced today including the per-connector `envPassthrough`
refusal at `:1424-1433`. **`api/schemas/instance.schema.json` is not an enforcement point** — no non-test
Go file loads it, and this design does not cite it as a control. A new `validateExternalCallTargets()`
refuses, per operation: a method outside the target's declared set; `effect: mutating` with
`mode: request` (a mutating bounded request/response has nowhere to put a reconcile read);
`effect: mutating` with an incomplete `idempotency` block or with `honoured: false` (refusing to
configure a duplicate-effect generator); a mutating `poll.path` without `{epoch}` (a handle-only poll
cannot resolve a lost submit); a non-`GET`/`HEAD` method on a `read-only` operation; and an expired
`hatch.expires`.

`idempotency.honoured: true` is an **operator assertion**, in exactly those words: Goobers cannot verify
it. What config load buys is that an operator cannot arrive at a mutating call-out by accident — making
one requires writing down, in operator-owned config, that the receiver dedups on the epoch, retains the
mapping and leases abandoned work. That is the difference between a control and a hope, and it is the
strongest honest claim available.

**Compile** never sees a method, and never sees a target or operation *name* either: existence is not a
compile rule of the language (§2.4). The tier-1 daemon cross-references both names, and the projection
against the operation's declared response, over the compiled machines at `cmd/goobers/daemon.go:568-573`.
**Dispatch** resolves target and operation by
name against the host registry the executor was constructed with and takes the method from there, never
from `env.Inputs`; the only dispatch-time refusals are unknown target, unknown operation, undeclared
capability, and the path-prefix check.

### 4.3 Assumption CT-1, and what breaks if it is false

> **CT-1.** Agents cannot edit `instance.yaml` and cannot apply cluster changes. They lack the
> permission, and under sandboxing they lack the reach.

The owner justification: an actor able to edit *and apply* the instance configuration could equally spin
up pods and manipulate the cluster wholesale, so the target list is not the marginal risk. CT-1 is a
**named assumption of the threat model**, not a control this design implements. It has visible structure
on both tiers: `instance.yaml` is a provisioning file read from an operator-supplied path
(`internal/instance/config.go:1320-1341`), outside the agent-authored `workflowSource` tree (`:211-221`);
on tier 3 it is a ConfigMap (`spike deploy/dev/k8s/goobers.yaml:326-327`) with credentials arriving
through the CSI driver and re-staged 0600 by an initContainer (`:152-170`), so changing a target is a
cluster apply and changing a credential is a Key Vault write.

If CT-1 is false, the target list is **not** the thing that got worse. That actor can already repoint any
repository's token ref (`internal/instance/config.go:432-436`), bind any capability to any env var, file,
keychain item or store path (`:96`, `:934-940`), add names to `runner.envPassthrough` and carry chosen
ambient variables into every stage subprocess (`:252`, `internal/procenv/procenv.go:111-133`), redirect
`workflowSource`, or configure a telemetry connector against any host — each a larger primitive than a
call-out target. What the call-out *adds* in that world is convenience and reach: a sanctioned,
journaled, credentialed egress path with a documented return leg, which is an easier exfiltration and
injection channel to operate and looks legitimate in the run record. Concretely: add a target, bind an
*existing* credential ref, self-grant the capability. `TokenRef`-as-reference prevents minting a new
credential; it does not prevent pointing an existing one at a new destination.

Two design consequences follow. The registry must be **legible**, so the portal and run record show which
target a stage reached by the structured identity §6.3 specifies. And CT-1 must be a **recorded
deployment obligation**, because its second clause is not true of the shipped tier-1 default (§9.5).

### 4.4 One credential, one target — enforced, not conventional

**D17: v1 has no `auth.mode: none`.** For an unauthenticated target the whole authority chain is a string
the author wrote: `dispatchTask` passes `t.Capabilities` verbatim into `buildEnvelope`
(`internal/runner/run.go:2993` → `:3907`), the runtime re-check tests only that list
(`internal/executor/externaltelemetry.go:91-94`), and the one thing that turns a declaration into an
authority — a token having to be obtainable — never fires, because `Materialize` *skips* a capability
with no configured grant rather than erroring (`internal/credentials/capability.go:123-127`, skip at
`:148-151`). Rev 1's answer, deferring to the egress binding, is not supportable: both bindings arbitrate
*destination* and neither knows which stage is asking. Deleting the enum value is negative machinery, and
it makes the authority record the operator's `credentials:` entry, which already exists, is already
operator-owned, and already fails closed via `ErrNoCredentialForCapability` (`:174-186`).

**The executor resolves the token before it builds the request and treats absence as a hard failure.**
This must be stated because the shipped analogue does the opposite — `buildStageEnv` *skips* a
declared-but-uncredentialed capability with an explicit `continue` (`internal/executor/env.go:215-222`).
`Set.Token(ctx, "callout:invoke@<target>")` is the first act after the capability re-check, and either
sentinel error is a typed refusal. That single line converts the grant from paperwork into the authority
record.

Three config-load rules make "one credential per target" true rather than conventional. **D-C1:
`auth.capability` is deleted; the grant key is derived, `callout:invoke@<target.name>`** — two independent
strings could never both work anyway, since `Materialize` populates `Set.declared` from the
*stage-declared* list only, so a token can only ever resolve under the stage's spelling. Deleting the
field deletes the question and makes §4.5's "the string a reviewer reads *is* the target the stage
reaches" literally true; key uniqueness then follows from name uniqueness plus the existing `seen[key]`
refusal (`internal/instance/config.go:1650-1653`). **D-C2: every configured target must have exactly one
matching credential grant**, and a `callout:invoke@x` grant for an unconfigured `x` is a load error,
because a dangling grant is a credential nobody can audit. **D-C3: no two call-out credential grants may
carry an identical token ref** — `instance.TokenRef` is a comparable four-string struct (`:877-889`), so
this is a `map[TokenRef]int` beside the existing `seen` map, scoped to pairs where at least one capability
is `callout:invoke@*` (a blanket rule would newly fail instances legitimately sharing one token across
provider capabilities). Its limit, stated rather than oversold: equal `TokenRef` is a *syntactic* test —
two env vars holding the same value, or two store paths aliasing one vault secret, remain the operator's
responsibility.

One target → one derived grant key → exactly one `credentials:` entry → one token ref distinct from every
other call-out target's → one origin and path prefix. The credential a stage obtains by naming `t` cannot
reach `u`'s address in any configuration that loads. **That is why §5.3 may say "unrepresentable" — a
refusal backs it** — and not, as rev 1 claimed, because the shipped ADX connector demonstrates cross-host
delivery. It does not: that connector builds every request against exactly one configured cluster
(`internal/externaltelemetry/adx/adx.go:187`, `cluster` fixed at `:113`). The hazard is **latent and
type-level** — `ConnectorConfig` binds one `Auth` beside a *list* of `AllowedHosts`
(`internal/externaltelemetry/contract.go:277-292`) — which is exactly what that defect table's own second
sentence says.

**The granularity ceiling, stated honestly.** A target-scoped grant bounds *which target* a stage
reaches; it does not bound *which workflow* may reach it. Deterministic grants are runner-owned and
gaggle-wide (`cmd/goobers/runnerwiring.go:2108`; `NewInjector` scopes to grants whose `Goober` is empty,
`internal/credentials/capability.go:51-56`), so every deterministic stage in the gaggle that declares the
grant — and passes the compile rule restricting it to a call-out stage — can invoke the target. **The
unit of call-out isolation is the gaggle**; an operator needing per-workflow isolation uses a second
gaggle. The deferred refinement is recorded with its enforcement point already found: a per-target
`allowedWorkflows` allow-list is enforceable *in the executor* with no new plumbing, because the envelope
already carries `WorkflowID: in.Machine.Def.Name` and `Gaggle` (`internal/runner/run.go:3893`, `:3897`).

### 4.5 Target-scoped capability grants, and ADR 0002

**D18.** The author writes `callout:invoke@<target>`; the runner does not mint scoping internally. This
follows `credentials.RepoScopedCapability`, which already produces `base@owner/name` keys so one base
capability resolves to a different token per repository and which is documented as opaque to `Injector`
and `Set` (`internal/credentials/scoping.go:69-78`) — so the credential machinery needs no change. A flat
`callout:invoke` is rejected: it would convert per-target curation into an all-or-nothing switch.

The split must live inside `internal/capability` rather than being patched at each of its 16 non-test
call sites: `Known` is an exact-string test over a hand-written 22-entry `All()`
(`internal/capability/capability.go:139-156`) and `StageDeclarable` wraps it (`:161-163`), so
`callout:invoke@x` fails both today. Unlike `contents:read@owner/name`, which is runner-owned and never
stage-declared, a call-out grant *is* stage-declared, so `StageDeclarable` must admit it. The published
enums stay in parity by expressing the scoped form as an `anyOf` **pattern** branch beside the existing
enum, with the five `path` strings in `constBackedEnums` repointed in the same commit (§2.6 rows 9, 10).
Note the v_current asymmetry, and what it must **not** become. `internal/capability` is one package both
interpreters call, so admitting the scoped form to `StageDeclarable` admits it on the frozen interpreter
too. That must not mean the frozen interpreter *accepts the grant*: XC-A4's biconditional — the only
thing stopping a non-call-out stage obtaining a target's bearer token, since `Materialize` applies no
kind check (§4.5) — lives in `v_next` alone. A 1.4 document declaring `callout:invoke@t` on an ordinary
shell stage would otherwise get the token with no rule refusing it. So the split is: `StageDeclarable`
admits the scoped *spelling* (the Go-level machinery is shared and must parse it), while `v_current`
**refuses any task declaring a `callout:invoke@…` grant**, with the same v_next-only diagnostic XC-A1b
gives the kind. A 1.4 author gets "this capability requires DSL 2.0", which is the honest message —
better than the "unknown capability" an earlier draft was trying to avoid, and far better than silent
acceptance, which is what avoiding it would have cost.

One admission rule is load-bearing: **the grant is valid only on a stage whose `run.kind` is
`external-call`.** It is one direction of XC-A4's biconditional (§2.4), and its reason is mechanical —
`buildStageEnv` (`internal/executor/env.go:149`) materializes **every** declared capability that has a
grant into `GOOBERS_CRED_<CAP>` (guard `:208-210`, `Materialize` `:211`, emission `:215-225`, name rule
`CredentialEnvVar` `:24-27`), and its only non-test caller is `ShellExecutor`
(`internal/executor/shell.go:314`) — so a **deterministic shell** stage merely declaring
`callout:invoke@build-oracle` would receive the target's bearer token in a subprocess running
agent-authored commands. The rule carries a second half worth writing down. The agentic path materializes
the same `Set` (`internal/harness/executor.go:335`) but emits a variable only for capabilities present in
the explicit `buildEnvCapabilities` table (`cmd/goobers/runnerwiring.go:368-377`), skipping the rest
(`internal/harness/environment.go:75-79`): **`callout:invoke` is never added to `buildEnvCapabilities`.**

**ADR 0002 is extended, not stretched.** Neither shipped spelling covers this case: the far side is not
the configured repository provider and not a named provider Goobers implements — it is an endpoint whose
semantics Goobers cannot characterise, because the receiver decides what the call does, and stretching
`provider:*` over it is precisely the contract bug the ADR's consequences section names. The third
spelling is `callout:<verb>@<target>`, and its rule is recorded with it: the namespace names an operation
*Goobers* performs (issuing a bounded request), never one the far side performs, and **the target suffix,
not the verb, carries the authority boundary**. Under D5 the verb deliberately carries no read/write
implication — that posture is declared per operation in `instance.yaml`, where it can be enforced. The
amendment lands with this design.

### 4.6 Resolution and injection: the Set, never the child environment

The credential resolves through `internal/credentials` and stops at the in-process `*credentials.Set`.
`Injector.Materialize` resolves only declared keys, registers every resolved value with the
`SecretRegistrar` *before* placing it in the Set, and fails the whole call if a granted key fails to
resolve so a stage never starts half-credentialed (`internal/credentials/capability.go:128-160`,
registration at `:156`). `Set` is the only thing handed to an executor — never the `Injector` or
`Resolver`, which can reach every configured ref (`:162-168`). Registration must reach the
instance-global registry through the tee registrar (`cmd/goobers/runnerwiring.go:2103-2107`), so the
value is redacted from spans and the instance log as well as the journal. `credentials.TokenRef` sets
exactly one of `Env`, `File`, `Keychain` or `Store` (`internal/credentials/source.go:26-58`), resolution
reads the live source at call time with file privacy verified first (`:71-83`), an empty resolved value
is an error (`:102-106`), and inline secret values are refused at config load with a CFG-009/SEC-010
diagnostic (`internal/instance/config.go:1654-1657`) — so naming a target does not mint a credential.

**A call-out credential is never placed in any child environment, on any code path, for any stage kind** —
not `GH_TOKEN`, not a dedicated `GOOBERS_CRED_*` variable. The contrast that makes the rule worth stating
is `buildEnvCapabilities`, which maps most credentialed capabilities onto the single `GH_TOKEN` variable
for the agentic environment (`cmd/goobers/runnerwiring.go:363-377`) — correct for capabilities backed by
one identity against one provider, wrong for a third-party bearer token.

This is also the correction to the external-telemetry precedent. The ADX connector's private resolver
(`internal/externaltelemetry/adx/adx.go:696-707`) supports only `env` and `file` and bypasses the
injector, losing keychain and secret-store sources. Precisely: it is **constructed once per connector at
daemon startup and is therefore host-scoped rather than stage-scoped** — it does re-read the ref on every
request (`resolvingTokenSource.Token` calls `s.resolver.Resolve` at `:654-655`, per query at `:196`, and
`tokenRefResolver` documents that a rotated source takes effect without a restart,
`internal/credentials/source.go:132-136`), but always the same ref, under no stage's declared capability
set. The call-out routes through the injector's grant map instead, which is what makes tier-3 Key Vault
delivery work with no second mechanism.

**P0-b belongs here.** `instance.yaml` already refuses to source a secret from an environment variable
stages can also see — but only for MCP grants (`internal/instance/config.go:1658-1665`, where
`stageEnvironmentAllows` tests the name against the `procenv` allowlist, its prefix families and the
declared passthrough list, `:1941-1958`). The same guard is applied to the daemon identity's token and
key (`:791-796`, `:819-824`) and to telemetry connector auth (`:1424-1432`); capability grants are the one
surface that skips it. So a grant with `token.env: CALLOUT_TOKEN` plus an operator who also lists
`CALLOUT_TOKEN` in `runner.envPassthrough` passes validation silently and `procenv.BaseEnvWith` carries
that variable into every stage subprocess in the instance, defeating this section's discipline by config.
The codebase understands the hazard precisely; it wrote this check and narrowed it to MCP.

### 4.7 Confinement is a subprocess property; a call-out has no subprocess

Three mechanisms read as isolation and all three shape **child processes only**: `procenv` is a
default-deny allowlist applied when building a child's environment (`internal/procenv/procenv.go:111-133`),
the sandbox wraps an argv (`internal/harness/confine.go:186-189`), and `run.network: none` configures
`exec.Cmd` attributes (`internal/executor/network.go:29-46`) applied by `ShellExecutor`
(`internal/executor/shell.go:377`). A call-out is in-process — `TaskExecutor.Run` calls the kind directly
in the daemon's own goroutine — so none apply, and declaring `network: none` would be parsed, accepted,
journaled and **have no effect whatsoever**. A field that reads as isolation and delivers none is worse
than an absent field, so the compiler refuses it (§2.2).

What actually confines a call-out is the target registry plus the egress binding, which produces a tier
asymmetry running opposite to intuition: **the laptop is the permissive environment, the cluster is the
restrictive one.** On tier 3 the process holds only mounted 0600 secrets and a ConfigMap, secrets resolve
through the Key Vault/CSI seam (`docs/design/k8s-infra-shape.md:87-94`), each gaggle has its own
namespace and cannot resolve a sibling's secrets (`:47-51`), and stage-pod egress is deny-first with no
inbound (`:80-85`). On tier 1 the daemon holds whatever the operator's shell had — `procenv` shapes
children, never the parent — so a call-out on a developer laptop runs with more ambient context than any
stage subprocess ever sees. The mitigation is §4.6's absolute rule, which is why it is stated as an
absolute rather than a default.

## 5. Egress, the gateway, and the governed escape hatch

### 5.1 One contract, two bindings

**D19.** Egress is declared **once**, on the target (§4.1), and enforced through a different substrate per
tier: in-process in the runner's own HTTP client on tier 1, a deployed egress gateway with
`NetworkPolicy` pinning worker egress to it on tier 3.

**The tier-1 binding is in-process, and that is stronger than a proxy, not weaker**: the daemon makes the
request itself and hands the stage a result, exactly as `ci-poll` and external-telemetry do today — the
telemetry host keeps the transport private so a plugin receives "only the host-wrapped request method"
(`internal/externaltelemetry/http.go:8-30`). The stage cannot bypass a call it does not make. In-process
enforcement is the *whole* control at tier 1, so it has to be complete, which is why the explicit-`Proxy`
fix ships with it (§9.4).

**The tier-3 binding is a gateway, and it is not optional there, because the in-process check has nothing
behind it.** On tier 3 the deterministic stage runs in the *worker process*, not a stage pod
(`internal/engine/activities.go:222`, `:256`) — and the reference manifests apply **no NetworkPolicy to
`goobers-system` at all** (its kustomization lists ten resources, none a policy,
`deploy/reference/goobers-system/kustomization.yaml:13-23`; the shipped `allow-stage-egress` policy
selects `goobers.dev/role: stage` and does not cover the call-out's origin). This design adds a
`goobers-system` egress policy admitting the worker to the gateway and nothing else, plus the gateway.

**Why they must share a contract.** NetworkPolicy is IP- and label-based; the shipped manifest says so
itself ("FQDN allowlists need the provider CIDRs below … or a CNI with FQDN policy support",
`networkpolicies.yaml:10-11`). A CIDR cannot express `https://host:443/v1/estimate`, so NetworkPolicy can
never *be* the target contract; the most it expresses is "the gateway". Re-authoring the rules in
NetworkPolicy terms would let the two tiers' answers drift with nothing to catch it, since egress
decisions are deliberately not conformance-normative (§6.1).

Two traps. **Ambient proxy env vars are not enforcement**: `procenv.Vars` passes
`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` to children (`internal/procenv/procenv.go:53`) and a child can unset
them, so a proxy enforces only when the network leaves no alternative route — which is what the
NetworkPolicy behind it is for. And **the tier-3 binding must be probed, not assumed**:
`checkNetworkPolicySupport` already hints the failure mode — "a CNI without NetworkPolicy support ignores
policies silently; verify deny-first takes effect with a probe pod"
(`internal/k8spreflight/checks.go:62-80`) — so the call-out adds gateway-reachability and
worker-containment checks to that preflight list as probes rather than a successful `apply`.

**Tier-3 v1 has only one of the two bindings enforcing**, and this document does not describe tier-3 v1
as gateway-contained anywhere. The gateway is sequenced with #1307/#2898 rather than pretended into
existence.

### 5.2 Ingress: #648 is the only door

A "special ingress/egress stage pod" is **rejected** (D20) for three independent reasons: it recreates
what `k8s-infra-shape.md:84` forbids ("No inbound to stage pods. Ever."), and stage pods run
agent-authored code; its lifetime is wrong, since a callback must outlive the stage that requested it
while stage pods are ephemeral per attempt; and it has no stable identity, since stage pods are
capability-routed across node pools. None of that makes a *gateway* exceptional —
`k8s-infra-shape.md:77` already specifies "one HTTPS ingress … (single inbound door)", realized as a
shipped reference manifest. An operator-deployed, non-agent-authored component with a stable address and
a run-independent lifetime is the *existing* shape of this architecture; the gateway is its outbound twin.

When async return legs arrive, the inbound surface is **#648** — a `POST` ingestion endpoint on the
existing daemon API surface, not a second listener. The codebase already refuses the alternatives: the
webhook receiver is loopback-only by contract (`internal/instance/config.go:360-368`) and binding the API
off-loopback without both TLS and OIDC is refused with "there is no insecure override" (`:2246-2253`).

### 5.3 The egress policy this design requires

Inheriting the external-telemetry `NetworkPolicy` verbatim is **not sufficient** — a good fit for one
connector against one vendor cluster, a poor fit for an arbitrary team-run endpoint. **This table is the
document's single analysis of that mechanism's weakness.**

| Defect | Evidence | Consequence |
|---|---|---|
| Hostname compare with resolution *afterward* | `Allows` does `strings.EqualFold(host, target.Hostname())` (`internal/externaltelemetry/contract.go:441-451`) inside a `RoundTripper` whose `next` is `http.DefaultTransport` (`http.go:20`, `:37-41`), so DNS happens inside the dial | No dial-time IP check, no DNS-rebinding defence. An allowlisted name resolving to `169.254.169.254` or an RFC1918 address is permitted |
| No port constraint | `Validate` rejects entries containing `/`, `:` or `@` (`contract.go:421-423`); `Allows` compares `Hostname()` only | `https://allowed-host:9200/` reaches an internal Elasticsearch on a legitimate host |
| No path constraint | same lines | The host's entire API surface is reachable, not the one endpoint the workflow needs |
| One credential beside a *list* of hosts | `ConnectorConfig` carries one `Auth` and `AllowedHosts []string` (`contract.go:277-292`) | **Latent, not observed**: nothing in the type system stops a connector reaching two hosts attaching one credential to both. The shipped ADX adapter happens to target a single configured cluster (`adx/adx.go:187`, `:113`), so no shipped path leaks today |

A fifth, smaller one worth fixing while nearby: `isLoopbackHost` accepts the literal name `localhost`
without resolving it (`contract.go:453-455`), so `allowHTTP: true` plus a rewritten `/etc/hosts` sends a
cleartext credentialed request off-box.

The model this design requires: a target is **scheme + host + port + path-prefix**, `https` only except
for loopback; **dial-time IP checking** via a `DialContext` denying link-local, loopback, RFC1918 and ULA
ranges unless the target opts in, on every connection and redirect hop, which closes the TOCTOU gap the
hostname compare leaves and is only meaningful when the proxy is declared rather than ambient; **one
credential bound to exactly one target**, which §4.4's three refusals make unrepresentable rather than
merely discouraged; and **host-side ceilings per target** in the shape `PolicyConfig` already
establishes, tighten-only. The tier-1 binding implements these in the kind's HTTP client and the tier-3
gateway implements the identical rules at the proxy — coherent only because they are expressed in terms a
proxy can evaluate.

The call-out **consumes** #1307 (egress allowlist + journaled network audit, proxy-first; OPEN,
`goobers:approved`, `plan:next-wave`) and #2898 (enforced egress; OPEN, `goobers:approved`) rather than
building parallel machinery: the target is strictly more precise than #1307's per-goober domain and
degrades cleanly to one; the journal shape of §6.1 *is* the audit trail it requires; and its proxy-first
mechanism is the tier-3 binding. This design does not pick #2898's enforcement baseline — that is the
operator's open decision — it only requires that whichever is chosen contains the **worker**, not merely
the stage pods. #1307 is re-gated on a human scope decision after the #2381 regression, so nothing here
authorizes an autonomous pass at enforced egress.

### 5.4 The governed escape hatch

Some operator will need a target the contract cannot express — rotating RFC1918 addresses, an
`http`-only legacy service — and without a hatch they disable the control globally, which is strictly
worse. So the hatch exists, governed (D22). The cautionary precedent is **#2034** (`network:none`
de-isolating globally with no visibility): the hatch was not wrong to exist, it was wrong for being
**broad and silent**, and the shipped fix is the template — fail closed unless explicitly opted in
(`internal/executor/network_windows.go:24-31`) and return a marker the caller stamps into the stage's
journaled `Outputs` (`network_windows.go:14-22`, applied at `internal/executor/shell.go:489-498`).

| Property | Requirement |
|---|---|
| Scope | Target-scoped, **never wildcard** — never a host pattern, a global switch, or an environment variable |
| Credential | Backed by an out-of-band provisioned ref, so naming a target still does not mint a credential |
| Visibility | Journaled at **every use** with structured callee identity — a truthful promise only because D13 gives `ref.touched` a non-lossy producer (§6.3) |
| Surface | Present in the portal on the stage that used it, riding the journaled-`Outputs` path #2034's marker uses |
| Lifetime | Carries `expires`; config load refuses an expired hatch. A hatch with no clock is a permanent policy exception wearing a temporary label |

Deferred: proactive notification ahead of expiry, and automatic renewal.

### 5.5 `no-phone-home` is not the egress control

**This subsection is the document's single treatment of the point.** The gate scans Go sources and
command files for *hardcoded* destinations; its `.yml`/`.yaml` scope is only paths containing
`/.github/workflows/` (`test/nophonehome/main.go:227-240`), so an operator's `instance.yaml` and
everything under `reference-workflows/` are outside it, and its Go monitors cover `net`, `net/http`,
`net/smtp`, gRPC and OTLP (`:26-81`). That is the gate doing its job — stopping *Goobers' own source*
from acquiring a destination the user did not configure — and its rule accepts any non-static destination
(`:694-710`), which is exactly what a configuration-sourced call-out is. SEC-048 also forbids the
tempting misreading: a failure is fixed by removing the collection path, not by adding an endpoint
allowlist — "There is intentionally no endpoint allowlist" (`docs/requirements/security.md:173-178`). The
target list is therefore **not** a no-phone-home exemption and must never be described as one.
## 6. Journal, conformance, and tracing

### 6.1 v1 adds no journal event types

**D11: zero new `EventType` constants, `Event` fields or journal-schema properties.** One call-out
attempt writes only shipped events: `stage.started` (carrying §3.3's epoch record), `stage.heartbeat`
(tier 1 only), `artifact.recorded` for the response bytes, `ref.touched` for the callee,
`stage.finished` for status, declared outputs, artifact pointers and any hatch marker, and `error` for a
transport or contract failure. That set is also what satisfies #1307's journaled network audit trail
(§5.3), so no new type is needed for an audit trail to exist.

Two mechanisms make a new type expensive, and both are loud. A new type is **conformance-normative by
default**, because `IsConformanceNormative` excludes heartbeats, gate markers, repair, runner
annotations, spans and the notification pair and then ends `default: return true`
(`internal/journal/event.go:406-431`). And **the tier-3 projection fails closed on the whole run**:
`projectableEventTypes` is a closed nine-entry map (`internal/engine/projection.go:25-35`),
`validateProjection` runs over every op at `:72`, and `journal.Create` is only reached at `:97` — so one
unrecognized type means no run directory, no `run.yaml`, no `events.jsonl` at all. These files are
byte-identical between `main` and the spike (verified by diff), so this is not a spike artifact a merge
resolves.

**Four of those six events are already in the projection whitelist; two are not.** `stage.heartbeat` is
tier-1-only by design (§3.5) and conformance-excluded as well. `artifact.recorded` works for a different
reason worth stating: the projection's artifact op does not append the event itself, it calls
`jr.RecordStageArtifactWithIntegrity` (`internal/engine/projection.go:111-127`) and those writer methods
emit `artifact.recorded` on the writer's own path (`internal/journal/run.go:496-498`) — the artifact
channel bypasses the whitelist entirely, which is exactly why response bytes are cheap and a new event
type is not.

The asymmetry is the trap: every *other* consumer is quiet. `readservice.classifyRunEvent` returns
`(RunEventUnknown, false)` from its default arm; the portal falls back to `humanize(event.type)`;
`readmodel.ProjectRun` folds through a switch with no error default; `internal/telemetry/rollup` carries
a hand-maintained fork of the `Event` struct with a 17-value subset of the constants
(`internal/telemetry/rollup/mirror.go:9-22`, const block at `:61-79`); and `journal.Run.Append` scrubs,
stamps, writes and fsyncs without validating the type (`internal/journal/run.go:280-317`). So a new type
sails through a tier-1 run, degrades gracefully in four read surfaces, and destroys the journal of the
first tier-3 run that emits it. If a later version adds one, it pays a four-way lockstep in a single
commit — the Go const and the schema enum (both machine-caught by the enum-parity rule), the conformance
classification (**caught by nothing**) and the projection whitelist (**caught by nothing but a tier-3
run**) — plus a dual-runner fixture, without which the whitelist edit is untested by construction.

### 6.2 What the cross-runner diff actually compares

Rev 1's rule — "anything the far side chooses is recorded as content, never as a field the cross-runner
diff compares" — is false. The correction belongs here, not in an objection.

> **Compared:** the response body's digest (`NormativeEvent.RefDigest`,
> `internal/journal/conformance.go:46`, populated at `:122-127`), the encoded artifact list (`:48`,
> `:148`), and **every projected output by value** (`:53-57`, `encodeOutputs` at `:162-176`) — alongside
> the stage status, `Integrity`, the stable `Error.Code`, and the `(provider, kind, id)` callee identity
> (`:68-72`).
>
> **Not compared:** the human `Error.Message`, `ExternalRef.URL` (dropped with a comment at `:130`),
> event timings, and the entire `Runner` map, which has no field on `NormativeEvent` at all and is the
> journal's one sanctioned non-normative annotation channel (`internal/journal/event.go:292-294`).
>
> The design does not keep far-side content out of the diff. It keeps far-side content out of *identity*
> fields, and confines what **is** compared to values a hermetic fixture can reproduce byte for byte.

`diffConformanceViews` compares the flat structs with `!=` (`internal/engine/conformance_test.go:487-499`)
and `TestConformanceDualRunnerJournalParity` (`:531`) runs every fixture through both runners. Three
consequences. **Any dual-runner fixture exercising this kind must drive a byte-deterministic fake**, since
two runners calling a real endpoint cannot produce an equal `RefDigest` or equal `Outputs` — not a new
burden, because `conformanceFixtures()` already drives every stage through scripted calls (`:184-222`).
**The recorded artifact must contain only reproducible bytes**: copying external-telemetry's
`ResultArtifact` verbatim would embed `QueryStarted`/`QueryEnded` wall-clock
(`internal/externaltelemetry/host.go:243-244`) and give a different digest on each tier even against a
byte-identical fake, so the call-out artifact carries the bounded body, status code, target and operation
names, the epoch and the request digest, while **timings, latency and the far side's correlation id are
not in the digested artifact** — they ride the `Runner` map and span attributes. The artifact *name* must
be deterministic too, since `NormativeEvent.Name` is normative (`:49`). And **§3.8's clamp increases
conformance coverage**, because `IsConformanceNormative` drops every `AttemptClass == AttemptInfra` event
outright (`internal/journal/event.go:407-409`), so rendering an out-of-budget `Retry-After` as a terminal
policy-class failure puts that outcome *into* the compared surface.

### 6.3 Callee identity, and the `ExternalRefs` channel

**D12.** `ExternalRef` already carries the ruling this design wants: identity is `(Provider, Kind, ID)`
and `URL` "is a convenience for humans and is not compared across runners"
(`internal/journal/event.go:376-384`), which the projection and schema both enforce. A target is
*declared* as a URL-shaped tuple in `instance.yaml` and *journaled* as structured identity — a deliberate
split. **`provider`** is the target's configured name, never a hostname, because it is the only input both
runners provably share and it is the derivation §4.3 requires for legibility. **`kind`** is the literal
`"callout"`, which must not be `"pr"` or `"branch"` since both are load-bearing sentinels
(`internal/runner/run.go:934`; `internal/readservice/status.go:87-93` counts a `ref.touched` with
`Kind == "pr"` and `Runner["operation"] == "open"` as the run's PR-open moment). **`id`** is the call
epoch, **never** the far side's correlation id, because `ExternalRefID` is normative
(`conformance.go:72`) and a far-side value there would make the dual-runner diff a test of whether a
third party is deterministic — which also makes the parity test the enforcer of epoch agreement between
the two walks (§3.3). The far side's correlation id lands in the artifact's non-digested provenance and a
span attribute.

**D13: `ref.touched` gets a real producer.** Today the only stage-level producer for a deterministic
stage is the `mutations.jsonl` workspace sidecar — `readMutationSidecar(env.Workspace)` at
`internal/runner/run.go:3108` on the success path only, with read failure downgraded to a best-effort
`mutation_sidecar_read_failed` event (`:3109-3120`) and the append itself explicitly best-effort ("a
failed Append here must not fail the stage", `:2501-2510`). Tier 3 reads the same sidecar inside the
activity (`internal/engine/activities.go:260`) and returns the facts on `stageActivityResult` (`:59-65`).
An in-process executor with a scratch workspace *physically can* write that file
(`createStageWorkspace` returns a real directory, `internal/runner/run.go:3927-3942`), but the sidecar's
own rationale says why it should not: it exists because provider subcommands "run as separate short-lived
processes with no legal journal access" (`cmd/goobers/mutationsidecar.go:13-22`), none of which applies
to a `KindExecutor` inside the runner's own process — and its record shape is a private struct duplicated
three times with no schema, parity test or version.

So `ResultEnvelope` gains `ExternalRefs []ExternalRefRecord`. The decisive argument is that **tier 3
already has this channel privately** — `stageActivityResult` embeds `ResultEnvelope` and adds
`Mutations`, which is exactly what the workflow turns into `ref.touched` — so promoting it invents no
mechanism, and the version bump is already paid by `CallEpoch`. Under a mutating v1 this is the audit
record of a real side effect on someone else's system, and §5.4's "journaled at every use" is a
governance promise: best-effort is defensible for a fact the runner also observed through an exit code
and a result file, and indefensible as the *only* record of a call the runner has no other evidence of.
Name it `ExternalRefs`, **not** `Mutations`, because `stageActivityResult` already has a `Mutations`
field with json tag `mutations` (`internal/engine/activities.go:63`) and Go's encoder would let the outer
field win silently. Tier 1 merges `result.ExternalRefs` into the slice `finishTaskDispatch` walks, with an
**explicit behaviour split written in the code, not implied**: sidecar-sourced facts stay best-effort, a
call-out-sourced record is a fatal append matching the `stage.finished` rule at `:2760-2772`. No event
type moves and `ref.touched` is already whitelisted (`internal/engine/projection.go:33`), so D11 survives.
Two rejected options are on the record: accepting best-effort and saying so (which makes §5.4's Visibility
row false as written), and reopening the conformance-excluded `externalcall.*` pair (which buys the
legible timeline of §9.3 for §6.1's four-way lockstep and does not fix this fault, since a new event type
still needs a producer).

### 6.4 The response body, and the integrity floor

The response is an artifact, never an inline event field: a single event line is hard-capped at 8 MiB
during recovery scanning (`internal/journal/reader.go:494-495`), and `events.jsonl` is a `cat`/`jq`-first
format a body would ruin regardless. Tier 1 uses the shipped path — a `BoundedArtifactRecorder`
(`internal/executor/shell.go:128-133`) with the bound applied *after* journal redaction at the boundary
that writes and digests (`internal/journal/run.go:501-514`) — subject to §6.2's reproducible-bytes rule,
with one byte ceiling enforced by the kind at the transport and the recorder bound as backstop.

**Tier 3 has a harder constraint the design states rather than discovers.** The projection's ops live in
deterministic workflow state served by a Temporal query, so **the body travels back through an activity
result into Temporal history before it can become a journal artifact**, and again through the projection
query; there is no path that avoids this. Hence the body is scrubbed at the history boundary as well as
the journal boundary (`scrubStageActivityResult`, `internal/engine/activities.go:266-281`, precisely so
"history and the later projection commit the same bytes and therefore the same digests"), which only
works once P0-c lands. And the tier-3 artifact channel is unfinished: the worker-side staging recorder
exists only on the spike (`spike internal/workerhost/artifacts.go:41-83`, its own doc naming the gap —
"it has no channel for executor-produced bytes"), `main/internal/workerhost/` has no `artifacts.go`, and
`internal/blobstore` plus the whole projection are dead-code-exempted pending the worker slice
(`test/deadcode/exemptions.txt:274-281`, `:171-179`). Because that gate rejects *stale* exemptions, the
change making any of it reachable deletes the matching lines in the same commit.

**D15: the integrity floor (P0-d).** Rev 1 claimed the artifact "and every projected output" are recorded
`IntegrityUnapproved` in the executor, so `producedIntegrity`'s trusted default "never applies, by
construction". The artifact half is true; the conclusion is false; and the outputs half **cannot** be
true, because `ResultEnvelope.Outputs` is `map[string]interface{}` with no per-key grade — the
`Integrity` field's own doc comment says outputs "are bare scalars with nowhere to hang provenance, so a
downstream stage resolving `inputsFrom` grades the producing stage"
(`api/v1alpha1/envelope.go:230-237`). An executor has exactly one lever on outputs, the stage-level
`Integrity`, and the runner throws it away at `internal/runner/run.go:2754`, recomputing from
`producedIntegrity` — which takes the stage's *inputs* (its recorded artifacts are not an argument) and
returns `IntegrityTrusted` for an empty grade list (`internal/runner/inputsfrom.go:352-354`, pinned by
`internal/runner/inputsintegrity_test.go:128`). A call-out in its common shape hits that branch, and
downstream `inputsFrom` admission grades a bare scalar by the producing stage's `Integrity` alone
(`inputsfrom.go:95-102`, checked at `run.go:2540-2544`), so the far side's strings satisfy any declared
`minimumIntegrity`. The engine does the same one frame earlier (`internal/engine/engine.go:471`, assigned
at `:479`/`:497`).

> **The rule.** A stage's produced `Integrity` is the weakest of: the grade `producedIntegrity` derives
> from its admitted inputs; the grade of every artifact the stage recorded on this attempt; and the grade
> the executor declared on the returned envelope, when it declared a valid one. Invalid or empty terms
> are skipped, never treated as a grade. The floor is applied after artifact normalization and before the
> `stage.finished` append, identically on both tiers.
>
> Consequently a call-out finishes `unapproved` because it **recorded an unapproved artifact**, not
> because the runner knows what a call-out is. Any future kind recording unapproved bytes inherits the
> grade with no code change.

Flooring rather than special-casing the kind is deliberate: a kind name in generic runner code is the
coupling D1 removed, and the floor repairs the **pre-existing** hole in provider-builtin stages, `ci-poll`
and external-telemetry (§1.6, P0-d) in the same line. ("Let the executor's grade survive" would let an
executor *raise* its output above its inputs, so it would have to be a floor anyway; `WeakestIntegrity`
can only move a grade down the ladder, so the change cannot introduce a laundering path.) On tier 3 the
assignment must move one frame out into `internal/engine/retry.go`, between `normalizeArtifactIntegrity`
(`:91`) and `rec.stageFinished` (`:94`) — required, not cosmetic, because today the closure assigns
*before* normalization, so flooring in place would floor against un-normalized grades. A one-tier fix
turns the parity test red, which is the feature. **Blast radius:** a changed grade only *refuses*
something downstream when a consumer binds that stage through `inputsFrom` **and** declares a
`minimumIntegrity` above `unapproved`, and in-tree that intersection is empty — `minimumIntegrity` appears
in exactly three workflow files (`reference-workflows/gaggles/goobers/workflows/implementation.yaml:113`,
`:169`, plus two `config-examples/acme-web*` copies) and none of those stages declares `inputsFrom`. The
observable cost is fixture regeneration and journaled grades that become more honest; operator-authored
configs outside this repo are not surveyable from here (§10).

**A mutating response carries the same grade: `unapproved`, on every leg.** The direction of the call has
no bearing on the provenance of what comes back, and a mutating target has *more* reason to describe its
own state favourably. Two consequences. The **ambiguous outcome is unapproved data, not absent data** —
whatever the far side reports about it is still far-side-authored; where the ambiguity is established by
*our* side (a `START_TO_CLOSE` timeout, a cancellation) the runner authors it and no far-side content is
involved, and the two are distinguishable in the journal for exactly that reason. And **the epoch is
ours, the receipt is theirs, and they must not share a grade slot**: since outputs cannot carry per-key
grades, a call-out's `Outputs` are uniformly `unapproved` and **the epoch is not re-exported as a
call-out output** — a later stage derives it from the same deterministic inputs or reads it from the
journal, rather than taking it back from a stage graded by the weakest thing it touched.

**The reserved-output set is a build-time constant, not an injected set.** The stock checks read **seven** well-known keys,
not four (`internal/gate/automated.go:114-320`): `status` (`:127`), `errorCode`/`errorMessage`/
`errorRetryable` via `failure-class` (`:129-141`, `:322-338`), `ciStatus` (`:278`), `landOutcome`
(`:291`), `queueOutcome` (`:308`) — all through three bare `inputs[key]` helpers with no namespacing. The
three rev 1 missed are the expensive ones: they have non-boolean outcome sets enforced at compile time,
and `ci-status` produces `timeout`, which reference workflows route to escalation rather than to the fail
branch's repass. They are also **permanently outside P0-a's scope**, because they are stage-authored by
design and reserving them inside `AutomatedInputs` would break every shipped `ci-status` and merge gate.
Nor does the integrity half reach them: `apiv1.Gate` has no `minimumIntegrity` field (it exists only on
`Task`, `api/v1alpha1/workflow_types.go:173-178`) and `AutomatedInputs` never reads `subject.Integrity`.
**A gate cannot refuse an input on provenance grounds**, so for those three keys the producing-side DSL
rule is the *sole* defense.

**Those keys are not operator data, so the rule takes no option.** They are the well-known input keys the
build-time-constant check registry reads: `status`/`errorCode`/`errorMessage`/`errorRetryable`
(`internal/gate/automated.go:36-39`, stamped at `:99`, `:103-109`) plus the three bare literals `ciStatus`
(`:278`), `landOutcome` (`:291`), `queueOutcome` (`:308`). Nothing about that set varies per instance. The
only reason an earlier revision injected it as `WithReservedOutputKeys` was a package-layout accident, and
the tree says so in as many words: `internal/gate/evaluate.go:11` imports `internal/workflow`, so
`internal/workflow` cannot import `internal/gate` — the comment at `v_next/checks.go:175-179` records
exactly this and calls its own `checkEqualsVocab` table "intentionally kept in sync … by hand". **Fix the
layout, not the rule**, by the same move §2.3 makes for the kind vocabulary via `internal/boundedwait`: a
new leaf package `internal/gatekeys` importing nothing in-tree, holding the seven constants and
`Reserved()`; `automated.go:36-39` re-declares its four exported `InputKey*` consts as aliases (untyped
string consts, so no call site changes) and the three literals become `gatekeys.CIStatus` /
`gatekeys.LandOutcome` / `gatekeys.QueueOutcome`; both interpreters import it. `WithReservedOutputKeys` is
**deleted before it is written**, along with the fail-closed-on-missing-option inversion an earlier
revision proposed — an option-gated compile rule is skip-when-unset by construction (§2.4), so inverting
one would have made five of six admission surfaces refuse every call-out.

What keeps the constant honest is an **AST drift gate**, unchanged in substance and still living in
`internal/gate` (which may import both): parse `automated.go` and require every literal-or-resolvable-const
key argument of a `stringField`/`outputStringField`/`numericField` call inside a `DefaultChecks` entry
(`:117-345`, helpers at `:347`, `:358`, `:373`) to appear in `gatekeys.Reserved()`. A new check reading a
new well-known key cannot compile green without widening the set, and widening it now reaches every compile
root instead of one. In-tree precedent for the shape: `v_current/schema_enum_registry_test.go`,
`cmd/goobers/provider_capability_manifest_test.go`, `test/stagenamelint/main.go`. **Honest residual:** this
retires the *key* vocabulary only. It does not retire the four hand-synced *outcome* shadows
(`automatedCheckOutcomes` and `checkEqualsVocab` in both interpreters); it adds no fifth shadow, and it
leaves the leaf seam those four could later move to (§10). `noWork` is deliberately **not** in the set:
`ShellExecutor` reads it back out of a result file it wrote (`internal/executor/shell.go:103`, consumed at
`:719`), unreachable from a `KindExecutor` building its own envelope.

### 6.5 Tracing: three rungs, v1 promises the middle one

| Rung | What it is | What it needs | v1 |
|---|---|---|---|
| **Floor** | The run id crosses the boundary; the far side's correlation id comes back and is recorded | nothing — already true | **Promised** |
| **Middle** | A W3C `traceparent` from a `SpanKindClient` span | a `SpanContext` accessor on `telemetry.Span`; a client-owned `propagation.TraceContext` | **Promised** |
| **Ceiling** | Far-side spans stitched into the run's trace | an OTLP receiver, a relaxed trace-id invariant, tier-3 span emission | **Not promised** |

**Floor.** The run id *is* the trace id in both directions: `NewRunID` mints a valid 16-byte OTel trace id
(`internal/telemetry/client.go:137-149`), `parseTraceID` refuses any run id that is not one
(`internal/telemetry/id.go:46-55`), and `StartRun` forces the root span onto it. So the cheapest possible
contract for a receiving side is: echo back the 32-hex run id we sent, plus your own correlation id — no
OpenTelemetry required of the far side. The returned id goes into the artifact's non-digested provenance
and onto the span, registered first in the canonical attribute registry
(`internal/telemetry/attributes.go:5-8`); it is deliberately not `ExternalRef.ID` (§6.3).

**Middle** is a from-scratch build, not a configuration flag, needing three additions in phase 2.
`telemetry.Span` cannot produce a span context — it wraps an unexported `trace.Span` and exposes nine
methods, none an accessor (`internal/telemetry/span.go:16-127`) — so this design adds one; the live OTel
context is otherwise in reach, since the runner threads the task span's context into `det.Run`. No
propagator exists anywhere (a repo-wide grep of both checkouts finds no `otel/propagation` import and no
`traceparent` in any Go file), so a `propagation.TraceContext` is owned by `telemetry.Client` and **never
registered globally**, because `telemetry.New` deliberately does not mutate global OTel state and a
regression test guards that. And `SpanKindClient` will be visible at the collector and invisible in the
journal: `SpanRecord` has no OTel kind or links field and derives `Kind` from the span *name prefix*
(`internal/telemetry/journalspan.go:223-237`, `:284-296`), so the change adds a `callout` prefix, a
matching `SpanKindCallout` constant beside the existing four (`internal/telemetry/attributes.go:104-111`)
and the `spanRecordKind` arm — otherwise the rollup's kind-filtered queries break, and a span carrying
agent provenance whose kind is neither `task` nor `gate` makes `insertAgentInvocation` abort the whole
run's rollup ingest (`internal/telemetry/rollup/ingest.go:970-981`). **One span per stage with
per-attempt events, not a span per attempt**: nothing samples (no `WithSampler`/`ParentBased` call in
either checkout), so a retried call-out must not dominate a run's span volume.

**Ceiling** has four independent blockers, any one sufficient: there is no OTLP receiver (`pdata`'s
`UnmarshalTraces` is called in exactly two non-test places, both re-reading our own bytes); a
foreign-trace span is silently dropped on the way to the journal, because `JournalSpanExporter.writeGroup`
stats `runs/<traceID>/run.yaml` and returns `nil` when it does not exist
(`internal/telemetry/journalspan.go:122-125`); the trace-id-equals-run-id invariant actively rejects
foreign parents (`StartRun` opens the root `WithNewRoot()`, and `contextWithRunTraceID` errors when a
valid parent's trace id differs, `internal/telemetry/id.go:34-44`); and tier 3 emits no spans at all, since
`internal/engine` has no telemetry import on either branch and the spike's worker builds its runner config
with an explicitly nil telemetry client. That last is #2865, with a consequence rev 1 got backwards:
**while a tier-3 call-out is in flight it is observable in no Goobers surface at all.** There is no
journal on tier 3 until the run closes — `validateProjection` requires the last op to be a terminal
`run.finished` (`internal/engine/projection.go:171-174`) and `ProjectCompletedRun` queries a *closed*
workflow's history (`:244-251`). A bounded-wait call-out is precisely the case where completion-time
projection is least useful, so **this design records as a stated assumption that completion-time
projection is acceptable for v1** — #2865's live-vs-projected question is an open human decision, and
this design is built on an undecided contract rather than a resolved one (§10).

### 6.6 Disclosure: propagating the run id is a policy statement

Emitting the run id — or the `traceparent` containing it — exports a Goobers run identifier across an
organizational boundary, consciously rather than as a side effect. The alternative is closed: #2894 is
explicit that "the existing run ID already equals the Temporal workflow ID and is a valid trace ID. New
emitters must reuse that identity and fail loudly rather than silently minting an unrelated trace ID."
What goes out is an opaque random value — `NewRunID` draws 16 bytes from `crypto/rand` — carrying no
repository, gaggle, workflow or tenant name, so what it discloses is *correlation across our own calls*,
not content. Two operational consequences belong in the target contract: a target that logs request
headers retains the identifier, and two call-outs from one run are linkable by whoever receives them.
Propagation is therefore per-target (`sendTraceContext`, default on), the same per-connector posture the
telemetry network policy already uses, and the epoch's digest form (§3.3) keeps the disclosure to exactly
this one decision. `no-phone-home` is not the control here and never was (§5.5): it governs destinations,
not header content.

## 7. Security model and threat analysis

The transport is the easy part. The risk is the **return leg**: bytes authored outside the instance
travelling inward through paths this codebase built for its *own* stages — the flattened gate map, the
downstream context-pointer set, the integrity grader, `Outputs`, and on tier 3 Temporal history. Under D5
there is a second axis: an outbound leg that changes someone else's state.

**The boundary, stated once.** Outbound: a target *name* resolved from `instance.yaml`, a declared
parameter set, an operator-declared method, a credential materialized in-process from a `TokenRef`, a
call epoch, and a `traceparent`. Inbound: an HTTP status, a body recorded as an artifact, and a declared
projection. Everything the far side returns is **unapproved input** in the SEC-047 sense
(`docs/requirements/security.md:90`) — indistinguishable in kind from an issue body written by a
stranger. *A call-out response is never authorization, never trusted, never unconstrained in shape.*

| # | Threat | Mechanism | Disposition |
|---|---|---|---|
| 1 | **Gate-verdict forgery** via an output colliding with a well-known gate key | `AutomatedInputs` writes `status` before copying the subject's outputs over it (`internal/gate/automated.go:99-102`); `ciStatus`/`landOutcome`/`queueOutcome` are read from the same flat map with no namespace. Exploitable **today** by any agentic stage, so this feature changes the attacker set, not the primitive | **Closed on the outputs door; on the error door, closed only by a constraint on the kind.** P0-a re-stamps the runner-owned keys after the copy — of those, only `status` was ever forgeable through outputs — and XC-A5 stops a call-out projection expressing any of the seven. The error keys are a *second* door: `AutomatedInputs` stamps `errorCode`/`errorMessage`/`errorRetryable` from `subject.Error` **after** the copy (`internal/gate/automated.go:103-110`), and `failure-class`, a fixed-vocabulary check, forks on `errorRetryable` alone (`:136-137`) into a compile-required `infra` branch — so a far side answering 4xx with `{"retryable": true}` would choose which branch a gate takes. Closed only because the kind never lets the far side write that field: `Retryable` is constant `false` and `Code` comes from a closed `callout_*` vocabulary (§3.6). **Residual: `errorMessage` still carries far-side text**, and `output-equals` and its five siblings read whatever key the *gate author* names — including `errorMessage` — which remains open by design |
| 2 | **Prompt injection** into downstream agentic stages | `contextPointersFor` broadcasts every artifact to every downstream stage with no declaration (`internal/runner/run.go:4311-4323`), and the default grader would launder the response as `trusted` (§6.4) | **Closed for stages once P0-d lands**, plus the compile rule requiring `minimumIntegrity` on an agentic consumer selecting the artifact via `contextFrom`. Residual: the undeclared broadcast path (§9.6), and **gates have no provenance floor at all** |
| 3 | **Kind substitution at runtime** | The overlay copies any upstream output into `env.Inputs` under any key and dispatch selects the executor from that map | **Closed for `external-call`**, reachable only through the typed field. D2 makes the binding an error in v_next; what remains open is 1.4 documents on the frozen interpreter (§9.7) |
| 4 | **SSRF / credential cross-delivery** | §5.3's four defects in the inherited allowlist | Target = scheme+host+port+path; **one credential per target, made unrepresentable by D-C1..3**; redirects re-enter the check. Dial-time IP pinning is a substrate control arriving with #1307/#2898 — v1 ships without it and says so |
| 5 | **Duplicate far-side effect** *(new under D5)* | At-least-once delivery through five re-execution paths, none opt-out, with no compensation hook (§3.6) | The epoch + the receiver's asserted dedup contract + reconcile-on-`START_TO_CLOSE`. **Residual: the contract is an operator assertion Goobers cannot verify**, and a watchdog takeover orphans the call outright |
| 6 | **Far-side availability denial** *(new)* | A hostile `Retry-After` parks a stage past its declared timeout — on tier 3 as a durable timer with no workflow-level timeout at all | Clamped in the kind, fail-fast rather than truncated (§3.8). Residual: the runner-level path stays unclamped for every other producer |
| 7a | **`envPassthrough` unchecked for capability grants** | §4.6 | P0-b |
| 7b | **Tier-3 nil scrubber; unscrubbed activity arguments** | §1.6 | P0-c, which **gates tier-3 dispatch**. Two consequences bear restating: the fallback net is seven regexes matching no opaque vault secret, and because only *return values* are scrubbed, what may appear in `env.Inputs` must itself be constrained — a target **name**, never a URL with embedded identifiers |
| 7c | **Registry 6-byte floor, per-attempt freshness** | `Register` ignores values shorter than `minSecretLen` (`internal/journal/scrub.go:51-58`), and the registry is rebuilt per attempt from that stage's own credentials | Structural: provenance carries a request digest and parameter **names**, never values, headers or bodies; a denied URL is reported through `url.Redacted()` |
| 7d | **`cmd/goobers` does not scrub its own stdout** | `main` writes straight to `os.Stdout` (`cmd/goobers/main.go:18-21`); `scrubbedWriter` applies only when a scrubber is supplied, which `cmd/scheduler` and `cmd/goober-runtime` do and the instance CLI does not | All call-out diagnostics route through the journal, never daemon stdout. Fixing the wrapper is desirable and not a blocker, because this feature does not use that path |
| 7e | **Shared `GH_TOKEN` collapse** | `buildEnvCapabilities` (`cmd/goobers/runnerwiring.go:368-372`) | Dedicated in-process credential, never in a child env; §4.5's kind restriction stops a shell stage obtaining it |
| 8 | **Tier-1 ambient-env blast radius** | Every advertised confinement mechanism shapes subprocesses; an in-process kind sits outside all of it (§4.7) | Compiler rejects `network: none`; credentials only via the Injector; P0-c gates tier 3 |

**What this design does not protect against.** *A compromised operator config* — CT-1 is a named
assumption and §4.3 walks what breaks, including the post-failure primitive this feature adds. *Egress
enforcement* — v1 constrains where a call-out may go, not where anything else in the process may go; that
is #1307/#2898. *A far side that lies within the contract* — every mitigation constrains *shape*, and
nothing detects well-formed, correctly-typed, entirely false data; that is a trust decision made when the
operator configures the endpoint, and it is why projected outputs are graded `unapproved` even when they
are scalars, since the grade records the *source*, not the plausibility of the value. *A far side that
ignores its own idempotency assertion* — under D5 the largest single residual, whose terms §3.6 states
plainly.
## 8. Implementation plan and follow-up filing

Four phases, sequenced by what each makes *provable*. Every phase is independently mergeable and
independently useful: phase 0 improves shipped behaviour even if this design is never built, and phase 1
lands TBH-1's typed `run.kind`, which the trust-boundary work needs regardless. Nothing in phases 1–3 may
be filed as an implementation issue before the governance step in the filing table completes.

**Per this repo's convention a design deliverable is a design doc plus gated follow-up issues, and the
per-phase acceptance-criteria checklists live in the issues, not here.** Each phase below names in one
sentence what its issue must carry; the filing table records which issue carries which phase.

| Phase | Ships | Provable when it lands |
|---|---|---|
| 0 — Prerequisites | P0-a, P0-b, P0-c, P0-d (§1.6) | Four regression suites, no call-out code in the tree |
| 1 — Surface | Typed `run.kind`, `run.externalCall`, the kind-vocabulary constant, `internal/gatekeys`, XC-A1…XC-A7, preview gating, the scoped capability, `externalCallTargets` config and its refusals, the daemon cross-reference pass | A workflow compiles or is refused; `goobers explain` describes the field; no bytes leave the process |
| 2 — Transport | `internal/callout` host + `KindExecutor`, the epoch and its ledger, self-enforced deadline, mandatory progress, declared projection, `Retry-After` clamp, per-target budget, correlation + `traceparent` | Unit suite against an httptest target; live smoke against the proving rig |
| 3 — Tier 3 | The same executor on the Temporal worker, the `TimeoutType` split, reconcile mode, heartbeats | Dual-runner conformance fixture through `make test-conformance` |

**Prerequisite ordering.** Phase 0 precedes everything and its four items are separately mergeable defects
in shipped behaviour (§1.6). Phase 1 is deliberately inert — declaration, compile rules, schema,
capability and instance config, dispatching nothing — which makes the security-relevant half reviewable on
its own. Phase 2 is a new `internal/callout` package modelled on `internal/externaltelemetry`: a
host-owned client whose `RoundTripper` refuses any URL the target denies, an explicitly declared proxy,
dial-time address checks with per-target opt-ins, credential refs resolved out of band, the per-target
ceilings, the epoch ledger and the budget ledger. Phase 3 is not a second implementation — registering the
kind once serves both tiers — it is the retry, heartbeat and journal semantics that differ, and it cannot
start until §1.6's four tier-3 landings are in place.

**What each phase's issue must carry.**

- **Phase 0** — the four regression suites of §1.6, one per prerequisite, each asserting the repaired
  behaviour on both tiers where the defect spans both, and P0-d's proving that the integrity floor is
  general rather than kind-shaped.
- **Phase 1** — the compile-surface criteria: a per-root admission table test proving XC-A1…XC-A7 hold
  across all six surfaces **with no options passed**, a kind-vocabulary parity test, one refusal test per
  diagnostic with a passing control case, config-load refusals asserted against `(*Config).Validate()`
  rather than the JSON schema, §6.4's AST drift gate, the housekeeping regenerations, a test whose name
  records that target existence is a daemon cross-reference and not a language guarantee (§2.4), and a
  **determinism guard pinning that the in-workflow re-compile at `internal/engine/engine.go:204-207` is
  called with a compile option set containing nothing instance-derived**, commented with replay as the
  reason.
- **Phase 2** — the transport criteria against an httptest target: policy and network boundary, the epoch
  suite **including §3.3's three concurrent-branch ledger tests and excluding the withdrawn "two branches,
  one task name" fixture that fan-out rule 1 refuses to compile**, the `Retry-After` clamp under an
  injected clock, budget refusal before any request is issued, the journal-read integrity assertions in
  the laundering shape with their downstream-`minimumIntegrity` twin, the projection contract matrix, and
  timeout coherence in **both** interpreter copies — plus the environmental facts an implementer must not
  rediscover: the hermetic unit runner does not block network syscalls (`test/hermetic/main.go:425-463`),
  `.golangci.yml:10-20` enables neither `bodyclose` nor `noctx`, the live smoke must satisfy the four
  structural rules the integration orchestrator enforces by AST scan, and `internal/callout` must either
  join the Windows curated package selection (`.github/workflows/ci.yml:539-559`) or have its absence
  recorded deliberately.
- **Phase 3** — the tier-3 criteria: the timeout-type split and reconcile flag built from exported SDK API
  (`temporal.NewTimeoutError`) with a non-call-out control, the closed `callout_*` error vocabulary pinned
  against the three colliding sentinels, replay deriving an identical epoch sequence with the cross-runner
  `runner.calloutEpoch` comparison stated as **supplementary** (§3.3), the dual-runner conformance fixture
  against a hermetic fake read for what §9.8 says it proves, and `HeartbeatTimeout`/`RecordHeartbeat` on
  the Temporal path.

**The live proving rig** is an Azure Container App in **one dedicated resource group**, so teardown is a
single `az group delete`: `minReplicas: 0` for genuine scale-to-zero (a bounded wait sized for a warm
endpoint fails on a cold one, and submit-then-poll is the shape that survives it), real managed TLS —
the one thing the unit tier structurally cannot prove — and a bearer token provisioned out of band,
exercising the same `TokenRef` path production uses. Modes selected by request path:

| Mode | Far side does | Proves |
|---|---|---|
| `/cold` | first call after scale-to-zero | the budget survives a start-up spike; heartbeats fire, so the 45m watchdog does not terminalize a healthy slow call |
| `/slow` | sleeps past the declared budget | the self-enforced deadline fires with margin and a typed timeout crosses the stage boundary |
| `/oversize` | body far larger than `response.maxBytes` | the bounded read refuses without buffering; truncation is journaled, not silent |
| `/wrong-type` | 200 with `text/html` | a contract violation is a stage failure, not a parsed output |
| `/5xx`, `/4xx` | 503 with a sane `Retry-After`; 403 | bounded retry vs. terminal refusal |
| `/retry-after-abuse` | 503 with `Retry-After: 86400` | the stage fails **inside** its budget rather than parking (§3.8) |
| `/extra-fields` | 200, valid JSON, undeclared fields | undeclared fields are dropped; nothing is merged wholesale |
| `/lost-response` | accepts a submit, records the epoch, drops the connection | **one** far-side unit of work, a reconcile read, and a `success` — never two units of work |

**Follow-up issues.** Items 2–4, 7–9 and 11 are ordinary engineering and can be filed on this document's
merge; items 1, 5, 6, 10 and 12 require the owner's decision, and item 1 gates the rest; items 13–15 are
the implementation issues themselves.

| # | Issue | Scope | Gate |
|---|---|---|---|
| 1 | Promote #1087, fold #2896 | Make #1087 the surviving owner, merge the closed duplicate's content **including its two stale-fact corrections** (§1.2), and file the v1 call-out as its first child on *Custom & Generic Stages* | **Owner approval required** |
| 2 | P0-a…P0-d | Four separate PRs (§1.6). Each is a defect in shipped behaviour needing no design approval, and **must not slip with this feature** if it is deferred. **Carries the acceptance criteria for phase 0** | File now |
| 3 | Feature-router symmetry | Route the five `vcurrent`-pinned functions through `interpreterForDefinition` (D4) | File now; a hard prerequisite for any later goober-scoped call-out surface |
| 4 | Extend ADR 0002 | The third capability spelling (§4.5) | File with phase 1 — the vocabulary is published in five schema enum sites, so renaming later breaks five schemas plus every author's YAML |
| 5 | SSH transport | A second transport, designed separately; record that the merge gate is blind to it (D10) | **Owner approval required** |
| 6 | Async return leg | The callback shape, gated on #648; explicitly rejects a special ingress/egress stage pod | **Owner approval required**, blocked on #648 |
| 7 | Consume enforced egress (#1307, #2898) | Cross-link, not parallel machinery (§5.3) | No approval needed; v1 requires neither |
| 8 | Instance-wide call attribution | Spend across targets, and a tie into `RunConditions`' budget vocabulary (`internal/instance/config.go:1007-1012`, which bounds runs per hour/day, not external calls). **Filed against UNOP-5, which §1.5 records as partially deferred** | File now |
| 9 | Stitched tracing | Adopt far-side spans; widen `SpanRecord` to persist OTel kind and links | Blocked on #2865; also needs an OTLP receiver, which does not exist |
| 10 | Per-workflow target isolation | The `allowedWorkflows` allow-list, enforcement point already identified (§4.4) | **Owner approval required** |
| 11 | Tier-3 worker runtime wiring | Cross-link **#2904**, which owns the extraction §1.6's tier-3 gate depends on | Already approved and ready |
| 12 | Open the routing-key vocabulary | #2896's own recommendation, carried through the fold: do this *before* adding target kinds | **Owner approval required**, gated on 1 |
| 13 | Phase 1 — surface | The DSL, compiler, capability and instance-config change, dispatching nothing; the §2.6 lockstep is one commit. **Carries the acceptance criteria for phase 1** | Blocked on 1 |
| 14 | Phase 2 — transport | `internal/callout`, the epoch ledger, the deadline, the progress invariant, the projection, the clamp, the budget and correlation. **Carries the acceptance criteria for phase 2** | Blocked on 13 |
| 15 | Phase 3 — tier 3 | The Temporal binding: the `TimeoutType` split, reconcile mode, heartbeats. **Carries the acceptance criteria for phase 3** | Blocked on 14 and on §1.6's four tier-3 landings |

## 9. Residuals recorded during drafting

Each is something the next author would otherwise rediscover the hard way. None changes a decision in
§1.3.

**9.1 — Nine of nineteen lockstep artefacts are ungated or weakly gated.** `zz_generated.deepcopy.go` has
no regenerate-then-diff step; the `StageContractVersion` bump is pinned by no test; an omitted
`capability.All()` entry surfaces only indirectly; `instance.schema.json` has no runtime enforcement at
all; the two envelope builders staying in parity is unasserted; the `credentialGrantKey` and config-load
rules fail only at runtime; the nil `Progress` wiring is invisible until a slow far side; and
`docs/stage-contract.md` and the DSL-author skill have no drift gate. Reviewers must check these by eye,
and the implementation issues must say so rather than leaning on a green build.

**9.2 — The mandatory progress hook is enforceable on tier 1 only.** The engine installs no progress
reporter, so `invoke.ReportProgress` is a no-op inside an activity, and no stage activity sets
`HeartbeatTimeout`. Reconcile mode (§3.6) makes `START_TO_CLOSE` survivable without a heartbeat, but
worker loss is still detected only after the declared limit plus the five-minute grace, or the one-hour
default. The objection is to shipping on tier 3 without also adding `HeartbeatTimeout` and
`RecordHeartbeat` — a small self-contained change to `stageActivityOptions` — because doing it later means
the first mutating call-out inherits a tier-3 path whose only failure detector is an hour-scale timeout.
Whether that latency is acceptable for a mutating call is a scope ruling (§10).

**9.3 — Overloading `ref.touched` has a legibility cost, now smaller than it was.** `ExternalRef`'s
documented meaning is "an external reference the run touched — an issue or PR in a provider", the portal
renders the row as "External reference touched", and `readservice` classifies it as a *result* only when
its kind is `pr`. Under read-only v1 that reading was misleading; under D5, "a mutating call-out appears
in the timeline as an external reference having been touched" is **literally true**. The alternative — a
typed `externalcall.*` pair, conformance-excluded and whitelisted in the same commit — buys a legible
timeline for §6.1's four-way lockstep. This design still declines it, but the argument for it is stronger
than rev 1 recorded (§10).

**9.4 — An ambient proxy defeats the tier-1 in-process claim.** `http.DefaultTransport` is
`Proxy: ProxyFromEnvironment` (Go 1.26.5, `net/http/transport.go:46-57`) and is what the existing policy
client uses, and the daemon inherits `HTTP_PROXY`/`HTTPS_PROXY` from whatever shell started it — so a
call-out could silently route through an undeclared, unjournaled proxy that sees the credential on `http`
and the CONNECT host on `https`, and a `DialContext` IP check would inspect the *proxy's* address and
pass. Fixed in-change: the transport sets `Proxy` explicitly from the target declaration.

**9.5 — CT-1's second clause is not true of the shipped tier-1 default.** "Under sandboxing they lack the
reach" describes a posture that is not the default and does not cover deterministic stages at all:
`Config.Sandbox` absent or zero-valued is disabled (`internal/instance/config.go:121-125`), and where
enabled it wraps a harness argv, so a deterministic shell stage receives no filesystem confinement on any
platform — while a stage whose command is the `goobers` CLI is handed `GOOBERS_INSTANCE_ROOT` and runs as
the daemon user. So CT-1 holds on tier 1 by operator convention rather than by any mechanism in the tree.
That is defensible for a single-operator instance and is not a reason to change the decision; it *is* a
reason to carry CT-1 with an explicit precondition — TBH-3's sandbox-on-by-default as the discharging
mechanism, and in the interim a load-time check that the instance root and `instance.yaml` are not
writable by the account stages execute as, in the same fail-closed shape `secfile.VerifyPrivate` already
applies to token files.

**9.6 — Grading the response `unapproved` is a refusal mechanism only where a consumer declares a
floor.** `contextPointersFor` still broadcasts every artifact to every downstream stage with no
declaration. The compile-time `minimumIntegrity` rule covers stages selecting the artifact via
`contextFrom` and does not cover the undeclared broadcast path, so a response is visible as context to
agentic stages that never asked for it, correctly labelled, and a stage with no declared floor will read
it. Closing this properly needs explicit data-flow declaration in the DSL. **This design lowers the grade
honestly; it does not narrow the fan-out.** And it reaches gates not at all (§6.4).

**9.7 — What the legacy dynamic-kind path still permits, stated precisely.** The far side **cannot**
select `external-call` — it is never registered as an `inputs.kind` value and the typed field is not in
the map. What a far-side-chosen value can do, in a 1.4 document on the frozen interpreter, is choose among
registered legacy kinds per downstream stage: flip a `shell` stage to `ci-poll` so its declared command
never runs, flip a `ci-poll` stage to `shell` so its otherwise-inert placeholder command *does* run, or
name an unregistered kind and burn the infrastructure budget on a dispatch failure. That is third-party
control over executor selection — not a new class, since an agentic stage's model-authored `result.json`
can do the same today, but it moves the chooser from our own model to an arbitrary endpoint. D2 closes it
at DSL 2.0; what remains open is 1.4.

**9.8 — The phase-3 conformance criterion proves less than its name suggests.** It diffs journals over
`ConformanceView`, and a hermetic fake is the only defensible fixture target, so it proves that the
*dispatch and journal shape* agree across tiers — genuinely what the conformance contract is for — and
proves nothing about the transport. The live rig is the only transport evidence and is a non-required
scheduled leg by design. Read the acceptance row as "tier 1 and tier 3 record the same run", never "tier 1
and tier 3 make the same call".

**9.9 — The infrastructure retry budget is a hard-coded 2 shared by both tiers**
(`internal/runner/run.go:56-58`, `internal/engine/retry.go:57`), and those attempts are excluded from the
conformance set by construction. Authors expecting `Task.Retry` to cover transport flakiness will be
wrong. Surfaced, not objected to.

**9.10 — "2.0 preview, not 2.1" makes `dslVersion` necessary but not sufficient** to determine whether a
document loads, and nothing in the tooling says so: two binaries both declaring 2.0 accept different
languages. Three things make it defensible — the failure is loud and coded (an unknown property under
`additionalProperties: false` is a schema violation at load, not a misinterpretation), the preview gate
closes the feature by default even on a binary that understands it, and the real compatibility key becomes
`Feature.SinceVersion`, a *release* version. If the project regrets this, the remedy is the 2.1 bump this
design declined, and its migration edge is trivial.

**9.11 — NetworkPolicy enforcement is CNI-dependent and fails silently**, as
`internal/k8spreflight/checks.go:62-80` already hints. The tier-3 binding must be preflight-verified with
a probe pod, never inferred from a successful `apply`.

## 10. Open questions for the owner

Decisions this design pass could not make. None blocks the document; each blocks something named.

**Scope rulings on the mutating contract.**

1. **Is tier-3 heartbeating a v1 prerequisite?** Reconcile mode makes `START_TO_CLOSE` survivable without
   one, but worker loss is detected only after the declared limit plus the five-minute grace, or the
   one-hour `activityTimeout` default (`internal/engine/engine.go:44`), and no stage activity sets
   `HeartbeatTimeout` on either checkout. Accepting that latency for a *mutating* call is a ruling (§9.2).
2. **Should an operator rerun advance the epoch?** §3.3 rules that it does not, because
   `startAttempt = int32(rerun.attempt)` continues one dispatch's attempt numbering. The two readings
   differ exactly on whether a human may convert a possibly-committed call into a certainly-duplicated one.
3. **Numeric defaults for `maxCallsPerHour`/`maxCallsPerRun`**, which §3.8 deliberately leaves unset =
   unbounded with a validate-time warning rather than picking numbers.
4. **Is tier-3's per-worker-process budget ceiling acceptable** (N workers ⇒ N × `maxCallsPerHour`)? A
   genuinely global bound needs a shared store, which §1.5 excludes, so accepting it is a scope ruling.
5. **Is an instance-wide `Retry-After` clamp wanted**, or only the kind-level clamp specified here?
   Instance-wide means a new stage-deadline parameter on both copies of `infrastructureRetryDelay`
   (neither call site has one) plus a behaviour change for the shipped provider path that
   `TestInfrastructureRetryWaitsUntilDeclaredReset` currently pins.

**Sequencing and blast radius.**

6. **Does P0-d ship as a fourth phase-0 prerequisite or as part of phase 1?** It repairs a live defect
   independent of this feature, arguing for its own timeline like P0-a — but it is a two-tier change
   requiring conformance-fixture regeneration.
7. **Is demoting existing provider-builtin, `ci-poll` and external-telemetry stage grades from `trusted`
   to `unapproved` acceptable as a silent behavioural change?** In-tree the blast radius is provably nil
   (§6.4), but operator-authored configs outside this repo are not surveyable from here — this may need a
   release note or a one-release deprecation window.
8. **Should the same PR retire `automatedCheckOutcomes` and `checkEqualsVocab`** as hand-synced registry
   shadows? `internal/gatekeys` establishes the leaf seam that makes it possible — the *outcome*
   vocabularies could move the same way the *key* vocabulary just did — and four hand-synced tables are
   exactly the drift the reserved-key work exists to avoid repeating. But it touches the v_current
   interpreter, which nothing else here does, and §6.4 deliberately scopes this change to the keys.
9. **Should the general `capability.Known` loop be hoisted above the `goobers == nil` guard**
   (`v_next:472-474`) in this change or filed separately? Hoisting makes capability admission root-independent
   — currently three of six surfaces skip it entirely — but it can newly refuse a previously-accepted
   document inside replayed workflow code at `internal/engine/engine.go:204-207`. Recommended as a separate
   cleanup with a drain requirement.
10. **Does D2 survive the versioning guardian's review?** The argument rests on the v_next/v_current split
    being an acceptable expression of the deprecate window and on the #124 precedent. Measured blast
    radius in this repo is zero; the policy question is about other adopters' documents.

**Surface choices.**

11. **Is `callout:invoke@<target>` ever legal in `instance.yaml`'s `credentials:` block?** D-C2 requires
    exactly one grant there per target, which answers yes — but if a later revision moves the credential
    into the target's own `auth` block, the scoped key must be **actively refused** at
    `internal/instance/config.go:1630-1640`, because the schema enum there is decorative. Confirm D-C2 is
    the intended branch.
12. **Should `InvocationEnvelope.CallEpoch` be named for the call-out kind at all**, or generalised
    (`DispatchEpoch`)? The envelope is a general contract and a future idempotency-needing kind would want
    the same field; naming it narrowly is honest today, and renaming later costs another
    `StageContractVersion` bump.
13. **Confirm the frozen-interpreter refusal is the right author experience.** The shared kind
    vocabulary is version-blind by construction (the schema is the union across DSL versions and the
    interpreter does the gating — the arrangement `workspace: repo-readonly` already lives under), so
    XC-A1b adds a per-kind minimum DSL version and `v_current` refuses both `run.kind: external-call`
    and any `callout:invoke@…` grant, each naming `2.0` and `goobers fix --to 2.0` (§2.3, §4.6). That
    closes the safety question — none of the guards that make a call-out safe exist in `v_current` —
    but it is the one place this design touches the frozen interpreter, so confirm the freeze-policy
    reading: it is contract-preserving because it refuses a document no 1.4 author can have written,
    the field not existing in 1.4. This overlaps q10.
14. **Is D-C3's scoping right** (refuse duplicate token refs only when at least one capability is
    `callout:invoke@*`), or should the rule be instance-wide with an explicit opt-out? Instance-wide is
    stronger but would newly fail instances legitimately sharing one token across provider capabilities.
15. **Is deleting `auth.mode: none` (D17) acceptable?** It is the cheapest way to make authority real; the
    alternative is the per-target `allowedWorkflows` allow-list (§4.4), also cheap but a second mechanism.
    If the owner has a concrete unauthenticated internal service in mind, the call flips and
    `allowedWorkflows` lands in v1.
16. **Is the tier-1-only target-existence check acceptable**, or should the tier-3 registration root be
    taught to read `instance.yaml`? The latter is a real architectural change to a control plane that is
    env-var-configured by design and would still not reach the in-workflow compile at
    `internal/engine/engine.go:204-207`, so it buys registration-time checking only. Priced, not
    recommended.
17. **May the poll leg ever address the far side by a server-assigned id** rather than by our epoch? §3.2
    permits it only behind a typed, pattern-constrained response field validated before use; if that
    pattern cannot be expressed, the constraint hardens into an outright prohibition.
18. **Is completion-time tier-3 observability acceptable for a bounded-wait call-out?** #2865 carries the
    open human decision on whether tier-3 telemetry must be visible while a run is in flight, and this
    design is built on the assumption that completion-time projection is sufficient (§6.5) — precisely the
    case where it is least obviously true.
19. **Is the `StageContractVersion` bump to `v1alpha9` accepted** for `ResultEnvelope.ExternalRefs`? The
    bump is already required by the typed transport and the epoch, so the marginal cost is small — but if
    declined, the fallback is to accept best-effort `ref.touched` and say so plainly, which means
    rewriting §5.4's Visibility row and repeating that beside a governance control whose entire purpose is
    visibility.
20. **Is reopening the conformance-excluded `externalcall.*` pair worth §9.3's legibility gain** now that
    v1 mutates? This design says no, because it does not fix the producer gap and pays a lockstep whose
    last arm fails the entire tier-3 run projection — but the argument against it is weaker than it was.
