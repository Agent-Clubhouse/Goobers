package rollup

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCurationHealthRun(
	t *testing.T,
	runsDir, runID string,
	startedAt time.Time,
	depth int,
	actionOutputs map[string]any,
) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	runYAML := strings.ReplaceAll(minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: backlog-curation")
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), runYAML)

	healthOutputs, err := json.Marshal(map[string]any{
		"readyPoolDepth":         depth,
		"averageReadyAgeSeconds": 3600,
		"oldestReadyAgeSeconds":  7200,
		"readyPoolObservedAt":    startedAt.Add(time.Second).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		eventLine(1, startedAt, `"type":"run.started"`),
		eventLine(2, startedAt.Add(time.Second), `"type":"stage.started","stage":"sample-ready-pool","attempt":1`),
		eventLine(3, startedAt.Add(2*time.Second), `"type":"stage.finished","stage":"sample-ready-pool","attempt":1,"status":"success","outputs":`+string(healthOutputs)),
	}
	seq := 4
	if actionOutputs != nil {
		outputs, marshalErr := json.Marshal(actionOutputs)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		lines = append(lines,
			eventLine(seq, startedAt.Add(3*time.Second), `"type":"stage.started","stage":"curate","attempt":1`),
			eventLine(seq+1, startedAt.Add(4*time.Second), `"type":"stage.finished","stage":"curate","attempt":1,"status":"success","outputs":`+string(outputs)),
		)
		seq += 2
	}
	lines = append(lines, eventLine(seq, startedAt.Add(5*time.Second), `"type":"run.finished","status":"completed"`))
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
}

func TestCurationRollupCountsWindowAndStarvedReadyPool(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	outputs := map[string]any{
		"ready": 4, "needsHuman": 1, "closed": 2, "deduped": 1,
		"split": 1, "stale": 2, "reconciled": 3, "milestoned": 2, "bounced": 1,
	}
	writeCurationHealthRun(t, runsDir, "1111111111111111cccccccccccccccc", now.Add(-48*time.Hour), 6, outputs)
	writeCurationHealthRun(t, runsDir, "2222222222222222cccccccccccccccc", now.Add(-time.Hour), 0, nil)

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)
	if err := db.IngestRun(filepath.Join(runsDir, "2222222222222222cccccccccccccccc")); err != nil {
		t.Fatalf("reingest curation run: %v", err)
	}

	all, err := db.Stats(StatsRequest{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if all.Curation.Runs != 2 || all.Curation.ReportedRuns != 1 {
		t.Fatalf("curation run records = %#v", all.Curation)
	}
	if all.Curation.Ready != 4 || all.Curation.NeedsHuman != 1 ||
		all.Curation.Closed != 2 || all.Curation.Reconciled != 3 {
		t.Fatalf("curation counts = %#v", all.Curation)
	}
	if !all.ReadyPool.HasSample || !all.ReadyPool.Starved || all.ReadyPool.Depth != 0 {
		t.Fatalf("latest ready-pool health = %#v, want intentionally starved", all.ReadyPool)
	}
	if !all.ReadyPool.HasBounceRate || all.ReadyPool.BounceRate != 0.2 {
		t.Fatalf("bounce rate = %#v", all.ReadyPool)
	}

	windowed, err := db.Stats(StatsRequest{Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("windowed Stats: %v", err)
	}
	if windowed.Curation.Runs != 1 || windowed.Curation.ReportedRuns != 0 || windowed.Curation.Ready != 0 {
		t.Fatalf("windowed curation = %#v", windowed.Curation)
	}
	if !windowed.ReadyPool.HasSample || windowed.ReadyPool.Depth != 0 || !windowed.ReadyPool.Starved {
		t.Fatalf("windowed ready-pool health = %#v", windowed.ReadyPool)
	}
}

func TestReadyClaimAgeAndDemandAreQueryable(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	startedAt := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	runID := "3333333333333333cccccccccccccccc"
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	runYAML := strings.ReplaceAll(minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: implementation")
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), runYAML)
	readyAt := startedAt.Add(-6 * time.Hour).Format(time.RFC3339Nano)
	events := []string{
		eventLine(1, startedAt, `"type":"run.started"`),
		eventLine(2, startedAt.Add(time.Second), `"type":"stage.started","stage":"query-backlog","attempt":1`),
		eventLine(3, startedAt.Add(2*time.Second), fmt.Sprintf(`"type":"stage.finished","stage":"query-backlog","attempt":1,"status":"success","outputs":{"id":"42","updatedAt":%q}`, readyAt)),
		eventLine(4, startedAt.Add(3*time.Second), `"type":"ref.touched","externalRef":{"provider":"github","kind":"issue","id":"42"},"runner":{"operation":"claim"}`),
		eventLine(5, startedAt.Add(4*time.Second), `"type":"run.finished","status":"completed"`),
	}
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(events, "\n")+"\n")

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	stats, err := db.Stats(StatsRequest{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.ReadyPool.ClaimAgeSamples != 1 ||
		stats.ReadyPool.AverageClaimAgeSeconds != (6*time.Hour+2*time.Second).Seconds() ||
		stats.ReadyPool.ImplementationDemand != 1 {
		t.Fatalf("ready claim health = %#v", stats.ReadyPool)
	}
}
