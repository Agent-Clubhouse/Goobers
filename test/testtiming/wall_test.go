package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWallBudgetWarnsWithoutFailing(t *testing.T) {
	for _, elapsed := range []time.Duration{899 * time.Second, 900 * time.Second, 901 * time.Second} {
		t.Run(elapsed.String(), func(t *testing.T) {
			dir := t.TempDir()
			budget := filepath.Join(dir, "budget.json")
			output := filepath.Join(dir, "timing.json")
			writeJSONFile(t, budget, budgetFile{SchemaVersion: schemaVersion, Jobs: map[string]jobBudget{
				"cloud-pg": {BaselineSeconds: 900, BudgetSeconds: 900, Baseline: "design allowance", RegressionTolerance: .5, AlertOnBudget: true},
			}})
			start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
			var stdout, stderr bytes.Buffer
			code := run([]string{"wall", "-job", "cloud-pg", "-started-at", start.Format(time.RFC3339),
				"-out", output, "-budget", budget, "-summary", filepath.Join(dir, "summary.md")},
				&stdout, &stderr, func() time.Time { return start.Add(elapsed) })
			if code != 0 {
				t.Fatalf("wall report failed: %d: %s", code, &stderr)
			}
			if warning := strings.Contains(stdout.String(), "::warning"); warning != (elapsed > 15*time.Minute) {
				t.Fatalf("unexpected warning: %s", &stdout)
			}
			recorded, err := readArtifact(output)
			if err != nil || recorded.ElapsedSeconds != elapsed.Seconds() || recorded.Job != "cloud-pg" {
				t.Fatalf("recorded=%+v error=%v", recorded, err)
			}
		})
	}
}

func TestWallRejectsInvalidStart(t *testing.T) {
	for _, start := range []string{"", "not-a-timestamp", "2026-10-23T00:00:00Z"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"wall", "-job", "cloud-pg", "-started-at", start,
			"-out", filepath.Join(t.TempDir(), "timing.json"), "-budget", "unused.json"},
			&stdout, &stderr, func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) })
		if code != 2 {
			t.Fatalf("start=%q code=%d stderr=%s", start, code, &stderr)
		}
	}
}
