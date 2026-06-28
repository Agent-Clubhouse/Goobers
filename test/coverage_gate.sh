#!/usr/bin/env bash
# coverage_gate.sh — QA merge-gate coverage check (M5).
#
# Enforces a minimum total line-coverage threshold across the Go module.
# Used by QA during review (interim gate) and intended to back a `make cover-check`
# target on the shared CI workflow once wired with Dev-3.
#
# Usage:
#   test/coverage_gate.sh [THRESHOLD]
#   THRESHOLD defaults to $COVERAGE_THRESHOLD or 70 (percent).
#
# Behavior:
#   - Reuses ./coverage.out if present (produced by `make test`/`make cover`);
#     otherwise runs `go test ./... -covermode=atomic -coverprofile=coverage.out`.
#   - Prints total + per-package coverage, then exits non-zero if total < threshold.
set -euo pipefail

THRESHOLD="${1:-${COVERAGE_THRESHOLD:-70}}"
PROFILE="${COVERAGE_PROFILE:-coverage.out}"

if ! command -v go >/dev/null 2>&1; then
  echo "coverage_gate: go toolchain not found on PATH" >&2
  exit 2
fi

if [[ ! -f "$PROFILE" ]]; then
  echo "coverage_gate: $PROFILE not found — running tests to generate it..."
  go test ./... -covermode=atomic -coverprofile="$PROFILE"
fi

echo "=== coverage by function ==="
go tool cover -func="$PROFILE"
# Total line: `go tool cover -func` prints a final "total:\t(statements)\tNN.N%" row.
TOTAL_LINE="$(go tool cover -func="$PROFILE" | grep -E '^total:' | tail -1)"
TOTAL_PCT="$(echo "$TOTAL_LINE" | grep -oE '[0-9]+(\.[0-9]+)?%' | tr -d '%')"

if [[ -z "${TOTAL_PCT:-}" ]]; then
  echo "coverage_gate: could not parse total coverage from: $TOTAL_LINE" >&2
  exit 2
fi

echo ""
echo "total coverage: ${TOTAL_PCT}%  (threshold: ${THRESHOLD}%)"

# Float-safe comparison via awk.
if awk -v t="$TOTAL_PCT" -v th="$THRESHOLD" 'BEGIN { exit !(t + 0 < th + 0) }'; then
  echo "FAIL: coverage ${TOTAL_PCT}% is below threshold ${THRESHOLD}%" >&2
  exit 1
fi
echo "PASS: coverage gate satisfied"
