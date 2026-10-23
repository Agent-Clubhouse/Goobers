//go:build cloudintegration

package telemetrypg

import (
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/telemetryclient"
)

// TestMergedReadMatchesLocalRollup is a file-vs-PG parity proof, not a fixture
// comparison. Real run journals are written to disk and ingested by the LOCAL
// rollup both one run at a time and all together; each single-run finding is
// applied to PG as that source stream's contribution, and the PG merged read
// must equal the local rollup over all runs: the combined 14-of-20 stage
// failure rate (not the later run's 9/10) and the summed error-signature count.
func TestMergedReadMatchesLocalRollup(t *testing.T) {
	store, _ := telemetryTestStore(t, "merged-parity")
	now := time.Now().UTC().Truncate(time.Microsecond)
	root := t.TempDir()
	all, err := rollup.Open(filepath.Join(root, "all.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = all.Close() }()
	thresholds := rollup.DefaultThresholds()
	thresholds.MinSamples, thresholds.MinErrorSignatureCount = 1, 1
	detect := func(db *rollup.DB) map[string]rollup.Finding {
		t.Helper()
		found, err := db.Detect(t.Context(), rollup.DetectRequest{
			StatsRequest: rollup.StatsRequest{Gaggle: "g", Workflow: "w", Since: now.Add(-time.Hour)},
			Thresholds:   thresholds,
		})
		if err != nil {
			t.Fatal(err)
		}
		byKey := map[string]rollup.Finding{}
		for _, finding := range found {
			if finding.Kind == rollup.FindingStageFailureRate || finding.Kind == rollup.FindingErrorSignature {
				byKey[string(finding.Kind)+"/"+finding.Subject] = finding
			}
		}
		return byKey
	}
	aggregateOf := map[string]telemetryclient.Aggregate{
		string(rollup.FindingStageFailureRate): telemetryclient.AggregateStageFailureRate,
		string(rollup.FindingErrorSignature):   telemetryclient.AggregateErrorSignature,
	}
	for i, failed := range []int{5, 9} {
		id := fmt.Sprintf("%032d", i+1)
		run, err := journal.Create(filepath.Join(root, "runs"), journal.RunIdentity{
			RunID: id, Gaggle: "g", Workflow: "w", WorkflowVersion: 1, StartedAt: now,
		}, nil, journal.WithClock(func() time.Time { return now }))
		if err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= 10; n++ {
			if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: n}); err != nil {
				t.Fatal(err)
			}
			status := "success"
			if n <= failed {
				status = "failure"
			}
			if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "build", Attempt: n, Status: status}); err != nil {
				t.Fatal(err)
			}
		}
		// Each run also surfaces the same error code a different number of times.
		for n := 0; n < i+2; n++ {
			if err := run.Append(journal.Event{Type: journal.EventError, Stage: "build", Attempt: 1, Error: &journal.ErrorDetail{Code: "stage_timeout", Message: "timed out"}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "runs", id)
		one, err := rollup.Open(filepath.Join(root, id+".db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := one.IngestRun(t.Context(), dir); err != nil {
			t.Fatal(err)
		}
		contribution := detect(one)
		_ = one.Close()
		if err := all.IngestRun(t.Context(), dir); err != nil {
			t.Fatal(err)
		}
		batch := Batch{Consumer: "rollup", Current: journal.StreamCursor{Stream: "run:" + id}, NextSeq: 22}
		for _, finding := range contribution {
			pointers := make([]telemetryclient.JournalPointer, len(finding.FlaggedRuns))
			for j, pointer := range finding.FlaggedRuns {
				pointers[j] = telemetryclient.JournalPointer{RunID: pointer.RunID, Seq: pointer.Seq}
			}
			batch.Aggregates = append(batch.Aggregates, AggregateRow{
				Gaggle: "g", Workflow: "w", Aggregate: aggregateOf[string(finding.Kind)],
				ObservedAt: now.Add(time.Duration(i) * time.Second),
				Finding: telemetryclient.Finding{
					Kind: string(finding.Kind), Subject: finding.Subject, Metrics: finding.Metrics,
					Threshold: finding.Threshold, FlaggedRuns: pointers, ErrorClass: finding.ErrorClass,
				},
			})
		}
		if len(batch.Aggregates) != 2 {
			t.Fatalf("run %d contributed %d findings, want stage + error signature", i, len(batch.Aggregates))
		}
		if err := store.Apply(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	local := detect(all)
	remote, err := store.DefectAggregates(t.Context(), httpapi.TelemetryDefectAggregateRequest{
		Gaggle: "g", Workflow: "w", Since: now.Add(-time.Hour),
		Aggregates: []telemetryclient.Aggregate{telemetryclient.AggregateStageFailureRate, telemetryclient.AggregateErrorSignature},
		Thresholds: telemetryclient.Thresholds{MinSamples: 1, MinErrorSignatureCount: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(remote.Findings) != len(local) {
		t.Fatalf("PG has %d findings, local rollup %d: %+v", len(remote.Findings), len(local), remote.Findings)
	}
	for _, got := range remote.Findings {
		want, ok := local[got.Kind+"/"+got.Subject]
		if !ok {
			t.Fatalf("PG finding %s/%s has no local counterpart", got.Kind, got.Subject)
		}
		if !maps.Equal(got.Metrics, want.Metrics) {
			t.Fatalf("%s/%s: PG metrics %v != local rollup %v", got.Kind, got.Subject, got.Metrics, want.Metrics)
		}
		if got.Threshold != want.Threshold || got.ErrorClass != want.ErrorClass {
			t.Fatalf("%s/%s: PG threshold/class %v/%q != local %v/%q", got.Kind, got.Subject, got.Threshold, got.ErrorClass, want.Threshold, want.ErrorClass)
		}
		gotRuns, wantRuns := pointerIDs(got.FlaggedRuns), localPointerIDs(want.FlaggedRuns)
		if fmt.Sprint(gotRuns) != fmt.Sprint(wantRuns) {
			t.Fatalf("%s/%s: PG flagged runs %v != local %v", got.Kind, got.Subject, gotRuns, wantRuns)
		}
	}
	if stage := local["stage-failure-rate/build"]; stage.Metrics["totalAttempts"] != 20 || stage.Metrics["failedAttempts"] != 14 {
		t.Fatalf("test precondition: local stage finding = %v, want 14 failures in 20 attempts", stage.Metrics)
	}
}

func pointerIDs(pointers []telemetryclient.JournalPointer) []string {
	ids := make([]string, len(pointers))
	for i, pointer := range pointers {
		ids[i] = pointer.RunID
	}
	sort.Strings(ids)
	return ids
}

func localPointerIDs(pointers []rollup.JournalPointer) []string {
	ids := make([]string, len(pointers))
	for i, pointer := range pointers {
		ids[i] = pointer.RunID
	}
	sort.Strings(ids)
	return ids
}
