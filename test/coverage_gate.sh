#!/usr/bin/env bash
# coverage_gate.sh — QA merge-gate coverage check (M5).
#
# Enforces a minimum line-coverage threshold over the module's *testable logic*.
# Used by QA during review (interim gate) and to back a `make cover-check` target
# on the shared CI workflow.
#
# Usage:
#   test/coverage_gate.sh [THRESHOLD]
#   THRESHOLD defaults to $COVERAGE_THRESHOLD or 70 (percent).
#
# Behavior:
#   - Reuses ./coverage.out if present (produced by `make test`/`make cover`);
#     otherwise runs `go test ./... -covermode=atomic -coverprofile=coverage.out`.
#   - EXCLUDES non-logic code from the denominator so the number is a precise
#     signal that won't be diluted (or mask a real regression) as the codebase
#     grows: main entrypoints (cmd/*), generated code (zz_generated*, *.deepcopy.go),
#     and embed-only packages (api/schemas). Override via $COVERAGE_EXCLUDE (an
#     extended-regex of profile file paths to drop).
#   - Prints the excluded file set (no silent caps), the filtered total, then exits
#     non-zero if the filtered total is below the threshold.
#
# Note: clear the test cache (`go clean -testcache`) before trusting a number from
# a freshly-mutated tree — stale cached coverage can read low.
set -euo pipefail

THRESHOLD="${1:-${COVERAGE_THRESHOLD:-70}}"
PROFILE="${COVERAGE_PROFILE:-coverage.out}"
# Profile file paths matching this regex are excluded from the coverage denominator.
EXCLUDE_RE="${COVERAGE_EXCLUDE:-/cmd/|zz_generated|\.deepcopy\.go|/api/schemas/}"

if ! command -v go >/dev/null 2>&1; then
  echo "coverage_gate: go toolchain not found on PATH" >&2
  exit 2
fi

if [[ ! -f "$PROFILE" ]]; then
  echo "coverage_gate: $PROFILE not found — running tests to generate it..."
  go test ./... -covermode=atomic -coverprofile="$PROFILE"
fi

# Build a filtered profile: keep the leading `mode:` line, drop data lines whose
# file path matches the exclusion regex.
FILTERED="$(mktemp)"
trap 'rm -f "$FILTERED"' EXIT
head -1 "$PROFILE" > "$FILTERED"
tail -n +2 "$PROFILE" | grep -vE "$EXCLUDE_RE" >> "$FILTERED" || true

# Report what was excluded — transparency, never a silent cap.
echo "=== excluded from coverage denominator (regex: $EXCLUDE_RE) ==="
EXCLUDED_FILES="$(tail -n +2 "$PROFILE" | grep -E "$EXCLUDE_RE" | sed -E 's/:[0-9].*$//' | sort -u || true)"
if [[ -n "$EXCLUDED_FILES" ]]; then
  echo "$EXCLUDED_FILES" | sed 's/^/  /'
else
  echo "  (nothing matched the exclusion regex)"
fi

echo ""
echo "=== coverage by function (after exclusions) ==="
go tool cover -func="$FILTERED"

TOTAL_LINE="$(go tool cover -func="$FILTERED" | grep -E '^total:' | tail -1)"
TOTAL_PCT="$(echo "$TOTAL_LINE" | grep -oE '[0-9]+(\.[0-9]+)?%' | tr -d '%')"

if [[ -z "${TOTAL_PCT:-}" ]]; then
  echo "coverage_gate: could not parse total coverage from: $TOTAL_LINE" >&2
  exit 2
fi

echo ""
echo "testable-logic coverage: ${TOTAL_PCT}%  (threshold: ${THRESHOLD}%)"

# Float-safe comparison via awk.
if awk -v t="$TOTAL_PCT" -v th="$THRESHOLD" 'BEGIN { exit !(t + 0 < th + 0) }'; then
  echo "FAIL: coverage ${TOTAL_PCT}% is below threshold ${THRESHOLD}%" >&2
  exit 1
fi
echo "PASS: coverage gate satisfied"
