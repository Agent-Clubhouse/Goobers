#!/usr/bin/env python3
"""CLI for the EvalSuite adapter shim: record, replay, inspect, scrub.

Usage examples:
    python3 -m evals.adapters.cli record --adapter-id bank_api \
        --request request.json --response response.json --seed 42

    python3 -m evals.adapters.cli replay --adapter-id bank_api \
        --request request.json --seed 42

    python3 -m evals.adapters.cli inspect --adapter-id bank_api
    python3 -m evals.adapters.cli inspect --cassette cassettes/bank_api/sha256-1a2b3c4d.json

    python3 -m evals.adapters.cli scrub --cassette cassettes/bank_api/sha256-1a2b3c4d.json
    python3 -m evals.adapters.cli scrub --adapter-id bank_api --all
"""
from __future__ import annotations

import argparse
import json
import sys

from . import shim as shim_mod

DEFAULT_CASSETTES_DIR = "evals/adapters/cassettes"


def _load_json_arg(value: str | None, inline_flag_name: str) -> dict:
    """Load a JSON object from a file path, or "-" for stdin."""
    if value is None:
        raise SystemExit(f"error: {inline_flag_name} is required")
    if value == "-":
        return json.load(sys.stdin)
    with open(value, "r", encoding="utf-8") as f:
        return json.load(f)


def _print_json(obj) -> None:
    print(json.dumps(obj, indent=2, sort_keys=True))


def cmd_record(args: argparse.Namespace) -> int:
    store = shim_mod.CassetteStore(args.cassettes_dir)
    request = _load_json_arg(args.request, "--request")
    response = _load_json_arg(args.response, "--response")

    shim = shim_mod.AdapterShim(
        store=store,
        real_callers={args.adapter_id: lambda _req, _response=response: _response},
        run_id=args.run_id,
    )
    result = shim.invoke(args.adapter_id, "real", request, seed=args.seed, recorder_mode="record")
    path = store.path_for(args.adapter_id, result.signature)
    print(f"Recorded cassette: {path}")
    _print_json(result.to_dict())
    return 0


def cmd_replay(args: argparse.Namespace) -> int:
    store = shim_mod.CassetteStore(args.cassettes_dir)
    request = _load_json_arg(args.request, "--request")

    shim = shim_mod.AdapterShim(store=store, run_id=args.run_id)
    try:
        result = shim.invoke(args.adapter_id, "replay", request, seed=args.seed)
    except shim_mod.CassetteMissingError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    _print_json(result.to_dict())
    return 0 if result.status == "ok" else 2


def cmd_inspect(args: argparse.Namespace) -> int:
    store = shim_mod.CassetteStore(args.cassettes_dir)

    if args.cassette:
        with open(args.cassette, "r", encoding="utf-8") as f:
            _print_json(json.load(f))
        return 0

    if not args.adapter_id:
        print("error: --cassette or --adapter-id is required", file=sys.stderr)
        return 1

    paths = store.list_paths(args.adapter_id)
    if not paths:
        print(f"no cassettes found for adapter_id={args.adapter_id!r}", file=sys.stderr)
        return 1

    summaries = []
    for path in paths:
        with open(path, "r", encoding="utf-8") as f:
            cassette = json.load(f)
        summaries.append(
            {
                "path": path,
                "signature": cassette.get("signature"),
                "recorded_at": cassette.get("metadata", {}).get("recorded_at"),
                "tags": cassette.get("tags", []),
                "scrubbed_fields": cassette.get("scrubbed_fields", []),
            }
        )
    _print_json(summaries)
    return 0


def cmd_scrub(args: argparse.Namespace) -> int:
    store = shim_mod.CassetteStore(args.cassettes_dir)

    if args.cassette:
        paths = [args.cassette]
    elif args.adapter_id and args.all:
        paths = store.list_paths(args.adapter_id)
    elif args.all:
        paths = store.list_paths()
    else:
        print("error: pass --cassette, or --adapter-id with --all, or --all alone", file=sys.stderr)
        return 1

    if not paths:
        print("no cassettes matched", file=sys.stderr)
        return 1

    scrubbed_count = 0
    for path in paths:
        with open(path, "r", encoding="utf-8") as f:
            cassette = json.load(f)
        already_scrubbed = set(cassette.get("scrubbed_fields", []))
        scrubbed = shim_mod.scrub_cassette(cassette)
        newly_scrubbed = set(scrubbed.get("scrubbed_fields", [])) - already_scrubbed
        if not newly_scrubbed:
            print(f"skipped (nothing new to scrub): {path}")
            continue
        # EVALS_CASSETTE.md §8: cassettes are immutable once created —
        # scrub is a recovery path for a scrub-rule gap found after the
        # fact (§9), not a content update, so it writes a new rotated file
        # and leaves the original untouched rather than overwriting it.
        rotated_path = store.save_rotation(path, scrubbed)
        scrubbed_count += 1
        print(f"scrubbed: {path} -> {rotated_path}")

    print(f"done: {scrubbed_count} cassette(s) scrubbed (originals left untouched, new rotations written)")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="evals-adapter-shim", description=__doc__)
    parser.add_argument(
        "--cassettes-dir",
        default=DEFAULT_CASSETTES_DIR,
        help=f"cassette store root (default: {DEFAULT_CASSETTES_DIR})",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    record = subparsers.add_parser("record", help="perform a (simulated) real call and write a cassette")
    record.add_argument("--adapter-id", required=True)
    record.add_argument("--request", required=True, help="path to request JSON, or - for stdin")
    record.add_argument("--response", required=True, help="path to the response JSON to record")
    record.add_argument("--seed", default=0, type=json.loads, help="JSON-encoded seed value (default: 0)")
    record.add_argument("--run-id", default="cli-record")
    record.set_defaults(func=cmd_record)

    replay = subparsers.add_parser("replay", help="look up a cassette by request signature and return it")
    replay.add_argument("--adapter-id", required=True)
    replay.add_argument("--request", required=True, help="path to request JSON, or - for stdin")
    replay.add_argument("--seed", default=0, type=json.loads, help="JSON-encoded seed value (default: 0)")
    replay.add_argument("--run-id", default="cli-replay")
    replay.set_defaults(func=cmd_replay)

    inspect = subparsers.add_parser("inspect", help="print one cassette, or list cassettes for an adapter")
    inspect.add_argument("--cassette", help="path to a specific cassette file")
    inspect.add_argument("--adapter-id", help="list all cassettes for this adapter_id")
    inspect.set_defaults(func=cmd_inspect)

    scrub = subparsers.add_parser("scrub", help="mask PII/secrets in one or more cassettes, in place")
    scrub.add_argument("--cassette", help="path to a specific cassette file")
    scrub.add_argument("--adapter-id", help="scope --all to one adapter_id")
    scrub.add_argument("--all", action="store_true", help="scrub every matching cassette")
    scrub.set_defaults(func=cmd_scrub)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
