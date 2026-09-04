package journal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneMigrationBackupsBoundsGrowth(t *testing.T) {
	runsDir := filepath.Join(t.TempDir(), "runs")
	backupRoot := migrationBackupRoot(filepath.Join(runsDir, "placeholder"))
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	old := []string{"run-old.v0.bak", "run-old.v1.bak"}
	for _, name := range old {
		path := filepath.Join(backupRoot, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-MigrationBackupMaxAge-time.Hour), now.Add(-MigrationBackupMaxAge-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	recent := filepath.Join(backupRoot, "run-recent.v1.bak")
	if err := os.Mkdir(recent, 0o700); err != nil {
		t.Fatal(err)
	}
	unrecognized := filepath.Join(backupRoot, "keep-me")
	if err := os.Mkdir(unrecognized, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := PruneMigrationBackups(runsDir, now); err != nil {
		t.Fatalf("PruneMigrationBackups: %v", err)
	}
	for _, name := range old {
		if _, err := os.Stat(filepath.Join(backupRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("expired backup %s remains: %v", name, err)
		}
	}
	for _, name := range []string{"run-recent.v1.bak", "keep-me"} {
		if _, err := os.Stat(filepath.Join(backupRoot, name)); err != nil {
			t.Fatalf("entry %s was removed: %v", name, err)
		}
	}
}

func TestPruneMigrationBackupsIgnoresMissingRoot(t *testing.T) {
	if err := PruneMigrationBackups(filepath.Join(t.TempDir(), "runs"), time.Now()); err != nil {
		t.Fatalf("PruneMigrationBackups: %v", err)
	}
}
