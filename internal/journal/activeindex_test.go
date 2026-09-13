package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActiveRunIndexTracksUntilTerminalFinalization(t *testing.T) {
	runsDir := filepath.Join(t.TempDir(), "runs")
	runID := "0123456789abcdef0123456789abcdef"
	runDir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if created, err := MarkRunActive(runsDir, runID); err != nil || !created {
		t.Fatalf("MarkRunActive: created=%t err=%v", created, err)
	}
	if created, err := MarkRunActive(runsDir, runID); err != nil || created {
		t.Fatalf("idempotent MarkRunActive: created=%t err=%v", created, err)
	}
	got, err := ActiveRunDirs(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != runDir {
		t.Fatalf("ActiveRunDirs = %v, want [%s]", got, runDir)
	}
	if err := ClearRunActive(runDir); err != nil {
		t.Fatal(err)
	}
	got, err = ActiveRunDirs(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ActiveRunDirs after clear = %v, want empty", got)
	}
}
