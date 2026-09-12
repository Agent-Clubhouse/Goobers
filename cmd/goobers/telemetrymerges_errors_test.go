package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestTelemetryMergesValidatesComparisonBeforeDatabaseWork(t *testing.T) {
	for _, flags := range [][]string{
		{"--shared-identities=shared"},
		{"--compare-github=invalid", "--shared-identities=shared"},
		{"--compare-github=acme/app"},
		{"--compare-github=acme/app", "--shared-identities=shared, "},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			root := t.TempDir()
			var stdout, stderr bytes.Buffer
			args := append(append([]string{"--rebuild"}, flags...), root)
			code := runTelemetryMergesAt(args, &stdout, &stderr, time.Now())
			if code != 2 || (!strings.Contains(stderr.String(), "--compare-github") && !strings.Contains(stderr.String(), "--shared-identities")) || stdout.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid flags performed database work: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestTelemetryMergesReportsOutputFailure(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(layout.TelemetryDB()), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, json := range []bool{false, true} {
		args := []string{layout.Root}
		if json {
			args = append([]string{"--json"}, args...)
		}
		var stderr bytes.Buffer
		if code := runTelemetryMergesAt(args, costFailWriter{}, &stderr, time.Now()); code != 2 || !strings.Contains(stderr.String(), "closed pipe") {
			t.Fatalf("json=%v code=%d stderr=%q", json, code, stderr.String())
		}
	}
}
