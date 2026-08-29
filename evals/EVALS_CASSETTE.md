# Design: Cassette Format — EvalSuite

> Status: **Finalized for implementation** (2026-08-08)
> Epic: [#2662](https://github.com/Agent-Clubhouse/Goobers/issues/2662) — EvalSuite: End-to-end workflow evaluation
> Issue: [#2665](https://github.com/Agent-Clubhouse/Goobers/issues/2665) — Sandbox & Tool-Adapter API + Cassette Format
> Companion spec: [`EVALS_SANDBOX_API.md`](./EVALS_SANDBOX_API.md)
> Builds on prototype research by mighty-hare (`EVALS_CASSETTE.md`, `EVALS_SCOPE.md`, preserved as research artifacts, not implementation)

## 1. Purpose

A cassette is a single recorded tool-adapter interaction, stored so that
`replay` mode ([`EVALS_SANDBOX_API.md`](./EVALS_SANDBOX_API.md) §2) can return
it verbatim without ever reaching the real tool. Cassettes are what make an
EvalSuite run deterministic and safe to run against production-shaped inputs
(shadow runs) — the whole point of §6 in the sandbox API spec depends on
cassettes existing and being trustworthy.

## 2. File layout

```
cassettes/{adapter_id}/{signature}.json
```

Example:

```
cassettes/bank_api/sha256-1a2b3c4d5e6f7890.json
```

- One file per signature. A signature collision across two genuinely
  different requests is a canonicalization bug (§4), not an expected event —
  it should never happen, and if it does, `replay` returning the wrong
  response is a correctness incident, not a tolerable edge case.
- `adapter_id` as a directory keeps cassette stores partitionable per adapter
  (independent rotation policy, independent access control for
  production-derived recordings — see §7).
- Filenames use a truncated signature (16 hex chars is enough entropy to
  avoid collision within one adapter's cassette set) for filesystem
  friendliness; the full signature is always stored inside the file (§3).

## 3. Cassette schema

```json
{
  "signature": "sha256:1a2b3c4d5e6f7890...",
  "adapter_id": "bank_api",
  "request": {
    "method": "POST",
    "path": "/v1/transfer",
    "headers": { "content-type": "application/json" },
    "body": { "from": "acct_1", "to": "acct_2", "amount_cents": 4200 }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "application/json" },
    "body": { "tx_id": "tx_deadbeef", "status": "settled" }
  },
  "response_hash": "sha256:7c9e2a...",
  "metadata": {
    "recorded_at": "2026-08-08T02:10:00Z",
    "run_id": "01JZ9K3XQMPLE0000000000000",
    "scenario_id": "wire-transfer-happy-path",
    "seed": 42,
    "recorder_version": "1.0"
  },
  "tags": ["payment", "happy-path"],
  "scrubbed_fields": ["request.body.from", "request.body.to"]
}
```

| Field | Required | Notes |
|---|---|---|
| `signature` | yes | Full signature (§4), duplicated from the filename for self-description — a cassette file must be identifiable on its own if moved or inspected out of context. |
| `adapter_id` | yes | Must match the request's `adapter_id`; the recorder MUST refuse to write a cassette where these disagree. |
| `request` | yes | The *canonicalized* request that produced this signature (§4) — not necessarily byte-identical to what the caller sent, since volatile fields are stripped before hashing and storage. |
| `response` | yes | Exactly what `replay` mode returns. |
| `response_hash` | yes | `sha256` of the canonicalized response body, independent of `signature`. Used for integrity checks and for diffing two cassettes recorded for logically-the-same interaction at different times (e.g. detecting a silent upstream API change during re-recording). |
| `metadata.recorded_at` | yes | ISO-8601 UTC. |
| `metadata.run_id` / `.scenario_id` | yes | Provenance — which run/scenario first produced this cassette. Re-recording under a new `run_id` for the same signature creates a *new* cassette per §8, it does not overwrite this field. |
| `metadata.seed` | yes | The seed that was part of the signature input — stored explicitly so a human (or the recorder) can verify determinism without recomputing the hash. |
| `metadata.recorder_version` | yes | Version of the recorder that wrote this file; see §8 for why this matters at rotation time. |
| `tags` | no | Free-form, for cassette-store browsing/filtering tooling (§9). |
| `scrubbed_fields` | no | JSONPath-ish list of fields that were redacted before write (§7). Presence of this key is itself a signal to reviewers that the cassette may contain masked, not real, values in those positions. |

Note what is deliberately absent: there is no `mode` field on the cassette
itself. A cassette is mode-agnostic storage — it was written during a `real`
recording session, but it is read by `replay`, and the same file could in
principle back a `mock` adapter's static response too. Mode is a property of
an *invocation* ([`EVALS_SANDBOX_API.md`](./EVALS_SANDBOX_API.md) §3), not of
the recording.

## 4. Signature computation

```
signature = "sha256:" + hex(sha256(canonicalize(request) + json.dumps(seed, sort_keys=True)))
```

**Canonicalization rules** (applied identically by the adapter shim and by
any offline tooling that needs to recompute a signature):

1. JSON-serialize `request` with sorted object keys at every nesting level.
2. Strip known-volatile headers before hashing: `Date`, `Request-Id`,
   `X-Request-Id`, `Traceparent`, and any header matching `X-Trace-*`. This
   list is the canonicalizer's responsibility to maintain — adapters MUST
   NOT invent their own exclusions, or two adapters could compute different
   signatures for what the suite author considers "the same request."
3. `method` and `path` are compared case-sensitively and exactly; no
   normalization of trailing slashes or query-parameter ordering is
   performed — if two requests differ only in query-param order, they are
   different requests and get different cassettes. (Suite authors who want
   those treated as equivalent should normalize at the stage-definition
   level, not rely on the recorder to guess intent.)
4. `seed` is appended as canonical JSON, not folded into `request` — this
   keeps "what varies the response" (the request) visibly separate from
   "what makes an otherwise-identical request replayable with different
   pseudo-randomness" (the seed) in the signature computation, and matches
   the request shape actually sent over the wire in
   [`EVALS_SANDBOX_API.md`](./EVALS_SANDBOX_API.md) §3.1.

## 5. Recorder behavior

Decision order, evaluated by the adapter shim on every `/adapter/invoke`
call:

1. `mode == "no-op"` → return the adapter's static inert response. No
   cassette lookup, no signature computation is skipped (still computed and
   returned, per [`EVALS_SANDBOX_API.md`](./EVALS_SANDBOX_API.md) §3.2, for
   traceability) but never used to read or write a file.
2. `mode == "mock"` → return the suite-provided mock/template response. No
   cassette read.
3. `mode == "replay"`:
   - Cassette exists at `cassettes/{adapter_id}/{signature}.json` → return
     its `response` verbatim.
   - Cassette missing → **fail fast** (`status: "error"`,
     `error.code: "CASSETTE_NOT_FOUND"`). This is the default and preferred
     behavior for eval determinism: a missing cassette means the suite
     author changed something about the request shape without
     re-recording, and silently falling back to anything else (a live call,
     a synthesized response) would hide that.
4. `mode == "real"`:
   - Performs the live call.
   - If `recorder_mode: "record"` was explicitly set on the invoking runner
     session (never implied by `mode: "real"` alone), writes a new cassette
     for this signature after scrubbing (§7).
   - `recorder_mode: "record"` MUST be an explicit, separate opt-in from
     `mode: "real"` — the two are independently required specifically so
     that "run for real" and "run for real *and persist what happened*"
     can't be conflated by a caller that only meant one of them.

`replay`'s fail-fast default (step 3) is the load-bearing determinism
guarantee of the whole cassette system — every other design choice in this
document exists to make that failure mode informative rather than mysterious
(clear `error.code`, signature included in the error, cassette path
implied by `adapter_id` + signature so a human can immediately locate
"the cassette that should exist and doesn't").

## 6. Determinism requirements

- **Seeds are always recorded, never inferred.** `metadata.seed` on the
  cassette must equal the `seed` used to compute its signature; a mismatch
  is a corrupt cassette.
- **Non-deterministic response fields must be normalized at recording
  time**, not at replay time. If the real tool's response contains a
  wall-clock timestamp, a freshly-generated UUID, or similar, the recorder
  MUST either (a) replace it with a deterministic token
  (`"<recorded-timestamp>"`, `"<generated-id>"`) documented in the
  adapter's own contract, or (b) leave it as-is and document that this
  specific field is expected to vary and MUST be excluded from judge/diff
  comparisons downstream. Silently recording a real timestamp and expecting
  every consumer to know to ignore it is not acceptable — the cassette file
  itself must make the non-determinism visible.
- **A cassette, once created, is immutable.** See §8 — "updating" a cassette
  always means creating a new one.

## 7. Security & PII

Cassettes recorded against real endpoints (`mode: "real"` with
`recorder_mode: "record"`) are the most likely artifact in the whole
EvalSuite pipeline to carry real customer data, because they are, by
definition, a snapshot of a real interaction.

- **Scrub before write, not after.** The recorder applies each adapter's
  declared scrub rules (a list of field paths — account numbers, auth
  tokens, payment identifiers, free-text PII) before the cassette ever
  touches disk. There is no "scrub later" pass; an unscrubbed cassette must
  never exist, even transiently, outside the recorder's own memory.
- **`scrubbed_fields` is populated whenever scrubbing occurred**, so a
  reviewer inspecting a cassette can immediately tell which values are
  redacted placeholders rather than real (if synthetic-looking) data.
- **Access control follows provenance.** Cassettes recorded from
  synthetic/staging inputs may live in the repo's cassette store alongside
  suite definitions. Cassettes recorded from real production-adjacent
  interactions MUST be stored in access-controlled storage (e.g. an
  artifact bucket with restricted ACLs), never committed to the repo,
  regardless of scrubbing — scrubbing reduces risk, it does not eliminate
  the need for access control on what remains.
- **This is the concrete mechanism behind
  [`EVALS_SANDBOX_API.md`](./EVALS_SANDBOX_API.md) §6.1 rule 6** — shadow
  runs are exactly the case most likely to produce cassette-adjacent data
  from production-shaped inputs, so the scrub-before-write rule here is
  what makes that guarantee real rather than aspirational.

## 8. Versioning & rotation

- `metadata.recorder_version` is stamped on every cassette at write time.
- **Cassettes are immutable once created.** "Updating" a recorded
  interaction always means recording a new cassette (new signature, because
  the request or seed changed) or, when the *same* signature must map to a
  genuinely different response (e.g. the real tool's contract changed),
  creating a **new file with an explicit rotation tag** in `metadata` (e.g.
  `"superseded_signature": "sha256:<old>"`) rather than overwriting the old
  file in place. This keeps replay results for any historical run
  reproducible even after rotation — a suite run last week against the old
  cassette must still be explainable by inspecting exactly what it read.
- Old cassettes are archived (moved out of the active `cassettes/` tree, not
  deleted) when rotated, with a migration note in the new cassette's
  metadata explaining why the rotation happened.
- A schema change to the cassette format itself (adding/removing a required
  field) bumps `recorder_version` and is documented in this file's own
  changelog going forward — the recorder should refuse to write cassettes
  under a version it doesn't recognize rather than guess a shape.

## 9. Tooling

A CLI ships alongside the adapter shim (implementation tracked in the
adapter shim prototype issue, contract specified here):

| Command | Behavior |
|---|---|
| `evals-cassette record <adapter_id> <request.json>` | Performs the real call (subject to all §7 scrubbing) and writes a cassette. Equivalent to `mode: "real"` + `recorder_mode: "record"` from outside the runner, for one-off recording sessions. |
| `evals-cassette replay <adapter_id> <signature>` | Prints the stored response for inspection, without invoking anything. |
| `evals-cassette inspect <adapter_id> <signature>` | Prints the full cassette (request, response, metadata, scrubbed-field list) for human review — the primary tool for auditing what a shadow run actually recorded. |
| `evals-cassette scrub <adapter_id> <signature> --fields <path,...>` | Re-applies scrubbing to an already-written cassette (recovery path for a scrub-rule gap discovered after the fact) and writes the result as a new, rotated cassette per §8 — never edits in place. |

## 10. CI policy

- Cassette **writes are disabled in CI by default.** `recorder_mode: "record"`
  requires an explicit flag on the CI job (`--allow-cassette-record`), which
  is not set on any suite-gating pipeline — CI runs are `replay`/`mock`/
  `no-op` only, so a missing cassette fails the run (§5 step 3) instead of
  silently recording a new one and masking a suite regression as "still
  green."
- Cassette **reads** are always allowed in CI — that's the entire point of
  `replay` mode.
