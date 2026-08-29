#!/usr/bin/env python3
"""Unit tests for evals.adapters.shim.

Run with: python3 -m unittest discover -s evals/adapters/tests -v
"""
from __future__ import annotations

import json
import os
import shutil
import tempfile
import unittest

from evals.adapters import shim as shim_mod


class SignatureTests(unittest.TestCase):
    def test_signature_is_deterministic(self):
        request = {"method": "POST", "path": "/transfer", "body": {"amount": 10}}
        sig1 = shim_mod.compute_signature(request, seed=42)
        sig2 = shim_mod.compute_signature(request, seed=42)
        self.assertEqual(sig1, sig2)

    def test_signature_ignores_key_order(self):
        a = {"method": "POST", "body": {"amount": 10, "to": "x"}}
        b = {"body": {"to": "x", "amount": 10}, "method": "POST"}
        self.assertEqual(shim_mod.compute_signature(a), shim_mod.compute_signature(b))

    def test_signature_ignores_volatile_headers(self):
        a = {"method": "GET", "headers": {"Date": "2026-01-01", "Accept": "json"}}
        b = {"method": "GET", "headers": {"Date": "2099-12-31", "Accept": "json"}}
        self.assertEqual(shim_mod.compute_signature(a), shim_mod.compute_signature(b))

    def test_signature_changes_with_seed(self):
        request = {"method": "GET", "path": "/x"}
        self.assertNotEqual(
            shim_mod.compute_signature(request, seed=1),
            shim_mod.compute_signature(request, seed=2),
        )

    def test_signature_changes_with_meaningful_field(self):
        a = {"method": "GET", "path": "/x"}
        b = {"method": "GET", "path": "/y"}
        self.assertNotEqual(shim_mod.compute_signature(a), shim_mod.compute_signature(b))

    def test_short_signature_prefix_length(self):
        sig = shim_mod.compute_signature({"a": 1})
        short = shim_mod.short_signature(sig)
        self.assertTrue(short.startswith("sha256-"))
        self.assertEqual(len(short) - len("sha256-"), 16)


class CassetteStoreTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.store = shim_mod.CassetteStore(self.tmp)

    def test_save_then_load_roundtrip(self):
        request = {"method": "GET", "path": "/x"}
        signature = shim_mod.compute_signature(request)
        cassette = {
            "signature": signature,
            "adapter_id": "bank_api",
            "request": request,
            "response": {"status": 200, "body": {"ok": True}},
            "metadata": {"recorded_at": "now", "run_id": "r1", "recorder_version": "0.1"},
            "tags": [],
            "response_hash": shim_mod.response_hash({"status": 200, "body": {"ok": True}}),
        }
        path = self.store.save(cassette)
        self.assertTrue(os.path.exists(path))

        loaded = self.store.load("bank_api", signature)
        self.assertEqual(loaded["response"], cassette["response"])

    def test_load_missing_returns_none(self):
        self.assertIsNone(self.store.load("bank_api", "sha256:doesnotexist"))

    def test_adapter_id_is_path_sanitized(self):
        cassette = {
            "signature": "sha256:abc",
            "adapter_id": "../../etc",
            "request": {},
            "response": {},
            "metadata": {},
            "tags": [],
            "response_hash": "sha256:x",
        }
        path = self.store.save(cassette)
        real_root = os.path.realpath(self.tmp)
        real_path = os.path.realpath(path)
        # No slash survives sanitization, so the adapter_id becomes a single
        # (harmless) path *component* rather than an actual traversal —
        # assert the resolved cassette path still lives under the store root.
        self.assertEqual(os.path.commonpath([real_root, real_path]), real_root)
        # And that it's exactly one directory below root (no extra nesting
        # from any slash that slipped through unsanitized).
        relative = os.path.relpath(real_path, real_root)
        self.assertEqual(len(relative.split(os.sep)), 2)

    def test_list_paths_scoped_to_adapter(self):
        for adapter_id in ("a", "b"):
            request = {"adapter": adapter_id}
            signature = shim_mod.compute_signature(request)
            self.store.save(
                {
                    "signature": signature,
                    "adapter_id": adapter_id,
                            "request": request,
                    "response": {},
                    "metadata": {},
                    "tags": [],
                    "response_hash": "sha256:x",
                }
            )
        self.assertEqual(len(self.store.list_paths("a")), 1)
        self.assertEqual(len(self.store.list_paths()), 2)


class AdapterShimInvokeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.store = shim_mod.CassetteStore(self.tmp)

    def test_real_mode_without_recorder_mode_does_not_record(self):
        calls = []

        def caller(request):
            calls.append(request)
            return {"status": 200, "body": {"tx_id": "tx-1"}}

        shim = shim_mod.AdapterShim(store=self.store, real_callers={"bank_api": caller}, run_id="r1")
        request = {"method": "POST", "path": "/transfer", "body": {"amount": 10}}
        result = shim.invoke("bank_api", "real", request, seed=1)

        self.assertEqual(result.status, "ok")
        self.assertFalse(result.recorded)
        self.assertEqual(len(calls), 1)
        self.assertIsNone(self.store.load("bank_api", result.signature))

    def test_real_mode_with_recorder_mode_record_writes_cassette(self):
        calls = []

        def caller(request):
            calls.append(request)
            return {"status": 200, "body": {"tx_id": "tx-1"}}

        shim = shim_mod.AdapterShim(store=self.store, real_callers={"bank_api": caller}, run_id="r1")
        request = {"method": "POST", "path": "/transfer", "body": {"amount": 10}}
        result = shim.invoke("bank_api", "real", request, seed=1, recorder_mode="record")

        self.assertEqual(result.status, "ok")
        self.assertTrue(result.recorded)
        self.assertEqual(len(calls), 1)
        cassette = self.store.load("bank_api", result.signature)
        self.assertIsNotNone(cassette)
        self.assertNotIn("mode", cassette)

    def test_replay_mode_returns_recorded_response_without_calling_real(self):
        calls = []

        def caller(request):
            calls.append(request)
            return {"status": 200, "body": {"tx_id": "tx-1"}}

        shim = shim_mod.AdapterShim(store=self.store, real_callers={"bank_api": caller}, run_id="r1")
        request = {"method": "POST", "path": "/transfer", "body": {"amount": 10}}
        shim.invoke("bank_api", "real", request, seed=1, recorder_mode="record")
        self.assertEqual(len(calls), 1)

        replay_result = shim.invoke("bank_api", "replay", request, seed=1)
        self.assertEqual(replay_result.status, "ok")
        self.assertFalse(replay_result.recorded)
        self.assertEqual(replay_result.response, {"status": 200, "body": {"tx_id": "tx-1"}})
        # Replay must not have invoked the real caller again.
        self.assertEqual(len(calls), 1)

    def test_replay_mode_fails_fast_when_cassette_missing(self):
        shim = shim_mod.AdapterShim(store=self.store, run_id="r1")
        request = {"method": "GET", "path": "/x"}
        with self.assertRaises(shim_mod.CassetteMissingError):
            shim.invoke("bank_api", "replay", request)

    def test_real_mode_rejected_for_shadow_run(self):
        def caller(_request):
            return {"status": 200, "body": {}}

        shim = shim_mod.AdapterShim(store=self.store, real_callers={"bank_api": caller}, run_id="r1")
        with self.assertRaises(shim_mod.ShadowRealModeForbiddenError) as ctx:
            shim.invoke("bank_api", "real", {"method": "GET"}, shadow=True)
        self.assertEqual(ctx.exception.code, "SHADOW_REAL_MODE_FORBIDDEN")

    def test_real_mode_rejected_for_shadow_run_even_with_recorder_mode(self):
        # A shadow invocation must fail closed regardless of any other flag.
        def caller(_request):
            return {"status": 200, "body": {}}

        shim = shim_mod.AdapterShim(store=self.store, real_callers={"bank_api": caller}, run_id="r1")
        with self.assertRaises(shim_mod.ShadowRealModeForbiddenError):
            shim.invoke("bank_api", "real", {"method": "GET"}, shadow=True, recorder_mode="record")
        self.assertEqual(self.store.list_paths(), [])

    def test_mock_mode_uses_registered_script(self):
        shim = shim_mod.AdapterShim(
            store=self.store,
            mock_scripts={"bank_api": {"status": 200, "body": {"mock": True}}},
            run_id="r1",
        )
        result = shim.invoke("bank_api", "mock", {"method": "GET"})
        self.assertEqual(result.status, "ok")
        self.assertEqual(result.response, {"status": 200, "body": {"mock": True}})
        self.assertFalse(result.recorded)
        # Mock mode never touches the cassette store.
        self.assertEqual(self.store.list_paths(), [])

    def test_mock_mode_default_when_no_script_registered(self):
        shim = shim_mod.AdapterShim(store=self.store, run_id="r1")
        result = shim.invoke("bank_api", "mock", {"method": "GET"})
        self.assertEqual(result.status, "ok")
        self.assertEqual(result.response["body"]["adapter_id"], "bank_api")

    def test_no_op_mode_is_fully_inert(self):
        calls = []

        def caller(request):
            calls.append(request)
            return {"status": 200, "body": {}}

        shim = shim_mod.AdapterShim(store=self.store, real_callers={"bank_api": caller}, run_id="r1")
        result = shim.invoke("bank_api", "no-op", {"method": "POST", "path": "/transfer"})
        # no-op is a normal, successful mode selection — "blocked" is
        # reserved for the policy layer refusing a call (e.g. shadow+real),
        # which no-op never triggers.
        self.assertEqual(result.status, "ok")
        self.assertEqual(result.side_effects_performed, [])
        self.assertFalse(result.recorded)
        self.assertEqual(len(calls), 0)
        self.assertEqual(self.store.list_paths(), [])

    def test_no_op_mode_is_inert_even_when_shadow(self):
        shim = shim_mod.AdapterShim(store=self.store, run_id="r1")
        result = shim.invoke("bank_api", "no-op", {"method": "POST"}, shadow=True)
        self.assertEqual(result.status, "ok")

    def test_real_mode_without_registered_caller_raises(self):
        shim = shim_mod.AdapterShim(store=self.store, run_id="r1")
        with self.assertRaises(shim_mod.AdapterError):
            shim.invoke("bank_api", "real", {"method": "GET"})

    def test_unknown_mode_raises(self):
        shim = shim_mod.AdapterShim(store=self.store, run_id="r1")
        with self.assertRaises(shim_mod.AdapterError):
            shim.invoke("bank_api", "bogus-mode", {"method": "GET"})


class ScrubTests(unittest.TestCase):
    def test_scrub_masks_matching_keys(self):
        cassette = {
            "signature": "sha256:x",
            "adapter_id": "bank_api",
            "request": {"body": {"auth_token": "abc123", "amount": 10}},
            "response": {"body": {"email": "a@example.com", "tx_id": "tx-1"}},
            "metadata": {},
            "response_hash": "sha256:stale",
        }
        scrubbed = shim_mod.scrub_cassette(cassette)

        self.assertEqual(scrubbed["request"]["body"]["auth_token"], shim_mod.SCRUB_MASK)
        self.assertEqual(scrubbed["request"]["body"]["amount"], 10)
        self.assertEqual(scrubbed["response"]["body"]["email"], shim_mod.SCRUB_MASK)
        self.assertEqual(scrubbed["response"]["body"]["tx_id"], "tx-1")
        self.assertCountEqual(
            scrubbed["scrubbed_fields"], ["request.body.auth_token", "response.body.email"]
        )

    def test_scrub_recomputes_response_hash(self):
        cassette = {
            "signature": "sha256:x",
            "adapter_id": "bank_api",
            "request": {},
            "response": {"body": {"token": "secret"}},
            "metadata": {},
            "response_hash": "sha256:stale",
        }
        scrubbed = shim_mod.scrub_cassette(cassette)
        self.assertEqual(scrubbed["response_hash"], shim_mod.response_hash(scrubbed["response"]))
        self.assertNotEqual(scrubbed["response_hash"], "sha256:stale")

    def test_scrub_does_not_mutate_original(self):
        cassette = {
            "signature": "sha256:x",
            "adapter_id": "bank_api",
            "request": {"body": {"password": "hunter2"}},
            "response": {},
            "metadata": {},
            "response_hash": "sha256:x",
        }
        shim_mod.scrub_cassette(cassette)
        self.assertEqual(cassette["request"]["body"]["password"], "hunter2")


class CassetteJSONSchemaTests(unittest.TestCase):
    """Assert cassettes written by the shim match EVALS_CASSETTE.md's recommended keys."""

    def test_recorded_cassette_has_documented_keys(self):
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, ignore_errors=True)
        store = shim_mod.CassetteStore(tmp)
        shim = shim_mod.AdapterShim(
            store=store,
            real_callers={"bank_api": lambda _r: {"status": 200, "body": {"tx_id": "tx-1"}}},
            run_id="r1",
        )
        result = shim.invoke(
            "bank_api", "real", {"method": "POST", "path": "/transfer"}, seed=42, recorder_mode="record"
        )
        with open(store.path_for("bank_api", result.signature), "r", encoding="utf-8") as f:
            cassette = json.load(f)

        for key in (
            "signature",
            "adapter_id",
            "request",
            "response",
            "metadata",
            "tags",
            "response_hash",
        ):
            self.assertIn(key, cassette)
        for key in ("recorded_at", "run_id", "recorder_version"):
            self.assertIn(key, cassette["metadata"])
        # EVALS_CASSETTE.md §3: a cassette is mode-agnostic storage — no
        # "mode" key. Recorded during a "real" session, read by "replay".
        self.assertNotIn("mode", cassette)


if __name__ == "__main__":
    unittest.main()
