#!/usr/bin/env python3
"""Adapter shim core: signatures, cassette storage, and mode dispatch.

Implements the sandbox/tool-adapter contract and cassette format from
evals/EVALS_SANDBOX_API.md and evals/EVALS_CASSETTE.md as proposed in PR
#2671 (issue #2665) — open, not yet merged, at the time this was reconciled.
Treat this as one implementation's read of that proposal, not confirmation
it is final; re-check against whatever actually lands on main.

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

RECORDER_VERSION = "1.0"

# Header names considered volatile per EVALS_CASSETTE.md §4's canonicalization
# rule ("Date, Request-Id, X-Request-Id, Traceparent, and any header matching
# X-Trace-*") — compared case-insensitively since HTTP header names are
# case-insensitive. X-Trace-* is a prefix match, applied separately below.
VOLATILE_HEADERS = {"date", "request-id", "x-request-id", "traceparent"}
VOLATILE_HEADER_PREFIXES = ("x-trace-",)


def _is_volatile_header(name: str) -> bool:
    lowered = name.lower()
    return lowered in VOLATILE_HEADERS or any(lowered.startswith(p) for p in VOLATILE_HEADER_PREFIXES)

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
    """Raised for adapter-shim-level failures (missing cassette, bad mode).

    code carries the EVALS_SANDBOX_API.md-style stable error code (e.g.
    "CASSETTE_NOT_FOUND", "SHADOW_REAL_MODE_FORBIDDEN") so callers (the CLI,
    the HTTP handler) can surface {"code": ..., "message": ...} per the
    spec's §3.3 error shape without re-deriving a code from the message text.
    """

    def __init__(self, message: str, code: str = "ADAPTER_ERROR"):
        super().__init__(message)
        self.code = code


class CassetteMissingError(AdapterError):
    """Raised in replay mode when no cassette matches the request signature."""

    def __init__(self, message: str):
        super().__init__(message, code="CASSETTE_NOT_FOUND")


class ShadowRealModeForbiddenError(AdapterError):
    """Raised when mode="real" is requested with metadata.shadow=true.

    EVALS_SANDBOX_API.md §6.1 rule 1 (a hard MUST): a shadow run can never
    reach a real, side-effecting call. This is the adapter shim's half of
    the required double enforcement (the runner's policy layer is the
    other, out of this prototype's scope).
    """

    def __init__(self, adapter_id: str, mode: str):
        super().__init__(
            f"adapter_id={adapter_id!r} requested mode={mode!r} with metadata.shadow=true; "
            "real calls are never permitted for shadow runs.",
            code="SHADOW_REAL_MODE_FORBIDDEN",
        )


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
        normalized["headers"] = {k: v for k, v in headers.items() if not _is_volatile_header(k)}
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


def scrub_value(
    value: Any, patterns: tuple[str, ...] = DEFAULT_SCRUB_PATTERNS, _path: str = ""
) -> tuple[Any, list[str]]:
    """Recursively mask dict values whose key matches a scrub pattern.

    Returns (scrubbed_value, paths) where paths is the dotted-path list of
    every field that was masked, e.g. ["body.auth_token"] — the raw material
    for EVALS_CASSETTE.md §3's cassette-level `scrubbed_fields` list.
    """
    if isinstance(value, dict):
        scrubbed = {}
        paths: list[str] = []
        for k, v in value.items():
            field_path = f"{_path}.{k}" if _path else k
            if any(p in k.lower() for p in patterns):
                scrubbed[k] = SCRUB_MASK
                paths.append(field_path)
            else:
                scrubbed[k], nested_paths = scrub_value(v, patterns, field_path)
                paths.extend(nested_paths)
        return scrubbed, paths
    if isinstance(value, list):
        scrubbed_items = []
        paths = []
        for i, v in enumerate(value):
            item, nested_paths = scrub_value(v, patterns, f"{_path}[{i}]")
            scrubbed_items.append(item)
            paths.extend(nested_paths)
        return scrubbed_items, paths
    return value, []


def scrub_cassette(cassette: dict, patterns: tuple[str, ...] = DEFAULT_SCRUB_PATTERNS) -> dict:
    """Return a copy of a cassette with request/response fields scrubbed.

    Recomputes response_hash after scrubbing so the stored hash still
    matches the stored (now-scrubbed) response, and populates the
    cassette-level `scrubbed_fields` list per EVALS_CASSETTE.md §3 so a
    reviewer can tell which values are redacted placeholders. Does not write
    anything or decide where the result should live — CassetteStore's
    caller is responsible for persisting it as a new, rotated cassette
    (§8: cassettes are immutable once created).
    """
    scrubbed = copy.deepcopy(cassette)
    scrubbed_fields: list[str] = list(scrubbed.get("scrubbed_fields", []))
    if "request" in scrubbed:
        scrubbed["request"], request_paths = scrub_value(scrubbed["request"], patterns, "request")
        scrubbed_fields.extend(request_paths)
    if "response" in scrubbed:
        scrubbed["response"], response_paths = scrub_value(scrubbed["response"], patterns, "response")
        scrubbed_fields.extend(response_paths)
        scrubbed["response_hash"] = response_hash(scrubbed["response"])
    scrubbed["scrubbed_fields"] = scrubbed_fields
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

    def save_rotation(self, original_path: str, cassette: dict) -> str:
        """Write cassette to a new *.rN.json path beside original_path.

        EVALS_CASSETTE.md §8: cassettes are immutable once created; an update
        (e.g. re-scrubbing) always creates a new file with a rotation tag
        rather than overwriting the original in place. Picks the lowest N
        not already in use so repeated rotations of the same cassette don't
        collide or silently overwrite an earlier rotation.
        """
        base, ext = os.path.splitext(original_path)
        # Strip a prior .rN suffix from base so rotating a rotation still
        # increments cleanly (base.r1 -> base.r2, not base.r1.r1).
        stripped = re.sub(r"\.r\d+$", "", base)
        n = 1
        while True:
            candidate = f"{stripped}.r{n}{ext}"
            if not os.path.exists(candidate):
                break
            n += 1
        with open(candidate, "w", encoding="utf-8") as f:
            json.dump(cassette, f, indent=2, sort_keys=True)
            f.write("\n")
        return candidate


@dataclass
class InvokeResult:
    status: str  # "ok" | "error" | "blocked", per EVALS_SANDBOX_API.md §3.2
    mode: str
    response: dict | None
    recorded: bool
    signature: str
    # side_effects_performed: side-effect categories actually exercised —
    # only ever non-empty for a mode="real" call against an adapter with a
    # registered side_effects manifest (§5); every other mode always
    # reports []. Not Optional/omitted: an empty list IS the meaningful
    # "no side effects happened" answer, distinct from "unknown".
    side_effects_performed: list[str] = field(default_factory=list)
    error: dict | None = None  # {"code": ..., "message": ...} per §3.3, only set for error/blocked

    def to_dict(self) -> dict:
        payload = {
            "status": self.status,
            "mode": self.mode,
            "response": self.response,
            "recorded": self.recorded,
            "signature": self.signature,
            "side_effects_performed": self.side_effects_performed,
        }
        if self.error is not None:
            payload["error"] = self.error
        return payload


# no-op's fixed, adapter-declared inert response. A real deployment would let
# each adapter declare its own; this prototype uses one shared shape since it
# doesn't model per-adapter no-op responses.
NO_OP_RESPONSE = {"status": 200, "body": {"status": "no-op", "note": "shadow run: side effects suppressed"}}


@dataclass
class AdapterShim:
    """Implements the /adapter/invoke contract for one or more adapter_ids.

    real_callers maps adapter_id -> a callable(request) -> response dict,
    used only in mode="real" (and by record, which is real + a cassette
    write). Prototype scope: no actual outbound HTTP client is bundled —
    callers inject their own real_callers for whatever adapter they're
    fronting, keeping this shim transport-agnostic.

    side_effects maps adapter_id -> its static side_effects manifest
    (EVALS_SANDBOX_API.md §5), echoed back as side_effects_performed on a
    successful mode="real" call.
    """

    store: CassetteStore
    real_callers: dict[str, Any] = field(default_factory=dict)
    mock_scripts: dict[str, Any] = field(default_factory=dict)
    side_effects: dict[str, list[str]] = field(default_factory=dict)
    run_id: str = "unset"

    def invoke(
        self,
        adapter_id: str,
        mode: str,
        request: dict,
        seed: Any = 0,
        shadow: bool = False,
        scenario_id: str | None = None,
        recorder_mode: str | None = None,
    ) -> InvokeResult:
        """recorder_mode="record" is an independent opt-in from mode="real"
        (EVALS_SANDBOX_API.md §3.2/§5): a "real" call without it performs the
        live call but does NOT write a cassette. It is meaningless — and
        ignored — for every other mode.
        """
        if mode not in ("real", "mock", "replay", "no-op"):
            raise AdapterError(f"unknown adapter mode: {mode!r}", code="UNKNOWN_MODE")

        # EVALS_SANDBOX_API.md §6.1 rule 1 (hard MUST, enforced here as the
        # adapter shim's half of the required double enforcement): a shadow
        # invocation can never reach a real call, checked before anything
        # else so no real_caller/cassette-write path is reachable for it.
        if mode == "real" and shadow:
            raise ShadowRealModeForbiddenError(adapter_id, mode)

        signature = compute_signature(request, seed)

        if mode == "no-op":
            # Deliberately unconditional: never touches a real caller, a
            # mock script, or the cassette store, regardless of shadow/
            # configuration — this is what makes it safe by construction.
            return InvokeResult("ok", mode, NO_OP_RESPONSE, recorded=False, signature=signature)

        if mode == "mock":
            response = self._mock_response(adapter_id, request)
            return InvokeResult("ok", mode, response, recorded=False, signature=signature)

        if mode == "replay":
            cassette = self.store.load(adapter_id, signature)
            if cassette is None:
                # Fail-fast, unconditionally — per EVALS_CASSETTE.md §5 step
                # 3 ("this is the default and preferred behavior for eval
                # determinism"). The spec models "record" as an explicit
                # mode="real" + recorder_mode="record" call, not as a replay
                # fallback, so there is no escape hatch here.
                raise CassetteMissingError(
                    f"no cassette for adapter={adapter_id!r} signature={signature!r}"
                )
            return InvokeResult("ok", mode, cassette["response"], recorded=False, signature=signature)

        # mode == "real"
        response = self._real_response(adapter_id, request)
        recorded = False
        if recorder_mode == "record":
            cassette = self._build_cassette(adapter_id, signature, request, response, seed, scenario_id)
            self.store.save(cassette)
            recorded = True
        performed = list(self.side_effects.get(adapter_id, []))
        return InvokeResult(
            "ok", mode, response, recorded=recorded, signature=signature, side_effects_performed=performed
        )

    def _real_response(self, adapter_id: str, request: dict) -> dict:
        caller = self.real_callers.get(adapter_id)
        if caller is None:
            raise AdapterError(
                f"no real_caller registered for adapter_id={adapter_id!r}", code="NO_REAL_CALLER"
            )
        return caller(request)

    def _mock_response(self, adapter_id: str, request: dict) -> dict:
        script = self.mock_scripts.get(adapter_id)
        if script is None:
            return {"status": 200, "body": {"mocked": True, "adapter_id": adapter_id}}
        if callable(script):
            return script(request)
        return script

    def _build_cassette(
        self,
        adapter_id: str,
        signature: str,
        request: dict,
        response: dict,
        seed: Any,
        scenario_id: str | None,
    ) -> dict:
        # No "mode" key: EVALS_CASSETTE.md §3 is explicit that a cassette is
        # mode-agnostic storage — it's written during a "real" recording
        # session but read by "replay", and mode is a property of an
        # invocation, not of the recording.
        metadata = {
            "recorded_at": _now_iso(),
            "run_id": self.run_id,
            "recorder_version": RECORDER_VERSION,
            "seed": seed,
        }
        if scenario_id is not None:
            metadata["scenario_id"] = scenario_id
        return {
            "signature": signature,
            "adapter_id": adapter_id,
            "request": request,
            "response": response,
            "metadata": metadata,
            "tags": [],
            "response_hash": response_hash(response),
        }
