package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func writeCurationHealthRun(
	t *testing.T,
	runsDir, runID string,
	startedAt time.Time,
	depth int,
	readyTransitions []map[string]any,
	reconciled int,
	actionOutputs map[string]any,
) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	runYAML := strings.ReplaceAll(minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: backlog-curation")
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), runYAML)

	healthReport := map[string]any{
		"readyPoolDepth":         depth,
		"averageReadyAgeSeconds": 3600,
		"oldestReadyAgeSeconds":  7200,
		"readyPoolObservedAt":    startedAt.Add(time.Second).Format(time.RFC3339Nano),
		"readyTransitions":       readyTransitions,
	}
	healthArtifact, err := json.Marshal(healthReport)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(healthArtifact)
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	artifactPath := filepath.Join("artifacts", "sha256", hexDigest[:2], hexDigest[2:])
	mustMkdirAll(t, filepath.Join(dir, filepath.Dir(artifactPath)))
	mustWriteFile(t, filepath.Join(dir, artifactPath), string(healthArtifact))
	healthOutputs, err := json.Marshal(map[string]any{
		"readyPoolDepth":         depth,
		"averageReadyAgeSeconds": 3600,
		"oldestReadyAgeSeconds":  7200,
		"readyPoolObservedAt":    startedAt.Add(time.Second).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	artifactJSON, err := json.Marshal([]map[string]any{{
		"path": artifactPath, "digest": digest, "size": len(healthArtifact), "mediaType": "application/json",
	}})
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		eventLine(1, startedAt, `"type":"run.started"`),
		eventLine(2, startedAt.Add(time.Second), `"type":"stage.started","stage":"reconcile-backlog","attempt":1`),
		eventLine(3, startedAt.Add(2*time.Second), fmt.Sprintf(`"type":"stage.finished","stage":"reconcile-backlog","attempt":1,"status":"success","outputs":{"reconciled":%d}`, reconciled)),
		eventLine(4, startedAt.Add(3*time.Second), `"type":"stage.started","stage":"sample-ready-pool","attempt":1`),
		eventLine(5, startedAt.Add(4*time.Second), `"type":"stage.finished","stage":"sample-ready-pool","attempt":1,"status":"success","outputs":`+string(healthOutputs)+`,"artifacts":`+string(artifactJSON)),
	}
	seq := 6
	if actionOutputs != nil {
		outputs, marshalErr := json.Marshal(actionOutputs)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		actionAt := startedAt.Add(time.Duration(seq) * time.Second)
		lines = append(lines,
			eventLine(seq, actionAt, `"type":"stage.started","stage":"curate","attempt":1`),
			eventLine(seq+1, actionAt.Add(time.Second), `"type":"stage.finished","stage":"curate","attempt":1,"status":"success","outputs":`+string(outputs)),
		)
		seq += 2
	}
	lines = append(lines, eventLine(seq, startedAt.Add(time.Duration(seq)*time.Second), `"type":"run.finished","status":"completed"`))
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
}

func TestCurationRollupCountsWindowAndStarvedReadyPool(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	outputs := map[string]any{
		"ready": 4, "needsHuman": 1, "closed": 2, "deduped": 1,
		"split": 1, "stale": 2, "milestoned": 2,
	}
	transitions := []map[string]any{
		{"eventId": 1, "itemId": "old", "label": "goobers:ready", "added": true, "occurredAt": now.Add(-72 * time.Hour)},
		{"eventId": 2, "itemId": "old", "label": "goobers:ready", "added": false, "occurredAt": now.Add(-2 * time.Hour)},
		{"eventId": 3, "itemId": "bounced", "label": "goobers:ready", "added": true, "occurredAt": now.Add(-3 * time.Hour)},
		{"eventId": 4, "itemId": "bounced", "label": "goobers:ready", "added": false, "occurredAt": now.Add(-2 * time.Hour)},
		{"eventId": 5, "itemId": "ready", "label": "goobers:ready", "added": true, "occurredAt": now.Add(-time.Hour)},
	}
	writeCurationHealthRun(t, runsDir, "1111111111111111cccccccccccccccc", now.Add(-48*time.Hour), 6, transitions, 3, outputs)
	writeCurationHealthRun(t, runsDir, "2222222222222222cccccccccccccccc", now.Add(-time.Hour), 0, transitions, 0, nil)

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)
	if err := db.IngestRun(context.Background(), filepath.Join(runsDir, "2222222222222222cccccccccccccccc")); err != nil {
		t.Fatalf("reingest curation run: %v", err)
	}

	all, err := db.Stats(context.Background(), StatsRequest{})
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
	if !all.ReadyPool.HasBounceRate || math.Abs(all.ReadyPool.BounceRate-(2.0/3.0)) > 0.000001 {
		t.Fatalf("bounce rate = %#v", all.ReadyPool)
	}
	if all.Curation.Bounced != 2 {
		t.Fatalf("actual bounce transitions = %d, want 2", all.Curation.Bounced)
	}

	windowed, err := db.Stats(context.Background(), StatsRequest{Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("windowed Stats: %v", err)
	}
	if windowed.Curation.Runs != 1 || windowed.Curation.ReportedRuns != 0 || windowed.Curation.Ready != 0 {
		t.Fatalf("windowed curation = %#v", windowed.Curation)
	}
	if !windowed.ReadyPool.HasSample || windowed.ReadyPool.Depth != 0 || !windowed.ReadyPool.Starved {
		t.Fatalf("windowed ready-pool health = %#v", windowed.ReadyPool)
	}
	if !windowed.ReadyPool.HasBounceRate || windowed.ReadyPool.BounceRate != 0.5 {
		t.Fatalf("windowed bounce cohort = %#v, want one bounce among two items readied in-window", windowed.ReadyPool)
	}
	if windowed.Curation.Bounced != 2 {
		t.Fatalf("windowed bounce transitions = %d, want both in-window removals", windowed.Curation.Bounced)
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
		eventLine(3, startedAt.Add(2*time.Second), fmt.Sprintf(`"type":"stage.finished","stage":"query-backlog","attempt":1,"status":"success","outputs":{"id":"42","readyAt":%q}`, readyAt)),
		eventLine(4, startedAt.Add(3*time.Second), `"type":"ref.touched","externalRef":{"provider":"github","kind":"issue","id":"42"},"runner":{"operation":"claim"}`),
		eventLine(5, startedAt.Add(4*time.Second), `"type":"run.finished","status":"completed"`),
	}
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(events, "\n")+"\n")

	db := openTestDB(t, tmp)
	if err := db.IngestRun(context.Background(), dir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	stats, err := db.Stats(context.Background(), StatsRequest{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.ReadyPool.ClaimAgeSamples != 1 ||
		stats.ReadyPool.AverageClaimAgeSeconds != (6*time.Hour+2*time.Second).Seconds() ||
		stats.ReadyPool.ImplementationDemand != 1 {
		t.Fatalf("ready claim health = %#v", stats.ReadyPool)
	}
}

// TestCurationEverRecordedDistinguishesFromEmptyWindow is #2278's core
// regression guard: a window with no curation rows must be distinguishable
// from a curation writer that has never fired at all — both otherwise
// present identically as Runs==0/counts==0/no-sample/no-bounce-rate.
func TestCurationEverRecordedDistinguishesFromEmptyWindow(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	outputs := map[string]any{
		"ready": 4, "needsHuman": 1, "closed": 2, "deduped": 1,
		"split": 1, "stale": 2, "milestoned": 2,
	}
	transitions := []map[string]any{
		{"eventId": 1, "itemId": "item", "label": "goobers:ready", "added": true, "occurredAt": now.Add(-3 * time.Hour)},
		{"eventId": 2, "itemId": "item", "label": "goobers:ready", "added": false, "occurredAt": now.Add(-2 * time.Hour)},
	}
	writeCurationHealthRun(t, runsDir, "4444444444444444cccccccccccccccc", now.Add(-time.Hour), 6, transitions, 3, outputs)

	db := openTestDB(t, tmp)
	if err := db.IngestRun(context.Background(), filepath.Join(runsDir, "4444444444444444cccccccccccccccc")); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	// A window entirely before the run: no rows fall inside it, but the
	// writer plainly HAS fired at some point.
	emptyWindow, err := db.Stats(context.Background(), StatsRequest{Until: now.Add(-48 * time.Hour)})
	if err != nil {
		t.Fatalf("Stats (empty window): %v", err)
	}
	if emptyWindow.Curation.Runs != 0 || emptyWindow.Curation.Ready != 0 {
		t.Fatalf("empty-window curation = %#v, want zero counts", emptyWindow.Curation)
	}
	if !emptyWindow.Curation.EverRecorded {
		t.Fatal("curation EverRecorded = false in an empty window that still has historical rows, want true")
	}
	if emptyWindow.ReadyPool.HasSample {
		t.Fatalf("empty-window ready-pool sample = %#v, want no sample in this window", emptyWindow.ReadyPool)
	}
	if !emptyWindow.ReadyPool.SampleEverRecorded {
		t.Fatal("ready-pool SampleEverRecorded = false despite a historical sample existing, want true")
	}
	if emptyWindow.ReadyPool.HasBounceRate {
		t.Fatalf("empty-window bounce rate = %#v, want none in this window", emptyWindow.ReadyPool)
	}
	if !emptyWindow.ReadyPool.BounceEverRecorded {
		t.Fatal("ready-pool BounceEverRecorded = false despite historical transitions existing, want true")
	}

	// A completely fresh instance: the writer has genuinely never fired.
	neverDB := openTestDB(t, t.TempDir())
	never, err := neverDB.Stats(context.Background(), StatsRequest{})
	if err != nil {
		t.Fatalf("Stats (never recorded): %v", err)
	}
	if never.Curation.EverRecorded || never.ReadyPool.SampleEverRecorded || never.ReadyPool.BounceEverRecorded {
		t.Fatalf("never-recorded stats = curation:%#v readyPool:%#v, want all EverRecorded flags false", never.Curation, never.ReadyPool)
	}
}

// writeOpenImplementationClaim hand-constructs a NON-terminal implementation
// run (no run.finished event) whose query-backlog stage already claimed an
// item — the shape a real run that is still executing later stages leaves
// behind, and the only way ready_claims can hold a row for a run
// IngestRun's caller reached before the run itself reached a terminal phase.
func writeOpenImplementationClaim(t *testing.T, runsDir, runID, itemID string, startedAt, claimedAt time.Time) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	runYAML := strings.ReplaceAll(minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: implementation")
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), runYAML)
	events := []string{
		eventLine(1, startedAt, `"type":"run.started"`),
		eventLine(2, startedAt.Add(time.Second), `"type":"stage.started","stage":"query-backlog","attempt":1`),
		eventLine(3, claimedAt, fmt.Sprintf(`"type":"stage.finished","stage":"query-backlog","attempt":1,"status":"success","outputs":{"id":%q,"readyAt":%q}`, itemID, claimedAt.Add(-time.Hour).Format(time.RFC3339Nano))),
	}
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(events, "\n")+"\n")
}

// TestInFlightClaimAgeExcludesClosedAndReflectsNow is #2279's coverage of no
// claims, one claim, multiple claims, and the exclusion of a claim whose run
// has already terminated (closed/released, per the issue's acceptance).
func TestInFlightClaimAgeExcludesClosedAndReflectsNow(t *testing.T) {
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)

	t.Run("no claims", func(t *testing.T) {
		db := openTestDB(t, t.TempDir())
		stats, err := db.Stats(context.Background(), StatsRequest{Now: now})
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.ReadyPool.InFlightClaimSamples != 0 ||
			stats.ReadyPool.AverageInFlightClaimAgeSeconds != 0 ||
			stats.ReadyPool.OldestInFlightClaimAgeSeconds != 0 {
			t.Fatalf("in-flight claim health = %#v, want all zero", stats.ReadyPool)
		}
	})

	t.Run("one claim", func(t *testing.T) {
		tmp := t.TempDir()
		runsDir := filepath.Join(tmp, "runs")
		claimedAt := now.Add(-90 * time.Minute)
		writeOpenImplementationClaim(t, runsDir, "5555555555555555dddddddddddddddd", "101", now.Add(-2*time.Hour), claimedAt)

		db := openTestDB(t, tmp)
		if err := db.IngestRun(context.Background(), filepath.Join(runsDir, "5555555555555555dddddddddddddddd")); err != nil {
			t.Fatalf("IngestRun: %v", err)
		}
		stats, err := db.Stats(context.Background(), StatsRequest{Now: now})
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		want := (90 * time.Minute).Seconds()
		if stats.ReadyPool.InFlightClaimSamples != 1 ||
			stats.ReadyPool.AverageInFlightClaimAgeSeconds != want ||
			stats.ReadyPool.OldestInFlightClaimAgeSeconds != want {
			t.Fatalf("in-flight claim health = %#v, want 1 sample at %v seconds", stats.ReadyPool, want)
		}
	})

	t.Run("multiple claims excludes a closed one", func(t *testing.T) {
		tmp := t.TempDir()
		runsDir := filepath.Join(tmp, "runs")
		youngerClaimedAt := now.Add(-30 * time.Minute)
		olderClaimedAt := now.Add(-3 * time.Hour)
		writeOpenImplementationClaim(t, runsDir, "6666666666666666dddddddddddddddd", "201", now.Add(-1*time.Hour), youngerClaimedAt)
		writeOpenImplementationClaim(t, runsDir, "7777777777777777dddddddddddddddd", "202", now.Add(-4*time.Hour), olderClaimedAt)

		// A third claim whose run has already finished — release-claim would
		// have freed the real claim ledger lease, and this run must not count
		// toward "currently open" regardless.
		closedDir := filepath.Join(runsDir, "8888888888888888dddddddddddddddd")
		mustMkdirAll(t, closedDir)
		closedStartedAt := now.Add(-5 * time.Hour)
		closedRunYAML := strings.ReplaceAll(minimalRunYAML("8888888888888888dddddddddddddddd", closedStartedAt), "workflow: wf", "workflow: implementation")
		mustWriteFile(t, filepath.Join(closedDir, fileRunYAML), closedRunYAML)
		closedReadyAt := closedStartedAt.Add(-time.Hour).Format(time.RFC3339Nano)
		closedEvents := []string{
			eventLine(1, closedStartedAt, `"type":"run.started"`),
			eventLine(2, closedStartedAt.Add(time.Second), `"type":"stage.started","stage":"query-backlog","attempt":1`),
			eventLine(3, closedStartedAt.Add(2*time.Second), fmt.Sprintf(`"type":"stage.finished","stage":"query-backlog","attempt":1,"status":"success","outputs":{"id":"203","readyAt":%q}`, closedReadyAt)),
			eventLine(4, closedStartedAt.Add(3*time.Second), `"type":"run.finished","status":"completed"`),
		}
		mustWriteFile(t, filepath.Join(closedDir, fileEvents), strings.Join(closedEvents, "\n")+"\n")

		db := openTestDB(t, tmp)
		for _, runID := range []string{
			"6666666666666666dddddddddddddddd",
			"7777777777777777dddddddddddddddd",
			"8888888888888888dddddddddddddddd",
		} {
			if err := db.IngestRun(context.Background(), filepath.Join(runsDir, runID)); err != nil {
				t.Fatalf("IngestRun %s: %v", runID, err)
			}
		}
		stats, err := db.Stats(context.Background(), StatsRequest{Now: now})
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		wantAvg := ((30 * time.Minute).Seconds() + (3 * time.Hour).Seconds()) / 2
		wantOldest := (3 * time.Hour).Seconds()
		if stats.ReadyPool.InFlightClaimSamples != 2 {
			t.Fatalf("in-flight claim samples = %d, want 2 (closed run excluded): %#v", stats.ReadyPool.InFlightClaimSamples, stats.ReadyPool)
		}
		if math.Abs(stats.ReadyPool.AverageInFlightClaimAgeSeconds-wantAvg) > 0.001 {
			t.Fatalf("average in-flight claim age = %v, want %v", stats.ReadyPool.AverageInFlightClaimAgeSeconds, wantAvg)
		}
		if stats.ReadyPool.OldestInFlightClaimAgeSeconds != wantOldest {
			t.Fatalf("oldest in-flight claim age = %v, want %v", stats.ReadyPool.OldestInFlightClaimAgeSeconds, wantOldest)
		}
		// AverageClaimAgeSeconds is unrelated to and unaffected by this
		// change — it counts every completed ready-to-claim transition
		// (ready_claims), regardless of whether the claiming run has since
		// terminated, so all three claims still land there.
		if stats.ReadyPool.ClaimAgeSamples != 3 {
			t.Fatalf("claim-age samples = %d, want 3 (pre-existing AverageClaimAgeSeconds semantics, untouched by #2279)", stats.ReadyPool.ClaimAgeSamples)
		}
	})
}
