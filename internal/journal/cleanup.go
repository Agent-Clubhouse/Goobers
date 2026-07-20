package journal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// CleanupSpansOnlyRuns removes legacy telemetry exhaust directories that
// contain only spans/. Real run journals and partially-created runs are left
// untouched.
func CleanupSpansOnlyRuns(runsDirs []string) (int, error) {
	removed := 0
	for _, runsDir := range runsDirs {
		entries, err := os.ReadDir(runsDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, fmt.Errorf("journal: read runs directory %s: %w", runsDir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			runDir := filepath.Join(runsDir, entry.Name())
			contents, err := os.ReadDir(runDir)
			if err != nil {
				return removed, fmt.Errorf("journal: inspect run directory %s: %w", runDir, err)
			}
			if len(contents) != 1 || contents[0].Name() != dirSpans || !contents[0].IsDir() {
				continue
			}
			if err := os.RemoveAll(runDir); err != nil {
				return removed, fmt.Errorf("journal: remove spans-only run directory %s: %w", runDir, err)
			}
			removed++
		}
	}
	return removed, nil
}
