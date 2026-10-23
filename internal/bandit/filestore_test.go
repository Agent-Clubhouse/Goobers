package bandit_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/bandit/conformance"
)

func TestFileObservationStore(t *testing.T) {
	conformance.Run(t, func(t *testing.T) bandit.ObservationStore {
		s, err := bandit.NewFileStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestFileObservationsSurviveReopenAndRejectCorruption(t *testing.T) {
	root := t.TempDir()
	s, err := bandit.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	r := conformance.Record("run-a", "build", 0, 1, 1)
	if err := s.Record(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	reopened, err := bandit.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Observations(t.Context(), "build"); err != nil || len(got) != 1 || got[0] != r.Observation {
		t.Fatalf("reopened result = %v, %v", got, err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("paths = %v, %v", paths, err)
	}
	if err := os.WriteFile(paths[0], []byte(`{"key":{"runId":"another-run"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Observations(t.Context(), "build"); err == nil {
		t.Fatal("corrupt history must fail closed")
	}
}
