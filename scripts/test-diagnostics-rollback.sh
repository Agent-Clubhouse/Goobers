#!/usr/bin/env bash
# Actual v0.4.1 package compatibility; no old model/daemon is dispatched.
set -euo pipefail
repo_root=$(git rev-parse --show-toplevel)
prior_revision=e24c075a12b1188771e5fc2246108fc24cfa29b6
cd "$repo_root"
if ! git cat-file -e "${prior_revision}^{commit}" 2>/dev/null; then
  git fetch --no-tags origin "$prior_revision"
fi
scratch_root=$(mktemp -d "${TMPDIR:-/tmp}/goobers-diagnostics-rollback.XXXXXX")
trap 'rm -rf "$scratch_root"' EXIT
mkdir -p "$scratch_root/prior" "$scratch_root/fixture"
export GOOBERS_ROLLBACK_FIXTURE="$scratch_root/fixture"
git archive "$prior_revision" | tar -x -C "$scratch_root/prior"
cmp internal/readmodel/schema.go "$scratch_root/prior/internal/readmodel/schema.go"
cp testdata/rollback/readmodel_test.go.txt "$scratch_root/prior/internal/readmodel/diagnostics_rollback_test.go"
cp testdata/rollback/config_test.go.txt "$scratch_root/prior/internal/instance/diagnostics_rollback_test.go"
go test -tags rollbackcompat ./internal/readmodel -run '^TestDiagnosticsRollbackPrepare$' -count=1 -timeout=3m -v
go test -tags rollbackcompat ./internal/instance -run '^TestDiagnosticsRollbackConfigPrepare$' -count=1 -timeout=3m -v
schema_snapshot() {
  python3 - "$GOOBERS_ROLLBACK_FIXTURE/read.db" <<'PY'
import json, sqlite3, sys
with sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True) as connection:
    print(json.dumps(connection.execute('SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name').fetchall()))
PY
}
schema_snapshot > "$scratch_root/schema-before.json"
(
 cd "$scratch_root/prior"
 go mod download
 go test ./internal/readmodel -run '^TestDiagnosticsPriorReleaseReadWrite$' -count=1 -timeout=3m -v
 go test ./internal/instance -run '^TestDiagnosticsRollbackPriorConfig$' -count=1 -timeout=3m -v
)
schema_snapshot > "$scratch_root/schema-old.json"
cmp "$scratch_root/schema-before.json" "$scratch_root/schema-old.json"
go test -tags rollbackcompat ./cmd/goobers -run '^TestDiagnosticsRollbackCLIRebuild$' -count=1 -timeout=3m -v
go test -tags rollbackcompat ./internal/readmodel -run '^TestDiagnosticsRollbackRestore$' -count=1 -timeout=3m -v
go test -tags rollbackcompat ./internal/instance -run '^TestDiagnosticsRollbackConfigVerify$' -count=1 -timeout=3m -v
schema_snapshot > "$scratch_root/schema-restored.json"
cmp "$scratch_root/schema-before.json" "$scratch_root/schema-restored.json"
