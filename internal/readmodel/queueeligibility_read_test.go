package readmodel

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLatestQueueEligibilityScopesBeforeLimitAndKeepsNewProblems(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	add := func(id, gaggle, workflow string, at time.Time, problem string) {
		t.Helper()
		p := ProjectRun(testIdentity(), Projection{}, completedRunEvents())
		p.Run.RunID, p.Run.Gaggle, p.Run.Workflow = id, gaggle, workflow
		p.Stages, p.Nodes, p.NodeParents, p.Remediation = nil, nil, nil, nil
		p.Run.Operator.QueueEligibility = &QueueEligibilityEvidence{Seq: 1, Stage: "select", RecordedAt: formatTime(at), Problem: problem}
		if err := store.UpsertRun(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	add("older", "team", "review", now, "older observation")
	add("latest", "team", "review", now.Add(time.Nanosecond), "new report unavailable")
	add("foreign", "other", "review", now.Add(time.Hour), "other gaggle")
	add("other-workflow", "team", "implement", now.Add(time.Hour), "other workflow")
	add("legacy", "", "review", now.Add(time.Hour), "legacy scope")
	got, found, err := store.LatestQueueEligibility(ctx, "team", "review")
	if err != nil || !found || got.RunID != "latest" || got.Evidence.Problem != "new report unavailable" {
		t.Fatalf("scope/latest problem lost: %+v found=%t err=%v", got, found, err)
	}
	legacy, found, err := store.LatestQueueEligibility(ctx, "", "review")
	if err != nil || !found || legacy.RunID != "legacy" {
		t.Fatalf("empty scope became all gaggles: %+v found=%t err=%v", legacy, found, err)
	}
	if _, found, err := store.LatestQueueEligibility(ctx, "absent", "review"); err != nil || found {
		t.Fatalf("absent: found=%t err=%v", found, err)
	}
	if _, _, err := store.LatestQueueEligibility(ctx, "team", ""); err == nil {
		t.Fatal("empty workflow became unrestricted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LatestQueueEligibility(ctx, "team", "review"); err == nil {
		t.Fatal("closed store became no observation")
	}
}

func TestLatestQueueEligibilityUsesScopedOrderingIndex(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rows, err := store.reader.Query("EXPLAIN QUERY PLAN "+latestQueueEligibilitySQL, "team", "review")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "SEARCH run USING INDEX idx_run_queue_eligibility (gaggle=? AND workflow=?)") || strings.Contains(plan.String(), "TEMP B-TREE") {
		t.Fatalf("query lost bounded scoped ordering: %s", plan.String())
	}
}
