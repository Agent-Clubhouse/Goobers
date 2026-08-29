package rollup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// writeRunWithCIFailure writes the smallest run journal that trips
// backfillCICheckFailures' scan loop: one failing stage whose finish event
// carries a ciStatus:"failing" output and a JSON artifact naming the failed
// check, matching the shape insertCICheckFailures expects.
func writeRunWithCIFailure(t *testing.T, runsDir, runID string, startedAt time.Time) string {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}

	runYAML := fmt.Sprintf(`schema: goobers.dev/journal/run/v1
runId: %s
workflow: merge-review
workflowVersion: 1
gaggle: web
trigger:
  kind: item
  ref: issue-42
startedAt: %s
`, runID, startedAt.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte(runYAML), 0o644); err != nil {
		t.Fatalf("write run.yaml: %v", err)
	}

	artifact := []byte(`{"checks":[{"name":"declared-dependency integration","state":"failing"}]}`)
	digest := journal.Digest(artifact)
	artifactPath := filepath.Join("artifacts", "ci-checks.json")
	if err := os.WriteFile(filepath.Join(dir, artifactPath), artifact, 0o644); err != nil {
		t.Fatalf("write ci-checks artifact: %v", err)
	}

	t0 := startedAt
	ts := func(offsetSeconds int) string {
		return t0.Add(time.Duration(offsetSeconds) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	lines := []string{
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":%q,"type":"run.started"}`, ts(0)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":2,"branch":0,"time":%q,"type":"stage.started","stage":"ci-poll","attempt":1,"attemptClass":"policy"}`, ts(1)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":3,"branch":0,"time":%q,"type":"stage.finished","stage":"ci-poll","attempt":1,"status":"failure","outputs":{"ciStatus":"failing"},"artifacts":[{"path":%q,"digest":%q,"size":%d,"mediaType":"application/json"}]}`,
			ts(2), filepath.ToSlash(artifactPath), digest, len(artifact)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":4,"branch":0,"time":%q,"type":"run.finished","status":"failed"}`, ts(3)),
	}
	events := "" +
		lines[0] + "\n" + lines[1] + "\n" + lines[2] + "\n" + lines[3] + "\n"
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte(events), 0o644); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}
	return dir
}

// TestBackfillCICheckFailuresDedupesSymlinkedRunsRoot is #3280's acceptance
// criterion: instanceRoot/runs commonly aliases one of the
// gaggles/<gaggle>/runs roots this same pass already scans (the documented
// legacy compat layout), so without dedup the same run is scanned — and its
// CI check failure inserted — twice, colliding on ci_check_failures'
// (run_id, seq, check_name) primary key and aborting the whole migration.
//
// Uses instance.CreateLegacyRuntimeAlias rather than os.Symlink to build the
// alias, so this exercises the platform-native form the real migration path
// creates — a directory junction on Windows, not a symlink (see
// internal/instance/runtime_alias_windows.go) — instead of only the
// Unix-symlink case.
func TestBackfillCICheckFailuresDedupesSymlinkedRunsRoot(t *testing.T) {
	tmp := t.TempDir()
	gaggleRuns := filepath.Join(tmp, "gaggles", "web", "runs")
	runID := fixtureRunID
	writeRunWithCIFailure(t, gaggleRuns, runID, fixtureStart)

	if err := instance.CreateLegacyRuntimeAlias(filepath.Join(tmp, "runs"), gaggleRuns); err != nil {
		t.Fatalf("create instanceRoot/runs alias -> gaggles/web/runs: %v", err)
	}

	db := openTestDB(t, tmp)
	ctx := context.Background()

	// Simulate a run that was ingested before ci_check_failures existed:
	// present in `runs`, absent from `ci_check_failures`, exactly what
	// migration 19's backfill exists to fix up.
	if _, err := db.sql.ExecContext(ctx, `
		INSERT INTO runs (run_id, workflow, workflow_version, gaggle, started_at)
		VALUES (?, 'merge-review', 1, 'web', ?)`,
		runID, formatTime(fixtureStart)); err != nil {
		t.Fatalf("seed runs row: %v", err)
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := backfillCICheckFailures(ctx, tx, tmp); err != nil {
		_ = tx.Rollback()
		t.Fatalf("backfillCICheckFailures: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var count int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM ci_check_failures WHERE run_id = ?`, runID).Scan(&count); err != nil {
		t.Fatalf("count ci_check_failures: %v", err)
	}
	if count != 1 {
		t.Fatalf("ci_check_failures rows for %s = %d, want 1 (run reachable via both instanceRoot/runs and gaggles/web/runs must backfill exactly once)", runID, count)
	}

	var checkName string
	if err := db.sql.QueryRowContext(ctx, `SELECT check_name FROM ci_check_failures WHERE run_id = ?`, runID).Scan(&checkName); err != nil {
		t.Fatalf("read check_name: %v", err)
	}
	if checkName != "declared-dependency integration" {
		t.Fatalf("check_name = %q, want %q", checkName, "declared-dependency integration")
	}
}

// TestCanonicalRunsRootDedupesSymlinkAlias exercises canonicalRunsRoot
// directly: a compat-aliased root (a symlink off Windows, a directory
// junction on Windows — see instance.CreateLegacyRuntimeAlias) and its real
// target must resolve to the same key, and a root that doesn't exist yet (no
// legacy dir, no compat alias) must resolve without error instead of
// aborting the migration.
func TestCanonicalRunsRootDedupesSymlinkAlias(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "gaggles", "web", "runs")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir real runs dir: %v", err)
	}
	link := filepath.Join(tmp, "runs")
	if err := instance.CreateLegacyRuntimeAlias(link, real); err != nil {
		t.Fatalf("create alias: %v", err)
	}

	realKey, err := canonicalRunsRoot(real)
	if err != nil {
		t.Fatalf("canonicalRunsRoot(real): %v", err)
	}
	linkKey, err := canonicalRunsRoot(link)
	if err != nil {
		t.Fatalf("canonicalRunsRoot(link): %v", err)
	}
	if realKey != linkKey {
		t.Fatalf("canonicalRunsRoot(real) = %q, canonicalRunsRoot(link) = %q, want equal", realKey, linkKey)
	}

	missing := filepath.Join(tmp, "gaggles", "other", "runs")
	missingKey, err := canonicalRunsRoot(missing)
	if err != nil {
		t.Fatalf("canonicalRunsRoot(missing): %v", err)
	}
	if missingKey == realKey {
		t.Fatalf("canonicalRunsRoot(missing) unexpectedly collided with the real root")
	}
}
