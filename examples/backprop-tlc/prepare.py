"""Operator-only, offline manifest preparation; never run by candidate agents."""

import argparse
import hashlib
import json
import os
from pathlib import Path

JAR_DIGEST = "sha256:e20447644bfabd29d6bd0fc2c0eb1473fb301c1a0e4dfe1961938ce5a191edbf"


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--java-home", type=Path, required=True)
    parser.add_argument("--jar", type=Path, required=True)
    args = parser.parse_args()
    runtime = args.java_home.resolve(strict=True)
    jar = args.jar.resolve(strict=True)
    if digest(jar.read_bytes()) != JAR_DIGEST:
        parser.error("TLC jar does not match the reviewed v1.8.0 pin")
    files = []
    for path in sorted(runtime.rglob("*")):
        if path.is_symlink():
            parser.error("runtime symlinks are unsupported; use a closed runtime tree")
        if path.is_file():
            files.append({"path": path.relative_to(runtime).as_posix(), "digest": digest(path.read_bytes())})
    files.sort(key=lambda item: item["path"])
    runtime_digest = digest(json.dumps(files, ensure_ascii=False, separators=(",", ":")).encode())
    source = Path(__file__).resolve().parent
    spec = (source / "Spec.tla").read_bytes()
    config = (source / "Spec.cfg").read_bytes()
    manifest = {
        "property": "finite-state-not-two",
        "invariant": "Safe",
        "specification": "Spec.tla",
        "specDigest": digest(spec),
        "configuration": "Spec.cfg",
        "configDigest": digest(config),
        "jar": str(jar),
        "runtime": str(runtime),
        "runtimeDigest": runtime_digest,
        "java": "bin/java.exe" if os.name == "nt" else "bin/java",
        "assumptions": [],
    }
    args.config.mkdir(parents=True, exist_ok=True)
    (args.config / "Spec.tla").write_bytes(spec)
    (args.config / "Spec.cfg").write_bytes(config)
    (args.config / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    print(runtime_digest)


if __name__ == "__main__":
    main()
