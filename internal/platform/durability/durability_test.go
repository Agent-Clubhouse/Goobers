package durability

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFileReplacesExistingDestination(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "state.json.tmp")
	destination := filepath.Join(directory, "state.json")
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(source, destination); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("destination = %q, want new content", got)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists after replacement: %v", err)
	}
}

func TestMoveRenamesDirectory(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "legacy")
	destination := filepath.Join(root, "scoped")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "state"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Move(source, destination); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "state")); err != nil {
		t.Fatalf("moved content: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists after move: %v", err)
	}
}

// RemoveFile is the delete half of the atomic-write protocol (#3562): the
// write path has waited out transient Windows contention since it was written
// and the delete path had no such retry, so a file this package had just
// published could refuse to be deleted and the caller treated that as final.
// The behaviour asserted here is the part that is the same everywhere — the
// file goes away, and an absent file reports ErrNotExist rather than success —
// so a platform that grew a retry cannot quietly change what the call MEANS.
func TestRemoveFileDeletesAndReportsAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stop-request")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFile(path); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after RemoveFile = %v, want ErrNotExist", err)
	}
	if err := RemoveFile(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RemoveFile on an absent path = %v, want ErrNotExist — callers "+
			"distinguish \"already consumed\" from \"could not consume\"", err)
	}
}
