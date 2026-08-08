# Design: Sandbox & Tool-Adapter API — EvalSuite

> Status: **Finalized for implementation** (2026-08-08)
> Epic: [#2662](https://github.com/Agent-Clubhouse/Goobers/issues/2662) — EvalSuite: End-to-end workflow evaluation
> Issue: [#2665](https://github.com/Agent-Clubhouse/Goobers/issues/2665) — Sandbox & Tool-Adapter API + Cassette Format
> Companion spec: [`EVALS_CASSETTE.md`](./EVALS_CASSETTE.md)
> Builds on prototype research by mighty-hare (`EVALS_SANDBOX_API.md`, `EVALS_SCOPE.md`, preserved as research artifacts, not implementation)

## 1. Goal

Every scenario stage in an EvalSuite that calls an external tool or service goes
through a **tool adapter** instead of calling that tool directly. The adapter is
the single seam that lets a stage run in one of four modes — `real`, `mock`,
`replay`, `no-op` — without the stage's own logic knowing which mode is active.

This is what makes two other EvalSuite guarantees possible:

- **Deterministic replay**: the same scenario, run twice, produces the same
  tool responses, so a judge's verdict is reproducible and diffable across
  versions.
- **Shadow/dark runs**: a candidate workflow version can be evaluated against
  production-shaped inputs without ever reaching a real side-effecting system.

The adapter API is the contract; [`EVALS_CASSETTE.md`](./EVALS_CASSETTE.md) is
the storage format that `replay` mode reads from and `record` (a sub-behavior
of `real`, see §3.2) writes to.

## 2. Adapter modes

| Mode | What it does | When it is used | May cause real side effects? |
|---|---|---|---|
| `real` | Calls the actual production/staging endpoint. | Recording a new cassette; deliberate live-fire scenarios explicitly opted into. | **Yes** — this is the only mode that may. |
| `mock` | Returns a developer-authored static or lightly-templated response; no cassette lookup. | Fast unit-style scenarios where the exact tool payload doesn't matter, only the shape. | No. |
| `replay` | Looks up a cassette by request signature and returns its recorded response verbatim. | The default mode for CI, side-by-side comparisons, and shadow runs. | No. |
| `no-op` | Returns a fixed, adapter-declared inert response without touching a cassette or a real endpoint. | Tool calls whose side effect is irrelevant to the scenario under test (e.g. a notification adapter during a shadow run) but whose absence would otherwise fail the stage. | No. |

A stage selects its adapter's mode via `stages[].tool_mocks.<adapter_id>.mode`
in the EvalSuite DSL (`eval_schema.json`, §2663) — the same field name and
four-value enum as the wire-level `/adapter/invoke` request (§3.1), so the
runner forwards the DSL value straight through with no translation:

```json
{
  "name": "agentic_action",
  "type": "agentic",
  "tool_mocks": {
    "bank_api": {
      "mode": "no-op",
      "response": { "status": "ok", "tx_id": "mock-tx-123" }
    }
  }
}
```

`response` (and any other adapter-specific configuration, e.g. a mock
template) is a sibling of `mode` under the same adapter key — `tool_mocks`
itself stays an unconstrained object in the schema so each adapter can carry
whatever extra config its `mock`/`no-op` behavior needs without a schema
change per adapter.

Mode selection is **per adapter, per stage** — a single scenario may run its
`payments` adapter in `no-op` while its `catalog` adapter runs in `replay`.

`real` is the exception to every rule in this document — see §6 for why it is
gated separately from the other three.

## 3. Interface

The adapter API is a single HTTP/JSON endpoint. Each adapter (`bank_api`,
`catalog`, `notifications`, ...) is a distinct logical instance behind this
one contract, addressed by `adapter_id`.

### 3.1 Request

```
POST /adapter/invoke
Content-Type: application/json
```

```json
{
  "adapter_id": "bank_api",
  "mode": "replay",
  "request": {
    "method": "POST",
    "path": "/v1/transfer",
    "headers": { "content-type": "application/json" },
    "body": { "from": "acct_1", "to": "acct_2", "amount_cents": 4200 }
  },
  "metadata": {
    "run_id": "01JZ9K3XQMPLE0000000000000",
    "scenario_id": "wire-transfer-happy-path",
    "stage": "execute-transfer",
    "seed": 42,
    "shadow": true
  }
}
```

| Field | Required | Notes |
|---|---|---|
| `adapter_id` | yes | Stable identifier for the adapter instance. Matches the key under `stages[].tool_mocks` in the DSL. |
| `mode` | yes | One of `real`, `mock`, `replay`, `no-op`. |
| `request.method` / `.path` / `.headers` / `.body` | yes | The tool call the stage would have made directly. `headers` and `body` are adapter-defined shapes; the adapter, not the runner, understands the target tool's actual protocol. |
| `metadata.run_id` | yes | Ties the invocation to a runner execution for audit correlation. |
| `metadata.scenario_id` / `.stage` | yes | For audit trail and cassette provenance (§`EVALS_CASSETTE.md` §2). |
| `metadata.seed` | yes | Deterministic seed for this invocation; part of the cassette signature (§`EVALS_CASSETTE.md` §4). |
| `metadata.shadow` | yes | `true` when this invocation originates from a shadow/dark run. The adapter shim MUST reject `mode: "real"` when `shadow: true` (§6.1) — this is the primary enforcement point for shadow-run safety. |

### 3.2 Response

```json
{
  "status": "ok",
  "mode": "replay",
  "response": {
    "status": 200,
    "headers": { "content-type": "application/json" },
    "body": { "tx_id": "tx_deadbeef", "status": "settled" }
  },
  "recorded": true,
  "signature": "sha256:9f2a1c...b7",
  "side_effects_performed": []
}
```

| Field | Notes |
|---|---|
| `status` | `ok`, `error`, or `blocked`. `blocked` means the policy layer (§6) refused the call — this is a distinct, expected outcome, not a transport error. |
| `mode` | Echoes the mode actually used to serve the request. In `real` mode with `recorder_mode=record` (see below), the adapter performs a live call *and* writes a cassette; `mode` in the response still reads `real`, and `recorded: true` signals the write happened. |
| `response` | The tool's response shape, as the stage's own code expects it. |
| `recorded` | `true` if this invocation resulted in a new cassette being written (only possible when `mode: "real"` and `recorder_mode: "record"` was explicitly set — see [`EVALS_CASSETTE.md`](./EVALS_CASSETTE.md) §5). |
| `signature` | The deterministic cassette key this request maps to (§`EVALS_CASSETTE.md` §4), always populated so tooling can inspect/replay a specific invocation later even for `mock`/`no-op` responses. |
| `side_effects_performed` | List of side-effect categories (from the adapter's declared `side_effects`, §5) that were actually exercised. Empty for every mode except `real`. Runner asserts this is `[]` for any invocation where `metadata.shadow: true`. |

### 3.3 Error / blocked example

```json
{
  "status": "blocked",
  "mode": "real",
  "error": {
    "code": "SHADOW_REAL_MODE_FORBIDDEN",
    "message": "adapter_id=bank_api requested mode=real with metadata.shadow=true; real calls are never permitted for shadow runs."
  },
  "signature": "sha256:9f2a1c...b7"
}
```

The runner MUST treat `status: "blocked"` as a scenario-level failure (not a
tool error to be judged), and MUST surface `error.code` in the run's audit
trail unmodified.

## 4. Determinism

- The cassette signature (`sha256(canonicalize(request) + json(seed))`) is
  computed identically by the adapter shim on every invocation, regardless of
  mode — this is what lets `replay` find what `real` recorded, and lets
  tooling correlate a `mock`/`no-op` response back to "what would replay have
  returned here."
- Canonicalization rules (key sorting, volatile-header stripping) are defined
  in [`EVALS_CASSETTE.md`](./EVALS_CASSETTE.md) §4 — this document only
  requires that the same function be used on both sides.
- `metadata.seed` is mandatory on every request specifically so `mock` mode
  can offer deterministic templated randomness (e.g. a mock ID generator)
  without adapters inventing their own seeding scheme.

## 5. Side-effect declaration & policy enforcement

Every adapter registers a static `side_effects` manifest, independent of any
single invocation:

```json
{
  "adapter_id": "bank_api",
  "side_effects": ["db-write", "payment", "external-notification"]
}
```

The runner's policy layer reads this manifest once at suite-load time and
uses it, together with `metadata.shadow` and the requested `mode`, to decide
whether to forward the call to the adapter at all or return `blocked`
immediately (§6.1). This means a misconfigured stage can never reach the
adapter process for a forbidden combination — the check happens in the
runner, not just inside the adapter shim, giving defense in depth.

## 6. Security guidelines for shadow runs

Shadow (dark) runs execute a candidate workflow against production-shaped
inputs specifically so its behavior can be observed *before* it is trusted —
which means the one property that must never be violated is: **a shadow run
cannot be distinguished, from the outside, from a run that produced no
external effect at all.**

### 6.1 Hard rules (MUST)

1. **`mode: "real"` MUST be rejected whenever `metadata.shadow: true`.** This
   is enforced twice: once in the runner's policy layer (§5, before the call
   ever leaves the runner process) and once in the adapter shim itself (in
   case a stage is invoked outside the runner, e.g. during local debugging).
   Both layers independently returning `blocked` is intentional redundancy,
   not duplication to clean up.
2. **A shadow run's adapter set MUST default to `no-op` for any adapter whose
   `side_effects` manifest is non-empty**, unless the suite author explicitly
   pins that adapter to `mock` or `replay` for the scenario. There is no
   implicit fallback to `real`.
3. **Shadow runs MUST NOT use production credentials, API keys, or service
   accounts.** The adapter shim's process environment for a shadow invocation
   is a credential-free sandbox (see §7); a `real`-mode call that somehow
   reached the shim would fail on missing credentials as a second
   independent barrier, not merely a policy check.
4. **Shadow runs MUST NOT be granted network egress to any host on the
   production/staging allowlist.** Sandbox network policy (§7) denies
   egress by default; `replay` and `mock` never need it, so this costs
   nothing for the common case.
5. **Every blocked or attempted-and-denied `real` call during a shadow run
   MUST be logged to the audit trail (§8) at `warn` or higher**, even though
   the call itself never executed — a shadow scenario that keeps attempting
   `real` calls is a signal the candidate version is misconfigured, and that
   signal must be visible to whoever reviews the run, not silently absorbed.
6. **PII and secrets present in shadow-run requests/responses MUST be
   scrubbed before persistence**, per [`EVALS_CASSETTE.md`](./EVALS_CASSETTE.md)
   §7 — a shadow run against production-shaped inputs is exactly the
   scenario most likely to carry real customer data through the pipeline.

### 6.2 Non-goals (explicitly out of scope for this design)

- Sandbox network policy enforcement mechanics (iptables rules, container
  network namespaces, etc.) — this document specifies the *contract*
  (deny-by-default egress, no production credentials); the enforcement
  mechanism is implementation detail for the adapter shim prototype
  (tracked separately under the epic).
- Rate limiting / cost controls for `real`-mode recording sessions — a
  follow-up concern once the adapter shim exists to record against.

## 7. Sandbox isolation

Adapters running in `mock`, `replay`, or `no-op` mode SHOULD run inside a
sandboxed process or container distinct from the runner:

- No filesystem access beyond the cassette store's designated read path and
  (for `record` sessions only) write path.
- No network egress by default; `replay`/`mock`/`no-op` never need it.
- No environment variables carrying production credentials — the sandbox's
  environment is provisioned separately from whatever environment a `real`
  invocation would run in, so there is nothing to leak even if a stage were
  somehow coerced into requesting `mode: "real"`.

`real` mode, when explicitly authorized (recording sessions, deliberate
live-fire scenarios), runs outside this sandbox with narrowly-scoped,
short-lived credentials — never the ambient credentials of the CI/runner
environment.

## 8. Audit logging

Every `/adapter/invoke` call, regardless of outcome, emits one structured
trace record containing at minimum:

```json
{
  "ts": "2026-08-08T02:14:00Z",
  "run_id": "01JZ9K3XQMPLE0000000000000",
  "adapter_id": "bank_api",
  "mode_requested": "real",
  "mode_served": "blocked",
  "shadow": true,
  "signature": "sha256:9f2a1c...b7",
  "response_status": null,
  "error_code": "SHADOW_REAL_MODE_FORBIDDEN"
}
```

Audit records are append-only and retained independently of cassette
lifecycle (§`EVALS_CASSETTE.md` §8) — a rotated-out cassette does not imply
its audit trail is deleted.

## 9. Open questions / follow-ups

- The adapter shim's concrete transport (Go binary vs. sidecar process) and
  the exact sandbox mechanism (container vs. OS-level process isolation) are
  implementation decisions for the adapter shim prototype issue, not this
  design.
- CI gating on shadow-run audit findings (e.g. failing a suite if any
  scenario attempted a blocked `real` call) is tracked under the CI gating
  child issue, not specified here.
