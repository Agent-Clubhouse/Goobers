//go:build integration

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParentComposesExistingGooberThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	for _, harness := range []string{"claude-code", "codex"} {
		t.Run(harness, func(t *testing.T) {
			t.Setenv("GOOBERS_PARENT_QUALIFICATION_HARNESS", harness)
			qualifyContainedParentJourney(t, "agentic")
		})
	}
}

// Emit only fixed fixture classifications, never model output, MCP config or secrets.
func logQualificationFixturePhases(t *testing.T, root string) {
	t.Helper()
	marker := regexp.MustCompile(`(?m)^qualification (?:phase|failure code): [a-z0-9-]+$`)
	_ = filepath.WalkDir(filepath.Join(root, "artifacts"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 64<<10 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil {
			for _, line := range marker.FindAll(data, -1) {
				t.Logf("fixture: %s", line)
			}
		}
		return nil
	})
}
