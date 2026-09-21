//go:build rollbackcompat

package readmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/history"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// Invoked in separate processes around the actual pinned release package by
// go run ./test/diagnosticsrollback. This is not a simulated old decoder.
func TestDiagnosticsRollbackPrepare(t *testing.T) {
	root := rollbackFixtureRoot(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	run, err := journal.Create(filepath.Join(root, "runs"), testIdentity(), nil, journal.WithClock(func() time.Time { now = now.Add(time.Second); return now }))
	if err != nil {
		t.Fatal(err)
	}
	events := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventRunnerAnnotation, Stage: "implement", Attempt: 1, Runner: map[string]any{"kind": "execution-deadline.v1", "executionId": "rollback-execution", "deadline": now.Add(time.Hour).Format(time.RFC3339Nano), "executionState": "active"}},
		readinessEvent(0, "tool_authorization_failure"),
		journal.RetryBackoffEvent("other", 1, "engine", journal.AttemptInfra, now, now.Add(time.Minute)),
	}
	for _, event := range events {
		event.Seq = 0
		event.Time = time.Time{}
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.BuildFromJournals(ctx, []string{filepath.Join(root, "runs")}); err != nil {
		t.Fatal(err)
	}
	row, found, err := store.GetRun(ctx, testIdentity().RunID)
	if err != nil || !found {
		t.Fatalf("row=%+v found=%v err=%v", row, found, err)
	}
	if row.Operator.RequiredMCP == nil || len(row.Operator.RetryBackoff.Waits) != 1 || len(row.Operator.Activity.Active) != 1 || row.Operator.Activity.Active[0].ExecutionDeadline == nil {
		t.Fatalf("fixture lacks new facts: %+v", row.Operator)
	}
	rollbackWriteJSON(t, filepath.Join(root, "expected.json"), row.Operator)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := history.Open(ctx, filepath.Join(root, "diagnostics"), nil)
	if err != nil {
		t.Fatal(err)
	}
	attrs := map[string]any{"schemaVersion": 1, "deploymentId": "d", "instanceId": "i", "component": "daemon", "bootId": "b", "bootStartedAt": now.Format(time.RFC3339Nano), "sequence": 1, "observedAt": now.Format(time.RFC3339Nano), "windowStart": now.Format(time.RFC3339Nano), "windowCoverage": "complete", "state": "idle", "reasonCode": "no_eligible_work"}
	if err := h.Append(ctx, []telemetry.DiagnosticRecord{{Time: now, Name: "goobers.fleet.heartbeat", Attributes: attrs}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := history.Read(filepath.Join(root, "diagnostics"))
	if err != nil || len(snapshot.Records) != 1 {
		t.Fatalf("prepare history=%+v err=%v", snapshot, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "diagnostics", "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "history-before.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticsRollbackRestore(t *testing.T) {
	root := rollbackFixtureRoot(t)
	ctx := context.Background()
	store, err := Open(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	old, found, err := store.GetRun(ctx, testIdentity().RunID)
	if err != nil || !found {
		t.Fatalf("old row found=%v err=%v", found, err)
	}
	if old.Operator.RequiredMCP != nil || len(old.Operator.RetryBackoff.Waits) != 0 || len(old.Operator.Activity.Active) != 1 || old.Operator.Activity.Active[0].ExecutionDeadline != nil {
		t.Fatalf("prior writer did not exercise unknown-field loss: %+v", old.Operator)
	}
	// Same-position incremental reads are not a rebuild. Exercise the supported
	// library rebuild and atomic swap explicitly after downgrade. This is not
	// the user CLI, whose destructive rebuild is tested separately.
	rebuild, err := store.BeginRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuild.Abort() }()
	result, err := rebuild.Target().BuildFromJournals(ctx, []string{filepath.Join(root, "runs")})
	if err != nil || result.Projected != 1 {
		t.Fatalf("rebuild=%+v err=%v", result, err)
	}
	if err := rebuild.Swap(ctx); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.GetRun(ctx, testIdentity().RunID)
	if err != nil || !found {
		t.Fatalf("restored found=%v err=%v", found, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want OperatorFacts
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Operator.RequiredMCP, want.RequiredMCP) || !reflect.DeepEqual(got.Operator.RetryBackoff, want.RetryBackoff) || !reflect.DeepEqual(got.Operator.Activity, want.Activity) {
		t.Fatalf("journal rebuild lost diagnostics: got=%+v want=%+v", got.Operator, want)
	}
	if got.RunID != old.RunID || got.Workflow != old.Workflow || got.Gaggle != old.Gaggle || got.LastSeq != old.LastSeq {
		t.Fatal("rebuild changed core run identity or journal position")
	}
	before, err := os.ReadFile(filepath.Join(root, "history-before.json"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "diagnostics", "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rollback mutated private diagnostic snapshot")
	}
	snapshot, err := history.Read(filepath.Join(root, "diagnostics"))
	if err != nil || len(snapshot.Records) != 1 {
		t.Fatalf("history=%+v err=%v", snapshot, err)
	}
	info, err := os.Stat(filepath.Join(root, "diagnostics", "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot permissions=%v", info.Mode())
	}
}

func rollbackFixtureRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("GOOBERS_ROLLBACK_FIXTURE")
	if root == "" {
		t.Fatal("use go run ./test/diagnosticsrollback")
	}
	return root
}
func rollbackWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
