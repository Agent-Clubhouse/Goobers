package scheduling

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestCompactionPreservesAppliedGeneration(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(layout.SchedulerDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	const generation = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	enc := json.NewEncoder(f)
	for _, event := range []journal.Event{
		{Schema: journal.EventSchema, Seq: 1, Time: old, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": journal.ConfigGenerationAppliedAnnotation, "configGeneration": generation}},
		{Schema: journal.EventSchema, Seq: 2, Time: time.Now(), Type: journal.EventTickSkipped},
	} {
		if err := enc.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	reader := AppliedExecutionGenerationReader(layout)
	if got, err := reader(t.Context()); err != nil || got != generation {
		t.Fatalf("before compact=%q err=%v", got, err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	result, err := journal.CompactInstanceEvents(layout.SchedulerDir(), cutoff, cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("compacted=%+v", result)
	if got, err := reader(t.Context()); err != nil || got != generation {
		t.Fatalf("compaction lost authoritative applied catalog: got=%q err=%v", got, err)
	}
}
