package daemonstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRefreshReadAndEvaluate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.lock")
	if err := Refresh(path, time.Now()); err == nil {
		t.Fatal("Refresh should fail when the lock does not exist")
	}

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tickAt := time.Date(2026, time.July, 23, 9, 0, 0, 0, time.UTC)
	if err := Refresh(path, tickAt); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(tickAt) {
		t.Fatalf("Read() = %s, want %s", got, tickAt)
	}

	fresh := Evaluate(tickAt.Add(30*time.Second), got, time.Minute)
	if !fresh.Healthy || fresh.Age != 30*time.Second {
		t.Fatalf("fresh liveness = %+v", fresh)
	}
	stale := Evaluate(tickAt.Add(2*time.Minute), got, time.Minute)
	if stale.Healthy || stale.Age != 2*time.Minute {
		t.Fatalf("stale liveness = %+v", stale)
	}
	future := Evaluate(tickAt.Add(-time.Second), got, time.Minute)
	if !future.Healthy || future.Age != 0 {
		t.Fatalf("future liveness = %+v", future)
	}
}
