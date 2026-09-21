package featureusage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestScanActualDriverPinnedDSLAndAbsoluteWindows(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		id, dsl string
		driver  journal.RunDriver
	}{{"local", "2.0", ""}, {"remote", "3.0", journal.DriverEngine}} {
		run, err := journal.Create(dir, journal.RunIdentity{RunID: sample.id, Gaggle: "g", Workflow: "w", WorkflowVersion: 987, Driver: sample.driver}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: []byte(`{"dslVersion":"` + sample.dsl + `"}`)}, journal.WithClock(func() time.Time { return now }))
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": Kind, "featureId": "adapter.codex", "count": int64(1)}}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := run.Append(journal.Event{Type: journal.EventAgentLifecycle, Stage: "stage", Agent: &journal.AgentProvenance{Schema: "goobers.dev/journal/agent/v1", ID: "child", ParentID: "parent", RunID: sample.id, Stage: "stage", Attempt: 1, Lifecycle: journal.AgentStarted, StartedAt: now, UpdatedAt: now, Fidelity: journal.AgentFidelityPartial}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: "success"}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	first := Scan(context.Background(), dir, "g", now.Add(-time.Minute), now.Add(time.Second))
	for _, id := range []string{"runner.local", "runner.engine", "dsl.v2", "dsl.v3"} {
		if first[id].Value != 1 || !first[id].Complete {
			t.Fatalf("%s=%+v", id, first[id])
		}
	}
	if first["adapter.codex"].Value != 2 || first["adapter.codex"].Complete || first["provider.github"].Complete {
		t.Fatal(first)
	}
	if first["capability.nested-agents"].Value != 2 || first["capability.nested-agents"].Complete {
		t.Fatal("child lifecycle dedup/coverage wrong", first)
	}
	second := Scan(context.Background(), dir, "g", now.Add(-time.Minute), now.Add(time.Second))
	if !reflect.DeepEqual(first, second) {
		t.Fatal("rescan double-counted", first, second)
	}
	after := Scan(context.Background(), dir, "g", now.Add(time.Second), now.Add(time.Minute))
	if after["runner.local"].Value != 0 || !after["adapter.codex"].Complete {
		t.Fatal(after)
	}
	// Starting a new boot/window never carries historical usage into zero.
	if after["adapter.codex"].Value != 0 {
		t.Fatal(after)
	}
}
func TestScanMissingTruncatedOversizedAndCanceledRemainUnknown(t *testing.T) {
	now := time.Now()
	for _, setup := range []func(string){
		func(dir string) {},
		func(dir string) { _ = os.MkdirAll(filepath.Join(dir, "broken"), 0o700) },
		func(dir string) {
			_ = os.MkdirAll(filepath.Join(dir, "large"), 0o700)
			_ = os.WriteFile(filepath.Join(dir, "large", "run.yaml"), []byte(strings.Repeat("x", 65<<10)), 0o600)
		},
	} {
		dir := filepath.Join(t.TempDir(), "runs")
		setup(dir)
		counts := Scan(context.Background(), dir, "g", now.Add(-time.Minute), now)
		for id, count := range counts {
			if count.Complete {
				t.Fatalf("%s claimed coverage", id)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir, "one"), 0o700)
	for id, count := range Scan(ctx, dir, "g", now.Add(-time.Minute), now) {
		if count.Complete {
			t.Fatalf("%s claimed canceled coverage", id)
		}
	}
}

func BenchmarkFeatureScan(b *testing.B) {
	for _, runs := range []int{0, 16} {
		b.Run(fmt.Sprintf("runs-%d", runs), func(b *testing.B) {
			dir := b.TempDir()
			now := time.Now().UTC()
			for i := 0; i < runs; i++ {
				run, err := journal.Create(dir, journal.RunIdentity{RunID: fmt.Sprintf("run-%d", i), Gaggle: "g", Workflow: "w", WorkflowVersion: 1}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: []byte(`{"dslVersion":"2.0"}`)}, journal.WithClock(func() time.Time { return now }))
				if err != nil {
					b.Fatal(err)
				}
				if err := run.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				Scan(context.Background(), dir, "g", now.Add(-time.Minute), now.Add(time.Second))
			}
		})
	}
}

func TestScanRejectsCrossGaggleEvidence(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	run, err := journal.Create(dir, journal.RunIdentity{RunID: "wrong-gaggle", Gaggle: "other", Workflow: "w"}, nil, journal.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	for id, count := range Scan(context.Background(), dir, "g", now.Add(-time.Minute), now.Add(time.Second)) {
		if count.Value != 0 || count.Complete {
			t.Fatalf("cross-gaggle evidence attributed to %s: %+v", id, count)
		}
	}
}

func TestScanSequenceGapsNeverClaimCompleteCoverage(t *testing.T) {
	for _, missing := range []int{0, 1} {
		t.Run(fmt.Sprintf("missing-event-%d", missing), func(t *testing.T) {
			now := time.Now().UTC()
			dir := t.TempDir()
			run, err := journal.Create(dir, journal.RunIdentity{RunID: "run", Gaggle: "g", Workflow: "w"}, nil, journal.WithClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			for _, typ := range []journal.EventType{journal.EventRunnerAnnotation, journal.EventRunFinished} {
				if err := run.Append(journal.Event{Type: typ}); err != nil {
					t.Fatal(err)
				}
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "run", "events.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			lines = append(lines[:missing], lines[missing+1:]...)
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for id, count := range Scan(context.Background(), dir, "g", now.Add(-time.Minute), now.Add(time.Second)) {
				if count.Complete {
					t.Fatalf("gap claimed complete %s: %+v", id, count)
				}
			}
		})
	}
}
