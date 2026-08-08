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

**Working against a draft spec.** At the time this was written, #2665 (the
dedicated cassette-format spec issue) had not yet landed a PR. This
implementation follows the cassette format and `/adapter/invoke` HTTP
contract as drafted in `EVALS_CASSETTE.md` and `EVALS_SANDBOX_API.md` under
the `mighty-hare` research worktree (preserved per #2662's notes). If #2665
lands with a different schema, reconcile against that PR rather than this
one — this prototype should be treated as one data point, not the source of
truth.

## Layout

```
evals/adapters/
  shim.py     — core: signatures, CassetteStore, AdapterShim mode dispatch, scrub
  server.py   — stdlib http.server exposing POST /adapter/invoke
  cli.py      — record / replay / inspect / scrub subcommands
  tests/      — unittest suite (36 tests, stdlib only)
  cassettes/  — default cassette store root (empty; populated by `record`)
```

No third-party dependencies — everything here uses only the Python standard
library, matching the existing `mighty-hare/run_evals.py` prototype's own
"intentionally avoids external deps for portability" choice. Targets Python
3.9+ (tested against 3.14; `from __future__ import annotations` defers the
`X | Y` union type hints to strings, so no 3.10+ runtime feature is
actually required).

## Modes

| Mode     | Behavior                                                                 | Touches cassette store | Touches a real backend |
|----------|---------------------------------------------------------------------------|:---:|:---:|
| `real`   | Calls the registered real caller for the adapter, records a cassette      | write | yes |
| `mock`   | Returns a registered mock script's response (or a generic stub)           | no    | no  |
| `replay` | Looks up a cassette by request signature; fails fast if none is found     | read  | no (unless `allow_record=True` falls through to `real`) |
| `no-op`  | Always returns a fixed, side-effect-free response — never calls anything  | no    | no  |

`no-op` is deliberately unconditional: it is the mode a shadow/dark run uses
to guarantee no side effects occur, so it never consults `real_callers`,
`mock_scripts`, or the cassette store, regardless of configuration.

## Cassette format

`cassettes/{adapter_id}/{sha256-<16 hex>}.json`, e.g.
`cassettes/bank_api/sha256-1a2b3c4d5e6f7890.json`:

```json
{
  "signature": "sha256:...",
  "adapter_id": "bank_api",
  "mode": "replay",
  "request": {"method": "POST", "path": "/transfer", "body": {...}},
  "response": {"status": 200, "body": {...}},
  "metadata": {"recorded_at": "...", "run_id": "...", "recorder_version": "0.1", "seed": 42},
  "tags": [],
  "response_hash": "sha256:..."
}
```

`signature = sha256(normalize(request) + json.dumps(seed))`, where
normalization canonicalizes JSON key order and strips volatile headers
(`Date`, `Request-Id`, `X-Request-Id`, `X-Trace-Id`) before hashing, per
`EVALS_CASSETTE.md`.

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

# Scrub: mask PII/secret fields in place (by key-name heuristic) and
# recompute response_hash. Scoped to one cassette, one adapter's cassettes,
# or the whole store.
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

## HTTP shim

```sh
python3 -m evals.adapters.server --port 8791 --cassettes-dir evals/adapters/cassettes
```

Implements the wire contract from `EVALS_SANDBOX_API.md`:

```
POST /adapter/invoke
{"adapter_id": "bank_api", "mode": "replay",
 "request": {"method": "POST", "path": "/transfer", "body": {...}},
 "metadata": {"run_id": "...", "seed": 42}}

-> {"status": "ok|error|blocked", "mode": "replay", "response": {...},
    "recorded": false, "signature": "sha256:..."}
```

Built on `http.server` rather than a framework, single-threaded-per-request
(`ThreadingHTTPServer`), no auth/TLS — a local sandbox tool for a runner to
talk to over a trusted loop, not a service to expose beyond that.

## Running the tests

```sh
python3 -m unittest discover -s evals/adapters/tests -v
```

36 tests, no external dependencies, no network access required.

## Known gaps / next steps (not in this prototype's scope)

- No actual `real_caller` implementations are bundled — this shim is
  transport-agnostic; wiring up a specific adapter's live HTTP client is
  left to whoever integrates this into a runner (#2668/#2667).
- The scrub heuristic is a prototype-grade key-name substring match, not a
  compliance-grade PII detector.
- No CI gating, baseline management, or artifact-store integration (S3
  ACLs, retention/rotation policy) — explicitly out of scope per #2662's
  "Non-goals" section and left to #2669.
- Cassette immutability (per `EVALS_CASSETTE.md`: "create new cassette with
  new signature or tag" rather than editing) is not enforced by `scrub`,
  which intentionally rewrites in place — scrubbing is a security operation
  on already-recorded PII, not a content update, so this is a deliberate
  exception rather than an oversight, but worth flagging for #2665's spec
  to make explicit either way.
