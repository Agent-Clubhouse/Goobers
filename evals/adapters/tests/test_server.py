#!/usr/bin/env python3
"""HTTP-level tests for the adapter shim's POST /adapter/invoke endpoint."""
from __future__ import annotations

import json
import shutil
import tempfile
import threading
import unittest
import urllib.error
import urllib.request

from evals.adapters import server as server_mod
from evals.adapters import shim as shim_mod


class ServerInvokeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        store = shim_mod.CassetteStore(self.tmp)
        self.shim = shim_mod.AdapterShim(
            store=store,
            real_callers={"bank_api": lambda _req: {"status": 200, "body": {"tx_id": "tx-1"}}},
            mock_scripts={"bank_api": {"status": 200, "body": {"mock": True}}},
            side_effects={"bank_api": ["db-write", "payment"]},
            run_id="server-test",
        )
        self.httpd = server_mod.serve(self.shim, host="127.0.0.1", port=0)
        self.port = self.httpd.server_address[1]
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self._shutdown)

    def _shutdown(self):
        self.httpd.shutdown()
        self.httpd.server_close()
        self.thread.join(timeout=5)

    def _post(self, payload: dict) -> tuple[int, dict]:
        body = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.port}/adapter/invoke",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                return resp.status, json.loads(resp.read())
        except urllib.error.HTTPError as exc:
            with exc:
                return exc.code, json.loads(exc.read())

    def test_mock_mode_over_http(self):
        status, payload = self._post(
            {
                "adapter_id": "bank_api",
                "mode": "mock",
                "request": {"method": "GET", "path": "/x"},
                "metadata": {"run_id": "t1", "seed": 1},
            }
        )
        self.assertEqual(status, 200)
        self.assertEqual(payload["status"], "ok")
        self.assertEqual(payload["response"], {"status": 200, "body": {"mock": True}})

    def test_no_op_mode_over_http_is_ok(self):
        # no-op is a normal successful mode, not a policy refusal — see
        # test_shadow_real_mode_over_http_is_blocked for the actual
        # "blocked" case.
        status, payload = self._post(
            {
                "adapter_id": "bank_api",
                "mode": "no-op",
                "request": {"method": "POST", "path": "/transfer"},
                "metadata": {"run_id": "t1"},
            }
        )
        self.assertEqual(status, 200)
        self.assertEqual(payload["status"], "ok")
        self.assertEqual(payload["side_effects_performed"], [])

    def test_shadow_real_mode_over_http_is_blocked(self):
        status, payload = self._post(
            {
                "adapter_id": "bank_api",
                "mode": "real",
                "request": {"method": "POST", "path": "/transfer"},
                "metadata": {"run_id": "t1", "shadow": True},
            }
        )
        self.assertEqual(status, 200)
        self.assertEqual(payload["status"], "blocked")
        self.assertEqual(payload["error"]["code"], "SHADOW_REAL_MODE_FORBIDDEN")

    def test_replay_missing_cassette_returns_404(self):
        status, payload = self._post(
            {
                "adapter_id": "bank_api",
                "mode": "replay",
                "request": {"method": "GET", "path": "/never-recorded"},
                "metadata": {"run_id": "t1"},
            }
        )
        self.assertEqual(status, 404)
        self.assertEqual(payload["status"], "error")

    def test_real_mode_without_recorder_mode_does_not_record_over_http(self):
        request = {"method": "POST", "path": "/transfer", "body": {"amount": 5}}
        status, payload = self._post(
            {"adapter_id": "bank_api", "mode": "real", "request": request, "metadata": {"run_id": "t1", "seed": 7}}
        )
        self.assertEqual(status, 200)
        self.assertFalse(payload["recorded"])

    def test_real_then_replay_roundtrip_over_http(self):
        request = {"method": "POST", "path": "/transfer", "body": {"amount": 5}}
        status, payload = self._post(
            {
                "adapter_id": "bank_api",
                "mode": "real",
                "request": request,
                "metadata": {"run_id": "t1", "seed": 7},
                "recorder_mode": "record",
            }
        )
        self.assertEqual(status, 200)
        self.assertTrue(payload["recorded"])
        self.assertEqual(payload["side_effects_performed"], ["db-write", "payment"])

        status, payload = self._post(
            {"adapter_id": "bank_api", "mode": "replay", "request": request, "metadata": {"run_id": "t1", "seed": 7}}
        )
        self.assertEqual(status, 200)
        self.assertFalse(payload["recorded"])
        self.assertEqual(payload["response"], {"status": 200, "body": {"tx_id": "tx-1"}})

    def test_missing_fields_returns_400(self):
        status, payload = self._post({"adapter_id": "bank_api"})
        self.assertEqual(status, 400)
        self.assertEqual(payload["status"], "error")

    def test_unknown_path_returns_404(self):
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.port}/not-adapter-invoke", data=b"{}", method="POST"
        )
        try:
            urllib.request.urlopen(req, timeout=5)
            self.fail("expected HTTPError")
        except urllib.error.HTTPError as exc:
            with exc:
                self.assertEqual(exc.code, 404)


if __name__ == "__main__":
    unittest.main()
