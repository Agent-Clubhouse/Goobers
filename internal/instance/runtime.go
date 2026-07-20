package instance

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// EnsureGaggleRuntime creates the runs and workcopies directories for gaggle.
func (l Layout) EnsureGaggleRuntime(gaggle string) error {
	if err := validateGagglePathName(gaggle); err != nil {
		return err
	}
	scoped := l.ForGaggle(gaggle)
	for _, dir := range []string{scoped.RunsDir(), scoped.WorkcopiesDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create gaggle runtime directory %s: %w", dir, err)
		}
	}
	return nil
}

// RunDirs returns every existing run-journal root in deterministic order.
// Scoped layouts return only their own root. An instance layout also includes
// the legacy flat root when present so pre-GAG-011 journals remain readable.
func (l Layout) RunDirs() ([]string, error) {
	if l.gaggle != "" {
		return []string{l.RunsDir()}, nil
	}

	var dirs []string
	if info, err := os.Lstat(l.RunsDir()); err == nil {
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			// A single-gaggle compatibility alias points at the scoped root,
			// which is discovered below. Do not scan it twice.
		case info.IsDir():
			dirs = append(dirs, l.RunsDir())
		default:
			return nil, fmt.Errorf("read runs directory: %s is not a directory", l.RunsDir())
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect legacy runs directory: %w", err)
	}

	entries, err := os.ReadDir(l.GagglesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return dirs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read gaggles directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runsDir := l.ForGaggle(entry.Name()).RunsDir()
		if info, err := os.Stat(runsDir); err == nil && info.IsDir() {
			dirs = append(dirs, runsDir)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("inspect runs directory for gaggle %q: %w", entry.Name(), err)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// FindRunDir resolves runID across scoped and legacy run roots.
func (l Layout) FindRunDir(runID string) (string, error) {
	if runID == "" || runID == "." || runID == ".." || filepath.Base(runID) != runID {
		return "", fmt.Errorf("invalid run id %q", runID)
	}
	runDirs, err := l.RunDirs()
	if err != nil {
		return "", err
	}
	var found string
	for _, runsDir := range runDirs {
		dir := filepath.Join(runsDir, runID)
		info, statErr := os.Lstat(dir)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return "", fmt.Errorf("inspect run %q: %w", runID, statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("run %q exists in multiple gaggle roots", runID)
		}
		found = dir
	}
	if found == "" {
		return "", fmt.Errorf("run %q: %w", runID, fs.ErrNotExist)
	}
	return found, nil
}

// MigrateLegacyRuntime moves a flat runs/workcopies layout into the sole active
// gaggle. A populated flat runtime is ambiguous when several gaggles are active,
// so startup fails with an actionable error instead of assigning data silently.
func (l Layout) MigrateLegacyRuntime(gaggles []string) error {
	names, err := normalizedGaggles(gaggles)
	if err != nil {
		return err
	}

	legacyHasData := false
	for _, legacy := range []string{l.RunsDir(), l.WorkcopiesDir()} {
		if info, statErr := os.Lstat(legacy); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		hasFiles, inspectErr := dirHasFiles(legacy)
		if inspectErr != nil {
			return fmt.Errorf("inspect legacy runtime directory %s: %w", legacy, inspectErr)
		}
		legacyHasData = legacyHasData || hasFiles
	}
	if legacyHasData && len(names) != 1 {
		return fmt.Errorf("legacy flat runs/workcopies contain data but %d gaggles are active; move each legacy directory under gaggles/<gaggle>/ before starting", len(names))
	}

	if len(names) == 1 {
		scoped := l.ForGaggle(names[0])
		for _, pair := range [][2]string{
			{l.RunsDir(), scoped.RunsDir()},
			{l.WorkcopiesDir(), scoped.WorkcopiesDir()},
		} {
			if err := migrateLegacyDir(pair[0], pair[1]); err != nil {
				return err
			}
		}
	} else if !legacyHasData {
		for _, legacy := range []string{l.RunsDir(), l.WorkcopiesDir()} {
			if err := os.Remove(legacy); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove empty legacy runtime directory %s: %w", legacy, err)
			}
		}
	}

	for _, gaggle := range names {
		if err := l.EnsureGaggleRuntime(gaggle); err != nil {
			return err
		}
	}
	if len(names) == 1 {
		scoped := l.ForGaggle(names[0])
		for _, pair := range [][2]string{
			{l.RunsDir(), scoped.RunsDir()},
			{l.WorkcopiesDir(), scoped.WorkcopiesDir()},
		} {
			if err := ensureLegacyRuntimeAlias(pair[0], pair[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func migrateLegacyDir(legacy, scoped string) error {
	if info, err := os.Lstat(legacy); err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(legacy)
		if err != nil {
			return fmt.Errorf("resolve legacy runtime alias %s: %w", legacy, err)
		}
		scopedAbs, err := filepath.EvalSymlinks(scoped)
		if err != nil {
			return err
		}
		if target != scopedAbs {
			return fmt.Errorf("legacy runtime alias %s points to %s, want %s", legacy, target, scopedAbs)
		}
		return nil
	}
	hasFiles, err := dirHasFiles(legacy)
	if err != nil {
		return fmt.Errorf("inspect legacy runtime directory %s: %w", legacy, err)
	}
	if !hasFiles {
		if err := os.Remove(legacy); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove empty legacy runtime directory %s: %w", legacy, err)
		}
		return os.MkdirAll(scoped, 0o755)
	}

	scopedHasFiles, err := dirHasFiles(scoped)
	if err != nil {
		return fmt.Errorf("inspect scoped runtime directory %s: %w", scoped, err)
	}
	if scopedHasFiles {
		return fmt.Errorf("cannot migrate legacy runtime %s: destination %s is not empty", legacy, scoped)
	}
	if err := os.Remove(scoped); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove empty migration destination %s: %w", scoped, err)
	}
	if err := os.MkdirAll(filepath.Dir(scoped), 0o755); err != nil {
		return fmt.Errorf("create migration parent for %s: %w", scoped, err)
	}
	if err := os.Rename(legacy, scoped); err != nil {
		return fmt.Errorf("migrate legacy runtime %s to %s: %w", legacy, scoped, err)
	}
	return nil
}

func ensureLegacyRuntimeAlias(legacy, scoped string) error {
	if _, err := os.Lstat(legacy); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect legacy runtime alias %s: %w", legacy, err)
	}
	target, err := filepath.Rel(filepath.Dir(legacy), scoped)
	if err != nil {
		return fmt.Errorf("resolve legacy runtime alias target: %w", err)
	}
	if err := os.Symlink(target, legacy); err != nil {
		return fmt.Errorf("create legacy runtime alias %s: %w", legacy, err)
	}
	return nil
}

func normalizedGaggles(gaggles []string) ([]string, error) {
	set := make(map[string]struct{}, len(gaggles))
	for _, gaggle := range gaggles {
		if err := validateGagglePathName(gaggle); err != nil {
			return nil, err
		}
		set[gaggle] = struct{}{}
	}
	names := make([]string, 0, len(set))
	for gaggle := range set {
		names = append(names, gaggle)
	}
	sort.Strings(names)
	return names, nil
}

func validateGagglePathName(gaggle string) error {
	if gaggle == "" || gaggle == "." || gaggle == ".." || filepath.Base(gaggle) != gaggle {
		return fmt.Errorf("invalid gaggle runtime path name %q", gaggle)
	}
	return nil
}
