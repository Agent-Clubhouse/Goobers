package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestTelemetryPruneOrphansDefaultsToReportAndRequiresSafeAge(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	root := initDeterministicDemo(t)
	runDir := filepath.Join(instance.NewLayout(root).ForGaggle("example").RunsDir(), "orphan-run")
	runsDir := filepath.Dir(runDir)
	creationDir := filepath.Join(journal.RunCreationStagingDir(runsDir), "unfinished-run-123456789")
	for _, dir := range []string{runDir, creationDir} {
		if err := os.MkdirAll(filepath.Join(dir, "spans"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-48 * time.Hour)
	for _, dir := range []string{runDir, creationDir} {
		if err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(path, old, old)
		}); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr strings.Builder
	if code := runTelemetryPruneOrphansAt([]string{root}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("report code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `would delete orphan="orphan-run"`) {
		t.Fatalf("report output = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), `would delete orphan="unfinished-run-123456789" source=creation-stage`) {
		t.Fatalf("creation-stage report output = %q", stdout.String())
	}
	for _, dir := range []string{runDir, creationDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("report removed orphan %s: %v", dir, err)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := runTelemetryPruneOrphansAt([]string{"--min-age=1h", "--delete", root}, &stdout, &stderr, now); code != 2 {
		t.Fatalf("unsafe age code = %d, stderr = %q", code, stderr.String())
	}
	for _, dir := range []string{runDir, creationDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("unsafe age removed orphan %s: %v", dir, err)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := runTelemetryPruneOrphansAt([]string{"--delete", root}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("delete code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `deleted orphan="orphan-run"`) {
		t.Fatalf("delete output = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), `deleted orphan="unfinished-run-123456789" source=creation-stage`) {
		t.Fatalf("creation-stage delete output = %q", stdout.String())
	}
	for _, dir := range []string{runDir, creationDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("orphan remains after delete %s: %v", dir, err)
		}
	}
}
