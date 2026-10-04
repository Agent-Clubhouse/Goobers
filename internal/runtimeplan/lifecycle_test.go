package runtimeplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupEvidenceGolden(t *testing.T) {
	var observations []Cleanup
	// Fake remote workers may report a stop without owning the work. That
	// assertion must never turn a host-owned/unknown cleanup into a guarantee.
	for _, state := range []string{"owned", "host-owned", "unsupported", "unobservable"} {
		got := CleanupObservation(state, "fixture worker", true)
		if got.RequiredGuaranteeSatisfied != (state == "owned") || got.AuthorizesWriter {
			t.Fatalf("unsafe cleanup authority for %s: %+v", state, got)
		}
		observations = append(observations, got)
	}
	for _, state := range []string{"owned", "", "new-worker-state"} {
		got := CleanupObservation(state, "fixture worker", false)
		if got.RequiredGuaranteeSatisfied || got.AuthorizesWriter {
			t.Fatalf("unknown cleanup granted authority: %+v", got)
		}
	}
	data, err := json.MarshalIndent(observations, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", "cleanup.golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(data) {
		t.Fatalf("cleanup golden mismatch:\n%s", data)
	}
}
