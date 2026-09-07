package readmodel

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestEngineFallbackLegacyAndMalformedAnnotations(t *testing.T) {
	legacy := journal.Event{Type: journal.EventRunnerAnnotation, RunID: "run", Workflow: "work", Runner: map[string]any{"kind": journal.RunnerAnnotationEngineSelection, "reason": "old prose", "selfPinnedStages": []any{"implement"}}}
	value, ok := RunnerEngineFallback(legacy)
	if !ok || value.ReasonClass != "unknown" || value.SelfPinnedStages[0] != "implement" {
		t.Fatalf("legacy: %+v", value)
	}
	if got := value.After(journal.Event{Type: journal.EventStageStarted}); got != value {
		t.Fatal("unrelated event discarded evidence")
	}
	for _, fields := range []map[string]any{
		{"kind": "different"},
		{"kind": journal.RunnerAnnotationEngineSelection, "selfPinnedStages": 42},
		{"kind": journal.RunnerAnnotationEngineSelection, "bad": make(chan int)},
	} {
		if _, ok := RunnerEngineFallback(journal.Event{Type: journal.EventRunnerAnnotation, Runner: fields}); ok {
			t.Fatalf("malformed annotation accepted: %v", fields)
		}
	}
}
