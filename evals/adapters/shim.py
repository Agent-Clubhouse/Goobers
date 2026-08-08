#!/usr/bin/env python3
"""Adapter shim core: signatures, cassette storage, and mode dispatch.

Implements the sandbox/tool-adapter contract from EVALS_SANDBOX_API.md and
the cassette format from EVALS_CASSETTE.md (both under the mighty-hare
research worktree at the time this was written — see this prototype's
README for reconciliation notes once #2665's cassette-format spec PR lands).

Kept dependency-free (standard library only) to match the existing
mighty-hare prototype's own "avoids external deps for portability" choice,
and because this is a research/prototype artifact, not yet wired into the
Go module's build or CI.
"""
from __future__ import annotations

import copy
import datetime
import hashlib
import json
import os
import re
from dataclasses import dataclass, field
from typing import Any

RECORDER_VERSION = "0.1"

# Header names considered volatile per EVALS_CASSETTE.md's normalization
# rule ("remove volatile headers (Date, Request-Id)") — compared
# case-insensitively since HTTP header names are case-insensitive.
VOLATILE_HEADERS = {"date", "request-id", "x-request-id", "x-trace-id"}

# Field-name substrings scrubbed by default (case-insensitive) per
# EVALS_CASSETTE.md's "Security & PII" section. This is a prototype
# heuristic, not a compliance-grade PII scrubber.
DEFAULT_SCRUB_PATTERNS = (
    "password",
    "token",
    "secret",
    "api_key",
    "apikey",
    "auth",
    "ssn",
    "social_security",
    "credit_card",
    "card_number",
    "cvv",
    "account_number",
    "email",
    "phone",
)

SCRUB_MASK = "***SCRUBBED***"


class AdapterError(Exception):
    """Raised for adapter-shim-level failures (missing cassette, bad mode)."""


class CassetteMissingError(AdapterError):
    """Raised in replay mode when no cassette matches the request signature."""


def _canonicalize(value: Any) -> Any:
    """Recursively sort dict keys so JSON serialization is deterministic.

    json.dumps(..., sort_keys=True) already sorts top-level and nested dict
    keys during serialization, so this exists mainly to strip volatile
    headers and other non-deterministic fields *before* serialization, per
    EVALS_CASSETTE.md's normalization rule.
    """
    if isinstance(value, dict):
        return {k: _canonicalize(v) for k, v in value.items()}
    if isinstance(value, list):
        return [_canonicalize(v) for v in value]
    return value


def normalize_request(request: dict) -> dict:
    """Canonicalize a request for signing: sort keys, drop volatile headers."""
    normalized = _canonicalize(copy.deepcopy(request))
    headers = normalized.get("headers")
    if isinstance(headers, dict):
        normalized["headers"] = {
            k: v for k, v in headers.items() if k.lower() not in VOLATILE_HEADERS
        }
    return normalized


def compute_signature(request: dict, seed: Any = 0) -> str:
    """signature = sha256(normalize(request) + json.dumps(seed)), per spec."""
    normalized = normalize_request(request)
    payload = json.dumps(normalized, sort_keys=True) + json.dumps(seed, sort_keys=True)
    digest = hashlib.sha256(payload.encode("utf-8")).hexdigest()
    return f"sha256:{digest}"


def short_signature(signature: str) -> str:
    """The short hex prefix used for cassette filenames (sha256-<16 hex>)."""
    digest = signature.split(":", 1)[-1]
    return f"sha256-{digest[:16]}"


def response_hash(response: dict) -> str:
    payload = json.dumps(_canonicalize(response), sort_keys=True)
    return "sha256:" + hashlib.sha256(payload.encode("utf-8")).hexdigest()


def scrub_value(value: Any, patterns: tuple[str, ...] = DEFAULT_SCRUB_PATTERNS) -> Any:
    """Recursively mask dict values whose key matches a scrub pattern."""
    if isinstance(value, dict):
        scrubbed = {}
        for k, v in value.items():
            if any(p in k.lower() for p in patterns):
                scrubbed[k] = SCRUB_MASK
            else:
                scrubbed[k] = scrub_value(v, patterns)
        return scrubbed
    if isinstance(value, list):
        return [scrub_value(v, patterns) for v in value]
    return value


def scrub_cassette(cassette: dict, patterns: tuple[str, ...] = DEFAULT_SCRUB_PATTERNS) -> dict:
    """Return a copy of a cassette with request/response fields scrubbed.

    Recomputes response_hash after scrubbing so the stored hash still
    matches the stored (now-scrubbed) response, and stamps metadata so a
    scrubbed cassette is distinguishable from a raw recording.
    """
    scrubbed = copy.deepcopy(cassette)
    if "request" in scrubbed:
        scrubbed["request"] = scrub_value(scrubbed["request"], patterns)
    if "response" in scrubbed:
        scrubbed["response"] = scrub_value(scrubbed["response"], patterns)
        scrubbed["response_hash"] = response_hash(scrubbed["response"])
    metadata = scrubbed.setdefault("metadata", {})
    metadata["scrubbed"] = True
    metadata["scrubbed_at"] = _now_iso()
    return scrubbed


def _now_iso() -> str:
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


@dataclass
class CassetteStore:
    """Filesystem cassette store: cassettes/{adapter_id}/{short_sig}.json."""

    root: str

    def _dir_for(self, adapter_id: str) -> str:
        safe_id = re.sub(r"[^A-Za-z0-9_.-]", "_", adapter_id)
        return os.path.join(self.root, safe_id)

    def _path_for(self, adapter_id: str, signature: str) -> str:
        return os.path.join(self._dir_for(adapter_id), f"{short_signature(signature)}.json")

    def path_for(self, adapter_id: str, signature: str) -> str:
        return self._path_for(adapter_id, signature)

    def load(self, adapter_id: str, signature: str) -> dict | None:
        path = self._path_for(adapter_id, signature)
        if not os.path.exists(path):
            return None
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)

    def save(self, cassette: dict) -> str:
        adapter_id = cassette["adapter_id"]
        signature = cassette["signature"]
        directory = self._dir_for(adapter_id)
        os.makedirs(directory, exist_ok=True)
        path = self._path_for(adapter_id, signature)
        with open(path, "w", encoding="utf-8") as f:
            json.dump(cassette, f, indent=2, sort_keys=True)
            f.write("\n")
        return path

    def list_paths(self, adapter_id: str | None = None) -> list[str]:
        base = self._dir_for(adapter_id) if adapter_id else self.root
        if not os.path.isdir(base):
            return []
        paths = []
        for dirpath, _dirnames, filenames in os.walk(base):
            for name in filenames:
                if name.endswith(".json"):
                    paths.append(os.path.join(dirpath, name))
        return sorted(paths)


@dataclass
class InvokeResult:
    status: str  # "ok" | "error" | "blocked"
    mode: str
    response: dict | None
    recorded: bool
    signature: str

    def to_dict(self) -> dict:
        return {
            "status": self.status,
            "mode": self.mode,
            "response": self.response,
            "recorded": self.recorded,
            "signature": self.signature,
        }


# Side effects a no-op response is guaranteed to avoid, mirroring
# EVALS_SANDBOX_API.md's "adapters must expose side_effects" contract.
NO_OP_RESPONSE = {"status": 200, "body": {"status": "no-op", "note": "shadow run: side effects suppressed"}}


@dataclass
class AdapterShim:
    """Implements the /adapter/invoke contract for one or more adapter_ids.

    real_callers maps adapter_id -> a callable(request) -> response dict,
    used only in mode="real" (and by record, which is real + a cassette
    write). Prototype scope: no actual outbound HTTP client is bundled —
    callers inject their own real_callers for whatever adapter they're
    fronting, keeping this shim transport-agnostic.
    """

    store: CassetteStore
    real_callers: dict[str, Any] = field(default_factory=dict)
    mock_scripts: dict[str, Any] = field(default_factory=dict)
    run_id: str = "unset"

    def invoke(
        self,
        adapter_id: str,
        mode: str,
        request: dict,
        seed: Any = 0,
        allow_record: bool = False,
    ) -> InvokeResult:
        if mode not in ("real", "mock", "replay", "no-op"):
            raise AdapterError(f"unknown adapter mode: {mode!r}")

        signature = compute_signature(request, seed)

        if mode == "no-op":
            # Shadow-run safety: never touches a real caller or cassette.
            return InvokeResult("blocked", mode, NO_OP_RESPONSE, recorded=False, signature=signature)

        if mode == "mock":
            response = self._mock_response(adapter_id, request)
            return InvokeResult("ok", mode, response, recorded=False, signature=signature)

        if mode == "replay":
            cassette = self.store.load(adapter_id, signature)
            if cassette is not None:
                return InvokeResult("ok", mode, cassette["response"], recorded=False, signature=signature)
            if not allow_record:
                # Fail-fast default per EVALS_CASSETTE.md ("Default: fail
                # fast (preferred for eval determinism)").
                raise CassetteMissingError(
                    f"no cassette for adapter={adapter_id!r} signature={signature!r}; "
                    "pass allow_record=True (recorder_mode=record) to fall back to a real call"
                )
            # Falls through to a real call + cassette write, mirroring the
            # spec's opt-in "recorder_mode=record" escape hatch.

        # mode == "real", or "replay" falling through with allow_record=True.
        response = self._real_response(adapter_id, request)
        cassette = self._build_cassette(adapter_id, signature, request, response, seed)
        self.store.save(cassette)
        return InvokeResult("ok", mode, response, recorded=True, signature=signature)

    def _real_response(self, adapter_id: str, request: dict) -> dict:
        caller = self.real_callers.get(adapter_id)
        if caller is None:
            raise AdapterError(f"no real_caller registered for adapter_id={adapter_id!r}")
        return caller(request)

    def _mock_response(self, adapter_id: str, request: dict) -> dict:
        script = self.mock_scripts.get(adapter_id)
        if script is None:
            return {"status": 200, "body": {"mocked": True, "adapter_id": adapter_id}}
        if callable(script):
            return script(request)
        return script

    def _build_cassette(
        self, adapter_id: str, signature: str, request: dict, response: dict, seed: Any
    ) -> dict:
        return {
            "signature": signature,
            "adapter_id": adapter_id,
            "mode": "replay",
            "request": request,
            "response": response,
            "metadata": {
                "recorded_at": _now_iso(),
                "run_id": self.run_id,
                "recorder_version": RECORDER_VERSION,
                "seed": seed,
            },
            "tags": [],
            "response_hash": response_hash(response),
        }
