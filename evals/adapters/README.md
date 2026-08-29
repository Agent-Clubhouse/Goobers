# EvalSuite adapter shim prototype

Prototype implementation of #2666 ("Adapter Shim Prototype & Cassette
Recorder"), a child of the EvalSuite epic (#2662). A lightweight HTTP/JSON
adapter shim that lets an agentic stage run in `real`, `mock`, `replay`, or
`no-op` mode, plus a CLI to `record`, `replay`, `inspect`, and `scrub`
cassettes.

## Status: prototype / research artifact

Per #2662's scope note, EvalSuite's early work is intentionally research-first
— design docs and prototypes that inform a later implementation decision, not
production code. This directory is scoped the same way: it is not wired into
`make ci`, the Go build, or any daemon. It has no dependency on the rest of
the Go module and can be deleted without affecting anything else in the repo.

**Reconciled against #2671's proposed spec.** This implementation follows
the cassette format and `/adapter/invoke` HTTP contract as proposed in PR
#2671 (issue #2665's "finalize sandbox & tool-adapter API + cassette format
design"), which adds `evals/EVALS_CASSETTE.md` and `evals/EVALS_SANDBOX_API.md`
at the repo root. As of this writing #2671 is open, not yet merged — this
implementation was diffed field-by-field against its content and updated to
match (see "Reconciliation notes" below for what changed and why), but treat
it as one implementation's read of an in-review proposal, not confirmation
that it is final. Re-check against whatever actually lands on `main`,
especially if #2671 changes further before merging. The earlier draft this
started from (`mighty-hare`'s `EVALS_CASSETTE.md` / `EVALS_SANDBOX_API.md`,
preserved as a research artifact) is superseded by #2671 for the purposes of
this implementation.

## Layout

```
evals/adapters/
  shim.py     — core: signatures, CassetteStore, AdapterShim mode dispatch, scrub
  server.py   — stdlib http.server exposing POST /adapter/invoke
  cli.py      — record / replay / inspect / scrub subcommands
  tests/      — unittest suite (42 tests, stdlib only)
  cassettes/  — default cassette store root (empty; populated by `record`)
```

No third-party dependencies — everything here uses only the Python standard
library, matching the existing `mighty-hare/run_evals.py` prototype's own
"intentionally avoids external deps for portability" choice. Targets Python
3.9+ (tested against 3.14; `from __future__ import annotations` defers the
`X | Y` union type hints to strings, so no 3.10+ runtime feature is
actually required).

## Modes

| Mode     | Behavior                                                              | Touches cassette store | Touches a real backend |
|----------|--------------------------------------------------------------------------|:---:|:---:|
| `real`   | Calls the registered real caller for the adapter. Writes a cassette only when `recorder_mode="record"` is also given — an independent opt-in, per the spec. | write, only if `recorder_mode="record"` | yes |
| `mock`   | Returns a registered mock script's response (or a generic stub)          | no  | no  |
| `replay` | Looks up a cassette by request signature; fails fast (`CASSETTE_NOT_FOUND`) if none is found — unconditionally, no fallback | read | no |
| `no-op`  | Always returns a fixed, side-effect-free response — never calls anything | no  | no  |

`no-op` is deliberately unconditional and always returns `status: "ok"`: it
is a normal, successful mode selection (the mode a shadow/dark run uses to
guarantee no side effects occur), not a policy refusal, so it never
consults `real_callers`, `mock_scripts`, or the cassette store regardless of
configuration.

`status: "blocked"` is reserved for an actual policy rejection. The only one
this prototype implements is the spec's hard MUST rule: **`mode="real"` is
always rejected when the invocation's `shadow` flag is true**
(`SHADOW_REAL_MODE_FORBIDDEN`), checked before any real caller or cassette
write is reachable — this is the safety property the whole shadow-run
premise depends on, so it's enforced unconditionally, independent of
`recorder_mode`.

## Cassette format

`cassettes/{adapter_id}/{sha256-<16 hex>}.json`, e.g.
`cassettes/bank_api/sha256-1a2b3c4d5e6f7890.json`:

```json
{
  "signature": "sha256:...",
  "adapter_id": "bank_api",
  "request": {"method": "POST", "path": "/transfer", "body": {...}},
  "response": {"status": 200, "body": {...}},
  "metadata": {"recorded_at": "...", "run_id": "...", "recorder_version": "1.0", "seed": 42},
  "tags": [],
  "response_hash": "sha256:...",
  "scrubbed_fields": []
}
```

Deliberately **no `mode` field** — a cassette is mode-agnostic storage
(written during a `real` recording session, read by `replay`); mode is a
property of an invocation, not of the recording.

`signature = sha256(canonicalize(request) + json.dumps(seed))`, where
canonicalization sorts JSON keys and strips volatile headers (`Date`,
`Request-Id`, `X-Request-Id`, `Traceparent`, and anything matching
`X-Trace-*`) before hashing.

`scrubbed_fields` lists the dotted paths of any masked field (e.g.
`"response.body.auth_token"`), present whenever scrubbing has occurred.

## CLI usage

```sh
# Record: perform a (caller-supplied) real call and write a cassette.
python3 -m evals.adapters.cli record \
  --adapter-id bank_api \
  --request request.json \
  --response response.json \
  --seed 42

# Replay: look up the cassette by request signature; exits 1 if missing.
python3 -m evals.adapters.cli replay \
  --adapter-id bank_api \
  --request request.json \
  --seed 42

# Inspect: list every cassette for an adapter, or print one cassette in full.
python3 -m evals.adapters.cli inspect --adapter-id bank_api
python3 -m evals.adapters.cli inspect --cassette cassettes/bank_api/sha256-1a2b3c4d5e6f7890.json

# Scrub: mask PII/secret fields (by key-name heuristic). Writes a NEW
# rotated cassette (original.r1.json, original.r2.json, ...) and leaves the
# original untouched — cassettes are immutable once created, so scrubbing an
# already-written cassette is a recovery path, not an in-place edit.
python3 -m evals.adapters.cli scrub --cassette cassettes/bank_api/sha256-....json
python3 -m evals.adapters.cli scrub --adapter-id bank_api --all
python3 -m evals.adapters.cli scrub --all
```

All commands accept `--cassettes-dir` (default: `evals/adapters/cassettes`)
to point at a different store root.

`record`'s CLI form takes the response to record as an explicit file rather
than actually calling a live backend — this prototype is transport-agnostic
by design (see "HTTP shim" below); a real integration would register a
`real_callers[adapter_id]` callable that does the actual outbound call, and
the CLI's `record` command simulates that call via the supplied
`--response` file so the cassette-writing path can be exercised without
network access.

**Known gap:** `record` hardcodes `mode="real"` with no `shadow` parameter
at all — fine today, since a human deliberately running `record` is by
definition not a shadow invocation, but if a future runner integration
(#2667) ever shells out to this CLI from inside a live run rather than
calling `AdapterShim.invoke()` directly, the shadow guard wouldn't apply
here since there's nothing to pass it. Add a `--shadow` passthrough at that
point if it becomes a real path, rather than assuming CLI callers are
always human-operated.

**Known gap:** `replay`'s transparent resolution doesn't currently prefer a
cassette's latest scrub rotation over the original — `replay` always
resolves the canonical (un-rotated) path by signature. A rotated/scrubbed
cassette is available for inspection and audit, but promoting it to be what
`replay` actually serves is not implemented; flagging this explicitly rather
than silently getting it wrong.

## HTTP shim

```sh
python3 -m evals.adapters.server --port 8791 --cassettes-dir evals/adapters/cassettes
```

Implements the wire contract proposed in #2671's `EVALS_SANDBOX_API.md` §3:

```
POST /adapter/invoke
{"adapter_id": "bank_api", "mode": "replay",
 "request": {"method": "POST", "path": "/transfer", "body": {...}},
 "metadata": {"run_id": "...", "scenario_id": "...", "seed": 42, "shadow": false},
 "recorder_mode": "record"}

-> {"status": "ok|error|blocked", "mode": "replay", "response": {...},
    "recorded": false, "signature": "sha256:...", "side_effects_performed": []}
```

`status: "blocked"` (e.g. the shadow+real rejection) is a normal HTTP 200
carrying a structured `{"error": {"code", "message"}}` — an expected policy
outcome, not a transport error. Malformed requests (bad JSON, missing
required fields, an unknown adapter/mode) are 4xx.

Built on `http.server` rather than a framework, single-threaded-per-request
(`ThreadingHTTPServer`), no auth/TLS — a local sandbox tool for a runner to
talk to over a trusted loop, not a service to expose beyond that.

## Running the tests

```sh
python3 -m unittest discover -s evals/adapters/tests -v
```

42 tests, no external dependencies, no network access required.

## Reconciliation notes (against PR #2671)

This prototype was originally built against `mighty-hare`'s draft spec, then
diffed field-by-field against #2671's proposed (open, unmerged) content and
updated. What actually changed, for anyone comparing the two:

- **`no-op` no longer returns `status: "blocked"`.** It's a normal
  successful mode selection; `blocked` is reserved for an actual policy
  refusal.
- **Added shadow-run enforcement.** `mode="real"` + `shadow: true` now
  raises `ShadowRealModeForbiddenError` (`SHADOW_REAL_MODE_FORBIDDEN`) before
  any real caller or cassette write is reachable — previously unimplemented
  entirely.
- **`recorder_mode="record"` is now a separate, explicit opt-in from
  `mode="real"`.** A `real` call without it performs the live call but does
  NOT write a cassette (previously every `real` call recorded
  unconditionally). The earlier `replay` → `allow_record` fallback is
  removed entirely; `replay` is now unconditionally fail-fast, matching the
  spec's actual model (record is a `real`-mode behavior, not a replay
  fallback).
- **Cassettes no longer carry a `mode` field**, per the spec's explicit
  "mode is a property of an invocation, not the recording."
  `metadata.scenario_id` is now recorded when supplied.
- **Response envelope gained `side_effects_performed`** — the adapter's
  registered `side_effects` manifest, echoed back on every successful
  `real`-mode call, `[]` for every other mode.
- **Errors are now structured** (`{"code": ..., "message": ...}`) instead of
  a bare string, with stable codes (`CASSETTE_NOT_FOUND`,
  `SHADOW_REAL_MODE_FORBIDDEN`, `NO_REAL_CALLER`, `UNKNOWN_MODE`).
- **`scrub` now writes a new rotated cassette** (`*.rN.json`) instead of
  editing in place, per the spec's cassette-immutability rule; the CLI's
  `scrubbed_fields` list replaced the earlier `metadata.scrubbed` boolean.
- **Volatile-header list updated**: added `Traceparent` and an `X-Trace-*`
  prefix match (previously a fixed exact-match list missing both).

## Known gaps / next steps (not in this prototype's scope)

- No actual `real_caller` implementations are bundled — this shim is
  transport-agnostic; wiring up a specific adapter's live HTTP client is
  left to whoever integrates this into a runner (#2668/#2667).
- The scrub heuristic is a prototype-grade key-name substring match, not a
  compliance-grade PII detector.
- No CI gating, baseline management, or artifact-store integration (S3
  ACLs, retention/rotation policy) — explicitly out of scope per #2662's
  "Non-goals" section and left to #2669.
- The runner-side policy layer (§5/§6.1 of the sandbox API: side-effect
  manifests driving mode defaults, the *first* of the spec's required
  double-enforcement for shadow-run safety) is out of scope here — this
  prototype only implements the adapter shim's own half of that
  enforcement. Audit logging (§8) is also not implemented.
- `replay` doesn't prefer a scrub-rotated cassette over the original (see
  "Known gap" under CLI usage above).
