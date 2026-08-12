#!/usr/bin/env bash
# Cross-compile the Windows binary the Windows image COPYs, WITH build metadata.
# SPIKE ARTIFACT.
#
# Exists because getting this wrong is silent and was. The Linux image builds
# from source inside the Dockerfile and stamps
#   -X github.com/goobers/goobers/internal/version.Version/.Commit
# from build args. The Windows image COPYs a prebuilt goobers.exe, so the
# stamping has to happen out here — and the obvious `-X main.version=...` sets
# variables that do not exist, which the linker accepts without complaint.
#
# The result was a fleet whose Linux half reported its build commit and whose
# Windows half reported `goobers dev (commit none)` — visible only in a
# cross-platform run's output, and directly compounding #2855: the build commit
# is the one dimension the cluster cannot supply for you.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
VERSION="${VERSION:-spike}"
COMMIT="${COMMIT:-$(git -C "$REPO" rev-parse --short HEAD)}"
DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
OUT="$REPO/deploy/dev/image/goobers.exe"

cd "$REPO"
GOOS=windows GOARCH=amd64 go build \
  -ldflags "-s -w \
    -X github.com/goobers/goobers/internal/version.Version=${VERSION} \
    -X github.com/goobers/goobers/internal/version.Commit=${COMMIT} \
    -X github.com/goobers/goobers/internal/version.Date=${DATE}" \
  -o "$OUT" ./cmd/goobers
echo "built $OUT  version=$VERSION commit=$COMMIT"
