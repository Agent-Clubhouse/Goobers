#!/usr/bin/env python3
"""Minimal HTTP JSON adapter shim: POST /adapter/invoke.

Implements the wire contract from EVALS_SANDBOX_API.md:

    POST /adapter/invoke
    {"adapter_id": "bank_api", "mode": "replay",
     "request": {...}, "metadata": {"run_id": "...", "seed": 42}}

    -> {"status": "ok|error|blocked", "mode": "replay", "response": {...},
        "recorded": true, "signature": "sha256..."}

Built on Python's standard-library http.server rather than a framework
(Flask, etc.) to keep this prototype dependency-free, consistent with the
mighty-hare research prototype it extends. Not hardened for production use
(single-threaded, no auth, no TLS) — a sandbox/eval-harness tool, not a
service to expose beyond a trusted local loop.
"""
from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from . import shim as shim_mod

DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 8791


def make_handler(shim: shim_mod.AdapterShim):
    class InvokeHandler(BaseHTTPRequestHandler):
        server_version = "EvalSuiteAdapterShim/" + shim_mod.RECORDER_VERSION

        def log_message(self, format: str, *args) -> None:  # noqa: A002 - stdlib signature
            # Quiet by default; tests/CLI usage don't need per-request access logs.
            pass

        def _send_json(self, status: int, payload: dict) -> None:
            body = json.dumps(payload).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self) -> None:  # noqa: N802 - stdlib method name
            if self.path != "/adapter/invoke":
                self._send_json(404, {"status": "error", "error": f"unknown path {self.path!r}"})
                return

            length = int(self.headers.get("Content-Length", "0"))
            raw = self.rfile.read(length) if length else b"{}"
            try:
                envelope = json.loads(raw or b"{}")
            except json.JSONDecodeError as exc:
                self._send_json(400, {"status": "error", "error": f"invalid JSON: {exc}"})
                return

            adapter_id = envelope.get("adapter_id")
            mode = envelope.get("mode")
            request = envelope.get("request")
            metadata = envelope.get("metadata") or {}
            seed = metadata.get("seed", 0)

            if not adapter_id or not mode or request is None:
                self._send_json(
                    400,
                    {"status": "error", "error": "adapter_id, mode, and request are required"},
                )
                return

            try:
                result = shim.invoke(adapter_id, mode, request, seed=seed)
            except shim_mod.CassetteMissingError as exc:
                self._send_json(404, {"status": "error", "mode": mode, "error": str(exc)})
                return
            except shim_mod.AdapterError as exc:
                self._send_json(400, {"status": "error", "mode": mode, "error": str(exc)})
                return

            self._send_json(200, result.to_dict())

    return InvokeHandler


def serve(shim: shim_mod.AdapterShim, host: str = DEFAULT_HOST, port: int = DEFAULT_PORT) -> ThreadingHTTPServer:
    """Build and return a bound-but-not-yet-serving server (caller controls the loop/lifecycle)."""
    handler = make_handler(shim)
    return ThreadingHTTPServer((host, port), handler)


def main(argv: list[str] | None = None) -> int:
    import argparse

    parser = argparse.ArgumentParser(description="Run the adapter shim HTTP server")
    parser.add_argument("--host", default=DEFAULT_HOST)
    parser.add_argument("--port", type=int, default=DEFAULT_PORT)
    parser.add_argument("--cassettes-dir", default="evals/adapters/cassettes")
    parser.add_argument("--run-id", default="server")
    args = parser.parse_args(argv)

    store = shim_mod.CassetteStore(args.cassettes_dir)
    adapter_shim = shim_mod.AdapterShim(store=store, run_id=args.run_id)
    httpd = serve(adapter_shim, args.host, args.port)
    print(f"adapter shim listening on http://{args.host}:{args.port}/adapter/invoke")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        httpd.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
