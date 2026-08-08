package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTypedFailureRun writes a run whose three failing attempts are exactly
// the shapes the 2026-08-08 audit found collapsed into one bucket: two
// dispatch failures the runner typed in its own namespace (an unauthorized
// clone, then a DNS outage on the infra retry) and one provider stage that
// failed with a typed code inline on stage.finished. Written as raw JSONL for
// the same reason writeFixtureRun is: a mismatch with the real on-disk shape
// must fail here rather than cancel itself out.
func writeTypedFailureRun(t *testing.T, runsDir, runID string, startedAt time.Time) string {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	runYAML := fmt.Sprintf(`schema: goobers.dev/journal/run/v1
runId: %s
workflow: implement
workflowVersion: 1
gaggle: site
trigger:
  kind: cron
startedAt: %s
`, runID, startedAt.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte(runYAML), 0o644); err != nil {
		t.Fatalf("write run.yaml: %v", err)
	}

	ts := func(offsetSeconds int) string {
		return startedAt.Add(time.Duration(offsetSeconds) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	lines := []string{
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":%q,"type":"run.started"}`, ts(0)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":2,"branch":0,"time":%q,"type":"stage.started","stage":"query-backlog","attempt":1}`, ts(1)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":3,"branch":0,"time":%q,"type":"error","stage":"query-backlog","attempt":1,"error":{"code":"executor_error","message":"prepare stage \"query-backlog\": create worktree: remote: Write access to repository not granted"},"runner":{"retryFailureClass":"policy","errorCode":"infra_git_failed","errorClass":"infra_git"}}`, ts(2)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":4,"branch":0,"time":%q,"type":"stage.started","stage":"query-backlog","attempt":2,"attemptClass":"infra"}`, ts(3)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":5,"branch":0,"time":%q,"type":"error","stage":"query-backlog","attempt":2,"attemptClass":"infra","error":{"code":"executor_error","message":"prepare stage \"query-backlog\": create worktree: Could not resolve host: github.com"},"runner":{"retryFailureClass":"infra","errorCode":"infra_net_failed","errorClass":"infra_net"}}`, ts(4)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":6,"branch":0,"time":%q,"type":"stage.started","stage":"open-pr","attempt":1}`, ts(5)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":7,"branch":0,"time":%q,"type":"stage.finished","stage":"open-pr","attempt":1,"status":"failure","error":{"code":"github_auth_failed","message":"403 on check-runs"}}`, ts(6)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":8,"branch":0,"time":%q,"type":"run.finished","status":"failed"}`, ts(7)),
	}
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}
	return dir
}

// TestIngestLandsTypedStageErrorClasses is the telemetry half of killing the
// executor_error/unknown bucket: the producer's typed cause must survive into
// stage_attempts and run_errors unchanged. Before it, every one of these rows
// read error_code=executor_error, error_class=unknown, runner_json NULL — 87%
// of one gaggle's failures, separable only by reading each journal by hand.
func TestIngestLandsTypedStageErrorClasses(t *testing.T) {
	tmp := t.TempDir()
	runDir := writeTypedFailureRun(t, filepath.Join(tmp, "runs"), fixtureRunID, fixtureStart)
	db := openTestDB(t, tmp)
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	type row struct {
		stage, status, code, class string
		runnerJSON                 sql.NullString
	}
	rows, err := db.sql.QueryContext(context.Background(), `
		SELECT stage, COALESCE(status,''), COALESCE(error_code,''), COALESCE(error_class,''), runner_json
		FROM stage_attempts ORDER BY stage, attempt`)
	if err != nil {
		t.Fatalf("query stage_attempts: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.stage, &r.status, &r.code, &r.class, &r.runnerJSON); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []row{
		{stage: "open-pr", status: "failure", code: "github_auth_failed", class: "provider"},
		{stage: "query-backlog", status: "failure", code: "infra_git_failed", class: "infra_git"},
		{stage: "query-backlog", status: "failure", code: "infra_net_failed", class: "infra_net"},
	}
	if len(got) != len(want) {
		t.Fatalf("stage_attempts rows = %d, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].stage != w.stage || got[i].status != w.status || got[i].code != w.code || got[i].class != w.class {
			t.Fatalf("stage_attempts[%d] = %+v, want stage=%s status=%s code=%s class=%s",
				i, got[i], w.stage, w.status, w.code, w.class)
		}
	}
	// The dispatch failures' own diagnostic context (retry classification) is
	// carried on the error event, which the rollup used to discard entirely.
	for _, r := range got[1:] {
		if !r.runnerJSON.Valid || !strings.Contains(r.runnerJSON.String, "retryFailureClass") {
			t.Fatalf("query-backlog runner_json = %v, want the error event's runner annotations", r.runnerJSON)
		}
	}

	errRows, err := db.sql.QueryContext(context.Background(),
		`SELECT code, COALESCE(error_class,'') FROM run_errors ORDER BY seq`)
	if err != nil {
		t.Fatalf("query run_errors: %v", err)
	}
	defer func() { _ = errRows.Close() }()
	var codes []string
	for errRows.Next() {
		var code, class string
		if err := errRows.Scan(&code, &class); err != nil {
			t.Fatalf("scan run_errors: %v", err)
		}
		codes = append(codes, code+"/"+class)
	}
	if err := errRows.Err(); err != nil {
		t.Fatalf("run_errors rows: %v", err)
	}
	wantCodes := []string{"infra_git_failed/infra_git", "infra_net_failed/infra_net", "github_auth_failed/provider"}
	if strings.Join(codes, ",") != strings.Join(wantCodes, ",") {
		t.Fatalf("run_errors = %v, want %v", codes, wantCodes)
	}
}

// TestIngestUntypedDispatchFailureStillClosesAttempt guards the compatibility
// edge: journals written before the producer typed its failures carry only
// error.code=executor_error, and that event must still be what closes the
// attempt — the typed refinement adds a cause, it never changes which events
// mark an attempt boundary.
func TestIngestUntypedDispatchFailureStillClosesAttempt(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dir := filepath.Join(runsDir, fixtureRunID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runYAML := fmt.Sprintf(`schema: goobers.dev/journal/run/v1
runId: %s
workflow: implement
workflowVersion: 1
gaggle: site
trigger:
  kind: cron
startedAt: %s
`, fixtureRunID, fixtureStart.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte(runYAML), 0o644); err != nil {
		t.Fatalf("write run.yaml: %v", err)
	}
	ts := func(o int) string {
		return fixtureStart.Add(time.Duration(o) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	lines := []string{
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":%q,"type":"run.started"}`, ts(0)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":2,"branch":0,"time":%q,"type":"stage.started","stage":"implement","attempt":1}`, ts(1)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":3,"branch":0,"time":%q,"type":"error","stage":"implement","attempt":1,"error":{"code":"executor_error","message":"legacy dispatch failure"}}`, ts(2)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":4,"branch":0,"time":%q,"type":"run.finished","status":"failed"}`, ts(3)),
	}
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}

	db := openTestDB(t, tmp)
	if err := db.IngestRun(context.Background(), dir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	var status, code, class string
	if err := db.sql.QueryRowContext(context.Background(), `
		SELECT COALESCE(status,''), COALESCE(error_code,''), COALESCE(error_class,'')
		FROM stage_attempts WHERE stage = 'implement'`).Scan(&status, &code, &class); err != nil {
		t.Fatalf("query stage_attempts: %v", err)
	}
	// executor_error now classifies as an executor defect rather than
	// "unknown" — the residual bucket is small and means something.
	if status != "failure" || code != "executor_error" || class != "executor" {
		t.Fatalf("attempt = (%s, %s, %s), want (failure, executor_error, executor)", status, code, class)
	}
}
