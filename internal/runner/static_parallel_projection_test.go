package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// TestConformanceStaticParallelProjectionUnchanged pins the conformance
// projection of a parallel that declares no selection (design
// plan-driven-dynamic-fan-out.md §5.3, §14 case 7). The golden was captured
// before selective parallels existed, so any byte of drift — a new normative
// field rendered for every event, a reordered event, an extra lifecycle event
// for a non-selective parallel — fails here. Both execution paths are pinned:
// the serial walk (width 1) and the bounded dispatcher (width 3).
func TestConformanceStaticParallelProjectionUnchanged(t *testing.T) {
	for _, width := range []int32{1, 3} {
		t.Run(map[int32]string{1: "serial", 3: "dispatcher"}[width], func(t *testing.T) {
			runID := "static-projection"
			byTask := map[string]stubTaskResult{
				runID + ":lens-a":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 0}},
				runID + ":lens-b":  {status: apiv1.ResultNoWork},
				runID + ":lens-c":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 2}},
				runID + ":collate": {status: apiv1.ResultSuccess},
			}
			r, runsDir := newParallelTestRunner(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
				return &stubDeterministic{rec: rec, byTask: byTask}, nil
			})
			result, err := r.Start(context.Background(), StartInput{
				RunID: runID, Gaggle: "demo",
				Machine: parallelRunnerMachine(t, width, apiv1.WorkspaceScratch),
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if result.Phase != journal.PhaseCompleted {
				t.Fatalf("phase = %q, want completed", result.Phase)
			}
			rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
			if err != nil {
				t.Fatal(err)
			}
			events, err := rd.Events()
			if err != nil {
				t.Fatal(err)
			}
			var lines []string
			for branch := 0; branch <= 3; branch++ {
				for _, event := range journal.ConformanceBranches(events)[branch] {
					lines = append(lines, event.String())
				}
			}
			for _, event := range events {
				if event.Type != journal.EventParallelStarted && event.Type != journal.EventParallelFinished {
					continue
				}
				raw, err := json.Marshal(struct {
					Type         journal.EventType       `json:"type"`
					Parallel     string                  `json:"parallel"`
					Completeness []journal.BranchOutcome `json:"completeness"`
					Target       string                  `json:"target,omitempty"`
				}{event.Type, event.Parallel, event.Completeness, event.Target})
				if err != nil {
					t.Fatal(err)
				}
				lines = append(lines, string(raw))
				full, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(full), `"selection"`) || strings.Contains(string(full), `"reason"`) {
					t.Errorf("non-selective %s carries selective fields: %s", event.Type, full)
				}
			}
			got := strings.Join(lines, "\n") + "\n"
			path := filepath.Join("testdata", "static-parallel-projection.golden")
			if os.Getenv("UPDATE_GOLDEN") == "1" && width == 1 {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if got != string(want) {
				t.Fatalf("static parallel projection drifted at width %d; a parallel without selection must project byte-identically\n--- got\n%s--- want\n%s", width, got, want)
			}
		})
	}
}
