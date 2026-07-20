package journal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// SpansOnlyRunCandidates returns legacy telemetry exhaust directories that
// contain only spans/. Real run journals and partially-created runs are not
// candidates.
func SpansOnlyRunCandidates(runsDirs []string) ([]string, error) {
	var candidates []string
	for _, runsDir := range runsDirs {
		entries, err := os.ReadDir(runsDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return candidates, fmt.Errorf("journal: read runs directory %s: %w", runsDir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			runDir := filepath.Join(runsDir, entry.Name())
			candidate, err := isSpansOnlyRun(runDir)
			if err != nil {
				return candidates, err
			}
			if candidate {
				candidates = append(candidates, runDir)
			}
		}
	}
	sort.Strings(candidates)
	return candidates, nil
}

func isSpansOnlyRun(runDir string) (bool, error) {
	contents, err := os.ReadDir(runDir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("journal: inspect run directory %s: %w", runDir, err)
	}
	return len(contents) == 1 && contents[0].Name() == dirSpans && contents[0].IsDir(), nil
}

// RemoveSpansOnlyRuns removes previously reported cleanup candidates. Callers
// must place this destructive operation behind an explicit operator opt-in.
func RemoveSpansOnlyRuns(candidates []string) (int, error) {
	removed := 0
	for _, runDir := range candidates {
		candidate, err := isSpansOnlyRun(runDir)
		if err != nil {
			return removed, err
		}
		if !candidate {
			continue
		}
		if err := os.RemoveAll(runDir); err != nil {
			return removed, fmt.Errorf("journal: remove spans-only run directory %s: %w", runDir, err)
		}
		removed++
	}
	return removed, nil
}
