#!/usr/bin/env python3
"""End-to-end tests for the evals.adapters.cli record/replay/inspect/scrub commands."""
from __future__ import annotations

import contextlib
import io
import json
import os
import shutil
import tempfile
import unittest

from evals.adapters import cli


def run_cli(args: list[str]) -> tuple[int, str]:
    out = io.StringIO()
    with contextlib.redirect_stdout(out):
        code = cli.main(args)
    return code, out.getvalue()


class CLIRoundtripTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.cassettes_dir = os.path.join(self.tmp, "cassettes")

        self.request_path = os.path.join(self.tmp, "request.json")
        self.response_path = os.path.join(self.tmp, "response.json")
        with open(self.request_path, "w") as f:
            json.dump({"method": "POST", "path": "/transfer", "body": {"amount": 10}}, f)
        with open(self.response_path, "w") as f:
            json.dump({"status": 200, "body": {"tx_id": "tx-1"}}, f)

    def test_record_then_replay(self):
        code, out = run_cli(
            [
                "--cassettes-dir",
                self.cassettes_dir,
                "record",
                "--adapter-id",
                "bank_api",
                "--request",
                self.request_path,
                "--response",
                self.response_path,
                "--seed",
                "42",
            ]
        )
        self.assertEqual(code, 0)
        self.assertIn("Recorded cassette:", out)

        code, out = run_cli(
            [
                "--cassettes-dir",
                self.cassettes_dir,
                "replay",
                "--adapter-id",
                "bank_api",
                "--request",
                self.request_path,
                "--seed",
                "42",
            ]
        )
        self.assertEqual(code, 0)
        payload = json.loads(out)
        self.assertEqual(payload["response"], {"status": 200, "body": {"tx_id": "tx-1"}})
        self.assertFalse(payload["recorded"])

    def test_replay_without_recording_fails_fast(self):
        code, out = run_cli(
            [
                "--cassettes-dir",
                self.cassettes_dir,
                "replay",
                "--adapter-id",
                "bank_api",
                "--request",
                self.request_path,
            ]
        )
        self.assertEqual(code, 1)

    def test_inspect_lists_cassettes_for_adapter(self):
        run_cli(
            [
                "--cassettes-dir",
                self.cassettes_dir,
                "record",
                "--adapter-id",
                "bank_api",
                "--request",
                self.request_path,
                "--response",
                self.response_path,
            ]
        )
        code, out = run_cli(["--cassettes-dir", self.cassettes_dir, "inspect", "--adapter-id", "bank_api"])
        self.assertEqual(code, 0)
        summaries = json.loads(out)
        self.assertEqual(len(summaries), 1)
        self.assertEqual(summaries[0]["scrubbed_fields"], [])

    def test_inspect_single_cassette_by_path(self):
        run_cli(
            [
                "--cassettes-dir",
                self.cassettes_dir,
                "record",
                "--adapter-id",
                "bank_api",
                "--request",
                self.request_path,
                "--response",
                self.response_path,
            ]
        )
        cassette_path = next(
            os.path.join(dp, f)
            for dp, _dn, fs in os.walk(self.cassettes_dir)
            for f in fs
        )
        code, out = run_cli(["--cassettes-dir", self.cassettes_dir, "inspect", "--cassette", cassette_path])
        self.assertEqual(code, 0)
        cassette = json.loads(out)
        self.assertEqual(cassette["adapter_id"], "bank_api")

    def test_scrub_masks_and_persists(self):
        with open(self.response_path, "w") as f:
            json.dump({"status": 200, "body": {"auth_token": "sekrit", "tx_id": "tx-1"}}, f)
        run_cli(
            [
                "--cassettes-dir",
                self.cassettes_dir,
                "record",
                "--adapter-id",
                "bank_api",
                "--request",
                self.request_path,
                "--response",
                self.response_path,
            ]
        )
        cassette_path = next(
            os.path.join(dp, f)
            for dp, _dn, fs in os.walk(self.cassettes_dir)
            for f in fs
        )
        code, out = run_cli(["--cassettes-dir", self.cassettes_dir, "scrub", "--cassette", cassette_path])
        self.assertEqual(code, 0)
        self.assertIn("scrubbed:", out)

        # The original cassette is immutable (EVALS_CASSETTE.md §8) — scrub
        # writes a new rotated file rather than editing it in place.
        with open(cassette_path) as f:
            original = json.load(f)
        self.assertEqual(original["response"]["body"]["auth_token"], "sekrit")

        rotated_path = cassette_path.replace(".json", ".r1.json")
        self.assertTrue(os.path.exists(rotated_path))
        with open(rotated_path) as f:
            rotated = json.load(f)
        self.assertEqual(rotated["response"]["body"]["auth_token"], "***SCRUBBED***")
        self.assertEqual(rotated["response"]["body"]["tx_id"], "tx-1")
        self.assertIn("response.body.auth_token", rotated["scrubbed_fields"])

    def test_scrub_all_for_adapter(self):
        with open(self.response_path, "w") as f:
            json.dump({"status": 200, "body": {"auth_token": "sekrit", "tx_id": "tx-1"}}, f)
        run_cli(
            [
                "--cassettes-dir",
                self.cassettes_dir,
                "record",
                "--adapter-id",
                "bank_api",
                "--request",
                self.request_path,
                "--response",
                self.response_path,
            ]
        )
        code, out = run_cli(
            ["--cassettes-dir", self.cassettes_dir, "scrub", "--adapter-id", "bank_api", "--all"]
        )
        self.assertEqual(code, 0)
        self.assertIn("done: 1 cassette(s) scrubbed", out)

    def test_scrub_skips_cassette_with_nothing_to_scrub(self):
        run_cli(
            [
                "--cassettes-dir",
                self.cassettes_dir,
                "record",
                "--adapter-id",
                "bank_api",
                "--request",
                self.request_path,
                "--response",
                self.response_path,
            ]
        )
        code, out = run_cli(
            ["--cassettes-dir", self.cassettes_dir, "scrub", "--adapter-id", "bank_api", "--all"]
        )
        self.assertEqual(code, 0)
        self.assertIn("skipped (nothing new to scrub):", out)
        self.assertIn("done: 0 cassette(s) scrubbed", out)

    def test_scrub_requires_a_target(self):
        code, _out = run_cli(["--cassettes-dir", self.cassettes_dir, "scrub"])
        self.assertEqual(code, 1)


if __name__ == "__main__":
    unittest.main()
