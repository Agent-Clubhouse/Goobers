package instance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/platform/durability"
)

const legacyRuntimeMigrationStateFile = "legacy-runtime-migration.json"

// RuntimeMigration reports populated legacy runtime directories moved into a
// gaggle-scoped layout.
type RuntimeMigration struct {
	ID        string   `json:"id"`
	Gaggle    string   `json:"gaggle"`
	MovedDirs []string `json:"movedDirectories"`
}

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
	return l.RunDirsContext(context.Background())
}

// RunDirsContext returns every existing run-journal root, checking ctx between
// filesystem operations. Individual filesystem calls may still block.
func (l Layout) RunDirsContext(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.gaggle != "" {
		return []string{l.RunsDir()}, nil
	}

	var dirs []string
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(l.RunsDir()); err == nil {
		alias, err := isLegacyRuntimeAlias(l.RunsDir(), info)
		if err != nil {
			return nil, fmt.Errorf("inspect legacy runs alias: %w", err)
		}
		switch {
		case alias:
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

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(l.GagglesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return dirs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read gaggles directory: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	return dirs, nil
}

// WorkcopiesDirs returns every existing managed-working-copy root in
// deterministic order, the same shape as RunDirs (see its doc): a scoped
// layout returns only its own root, while an instance layout also includes
// the legacy flat root when present (skipping it when it is a single-gaggle
// compatibility alias, so it is not scanned twice). Used to enumerate
// every gaggle's mirrors on the node — e.g. the object-cache GC helper's
// fail-closed dependents scan (#654, design §3 B3), which must check every
// gaggle's workcopies root, not just one.
func (l Layout) WorkcopiesDirs() ([]string, error) {
	if l.gaggle != "" {
		return []string{l.WorkcopiesDir()}, nil
	}

	var dirs []string
	if info, err := os.Lstat(l.WorkcopiesDir()); err == nil {
		alias, err := isLegacyRuntimeAlias(l.WorkcopiesDir(), info)
		if err != nil {
			return nil, fmt.Errorf("inspect legacy workcopies alias: %w", err)
		}
		switch {
		case alias:
			// A single-gaggle compatibility alias points at the scoped root,
			// which is discovered below. Do not scan it twice.
		case info.IsDir():
			dirs = append(dirs, l.WorkcopiesDir())
		default:
			return nil, fmt.Errorf("read workcopies directory: %s is not a directory", l.WorkcopiesDir())
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect legacy workcopies directory: %w", err)
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
		workcopiesDir := l.ForGaggle(entry.Name()).WorkcopiesDir()
		if info, err := os.Stat(workcopiesDir); err == nil && info.IsDir() {
			dirs = append(dirs, workcopiesDir)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("inspect workcopies directory for gaggle %q: %w", entry.Name(), err)
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
// gaggle. Multi-gaggle instances retain populated flat roots so their legacy
// journals and Git worktree metadata remain readable.
func (l Layout) MigrateLegacyRuntime(gaggles []string) error {
	_, err := l.MigrateLegacyRuntimeWithReport(gaggles)
	return err
}

// MigrateLegacyRuntimeWithReport performs or resumes MigrateLegacyRuntime and
// returns its durable report until CompleteLegacyRuntimeMigration acknowledges it.
func (l Layout) MigrateLegacyRuntimeWithReport(gaggles []string) (RuntimeMigration, error) {
	return l.migrateLegacyRuntimeWithReport(gaggles, durability.SyncDir)
}

func (l Layout) migrateLegacyRuntimeWithReport(gaggles []string, syncDir func(string) error) (RuntimeMigration, error) {
	names, err := normalizedGaggles(gaggles)
	if err != nil {
		return RuntimeMigration{}, err
	}

	pending, exists, err := l.readLegacyRuntimeMigration()
	if err != nil {
		return RuntimeMigration{}, err
	}
	if exists {
		if err := l.finishLegacyRuntimeMigration(pending.Gaggle, syncDir); err != nil {
			return RuntimeMigration{}, err
		}
		for _, gaggle := range names {
			if err := l.EnsureGaggleRuntime(gaggle); err != nil {
				return RuntimeMigration{}, err
			}
		}
		return pending, nil
	}

	var movedDirs []string
	for _, legacy := range []string{l.RunsDir(), l.WorkcopiesDir()} {
		if info, statErr := os.Lstat(legacy); statErr == nil {
			alias, aliasErr := isLegacyRuntimeAlias(legacy, info)
			if aliasErr != nil {
				return RuntimeMigration{}, fmt.Errorf("inspect legacy runtime alias %s: %w", legacy, aliasErr)
			}
			if alias {
				continue
			}
		}
		hasFiles, inspectErr := dirHasFiles(legacy)
		if inspectErr != nil {
			return RuntimeMigration{}, fmt.Errorf("inspect legacy runtime directory %s: %w", legacy, inspectErr)
		}
		if hasFiles {
			movedDirs = append(movedDirs, filepath.Base(legacy))
		}
	}
	legacyHasData := len(movedDirs) > 0
	scopedRuntimeExists, err := l.scopedRuntimeExists()
	if err != nil {
		return RuntimeMigration{}, err
	}
	preserveLegacy := legacyHasData && scopedRuntimeExists
	if len(names) == 1 && !preserveLegacy {
		migration := RuntimeMigration{}
		if legacyHasData {
			migration, err = newRuntimeMigration(names[0], movedDirs)
			if err != nil {
				return RuntimeMigration{}, err
			}
			if err := l.writeLegacyRuntimeMigration(migration); err != nil {
				return RuntimeMigration{}, err
			}
		}
		if err := l.finishLegacyRuntimeMigration(names[0], syncDir); err != nil {
			return RuntimeMigration{}, err
		}
		return migration, nil
	} else if !legacyHasData {
		for _, legacy := range []string{l.RunsDir(), l.WorkcopiesDir()} {
			info, err := os.Lstat(legacy)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return RuntimeMigration{}, fmt.Errorf("inspect legacy runtime directory %s: %w", legacy, err)
			}
			alias, err := isLegacyRuntimeAlias(legacy, info)
			if err != nil {
				return RuntimeMigration{}, fmt.Errorf("inspect legacy runtime alias %s: %w", legacy, err)
			}
			if alias {
				continue
			}
			if err := os.Remove(legacy); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return RuntimeMigration{}, fmt.Errorf("remove empty legacy runtime directory %s: %w", legacy, err)
			}
		}
	}

	for _, gaggle := range names {
		if err := l.EnsureGaggleRuntime(gaggle); err != nil {
			return RuntimeMigration{}, err
		}
	}
	return RuntimeMigration{}, nil
}

// CompleteLegacyRuntimeMigration clears the durable retry record after its
// instance-journal annotation has been committed.
func (l Layout) CompleteLegacyRuntimeMigration(migration RuntimeMigration) error {
	return l.completeLegacyRuntimeMigration(migration, durability.SyncDir)
}

func (l Layout) completeLegacyRuntimeMigration(migration RuntimeMigration, syncDir func(string) error) error {
	if len(migration.MovedDirs) == 0 {
		return nil
	}
	if err := validateRuntimeMigration(migration); err != nil {
		return err
	}
	pending, exists, err := l.readLegacyRuntimeMigration()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if pending.ID != migration.ID ||
		pending.Gaggle != migration.Gaggle ||
		strings.Join(pending.MovedDirs, "\x00") != strings.Join(migration.MovedDirs, "\x00") {
		return fmt.Errorf("complete legacy runtime migration: pending migration %q does not match %q", pending.ID, migration.ID)
	}
	if err := l.syncLegacyRuntimeMigrationMetadata(pending.Gaggle, syncDir); err != nil {
		return err
	}
	if err := os.Remove(l.legacyRuntimeMigrationPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove legacy runtime migration state: %w", err)
	}
	if err := durability.SyncDir(l.SchedulerDir()); err != nil {
		return fmt.Errorf("sync legacy runtime migration state removal: %w", err)
	}
	return nil
}

func (l Layout) finishLegacyRuntimeMigration(gaggle string, syncDir func(string) error) error {
	scoped := l.ForGaggle(gaggle)
	for _, pair := range [][2]string{
		{l.RunsDir(), scoped.RunsDir()},
		{l.WorkcopiesDir(), scoped.WorkcopiesDir()},
	} {
		if _, err := migrateLegacyDir(pair[0], pair[1]); err != nil {
			return err
		}
	}
	if err := l.EnsureGaggleRuntime(gaggle); err != nil {
		return err
	}
	for _, pair := range [][2]string{
		{l.RunsDir(), scoped.RunsDir()},
		{l.WorkcopiesDir(), scoped.WorkcopiesDir()},
	} {
		if err := ensureLegacyRuntimeAlias(pair[0], pair[1]); err != nil {
			return err
		}
	}
	return l.syncLegacyRuntimeMigrationMetadata(gaggle, syncDir)
}

func (l Layout) syncLegacyRuntimeMigrationMetadata(gaggle string, syncDir func(string) error) error {
	scoped := l.ForGaggle(gaggle)
	for _, dir := range []string{scoped.runtimeRoot(), l.GagglesDir(), l.Root} {
		if err := syncDir(dir); err != nil {
			return fmt.Errorf("sync legacy runtime migration metadata in %s: %w", dir, err)
		}
	}
	return nil
}

func newRuntimeMigration(gaggle string, movedDirs []string) (RuntimeMigration, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return RuntimeMigration{}, fmt.Errorf("generate legacy runtime migration id: %w", err)
	}
	migration := RuntimeMigration{
		ID:        hex.EncodeToString(idBytes),
		Gaggle:    gaggle,
		MovedDirs: append([]string(nil), movedDirs...),
	}
	if err := validateRuntimeMigration(migration); err != nil {
		return RuntimeMigration{}, err
	}
	return migration, nil
}

func validateRuntimeMigration(migration RuntimeMigration) error {
	id, err := hex.DecodeString(migration.ID)
	if err != nil || len(id) != 16 {
		return fmt.Errorf("invalid legacy runtime migration id %q", migration.ID)
	}
	if err := validateGagglePathName(migration.Gaggle); err != nil {
		return err
	}
	lastIndex := -1
	for _, dir := range migration.MovedDirs {
		var index int
		switch dir {
		case RunsDirName:
			index = 0
		case WorkcopiesDirName:
			index = 1
		default:
			return fmt.Errorf("invalid legacy runtime migration directory %q", dir)
		}
		if index <= lastIndex {
			return fmt.Errorf("legacy runtime migration directories are duplicated or out of order")
		}
		lastIndex = index
	}
	if lastIndex < 0 {
		return fmt.Errorf("legacy runtime migration has no moved directories")
	}
	return nil
}

func (l Layout) legacyRuntimeMigrationPath() string {
	return filepath.Join(l.SchedulerDir(), legacyRuntimeMigrationStateFile)
}

func (l Layout) readLegacyRuntimeMigration() (RuntimeMigration, bool, error) {
	data, err := os.ReadFile(l.legacyRuntimeMigrationPath())
	if errors.Is(err, fs.ErrNotExist) {
		return RuntimeMigration{}, false, nil
	}
	if err != nil {
		return RuntimeMigration{}, false, fmt.Errorf("read legacy runtime migration state: %w", err)
	}
	var migration RuntimeMigration
	if err := json.Unmarshal(data, &migration); err != nil {
		return RuntimeMigration{}, false, fmt.Errorf("parse legacy runtime migration state: %w", err)
	}
	if err := validateRuntimeMigration(migration); err != nil {
		return RuntimeMigration{}, false, err
	}
	return migration, true, nil
}

func (l Layout) writeLegacyRuntimeMigration(migration RuntimeMigration) error {
	if err := validateRuntimeMigration(migration); err != nil {
		return err
	}
	data, err := json.Marshal(migration)
	if err != nil {
		return fmt.Errorf("encode legacy runtime migration state: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		return fmt.Errorf("create legacy runtime migration state directory: %w", err)
	}
	if err := durability.SyncDir(l.Root); err != nil {
		return fmt.Errorf("sync legacy runtime migration state directory: %w", err)
	}
	temp, err := os.CreateTemp(l.SchedulerDir(), "."+legacyRuntimeMigrationStateFile+".*")
	if err != nil {
		return fmt.Errorf("create legacy runtime migration state: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write legacy runtime migration state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync legacy runtime migration state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close legacy runtime migration state: %w", err)
	}
	if err := durability.ReplaceFile(tempPath, l.legacyRuntimeMigrationPath()); err != nil {
		return fmt.Errorf("publish legacy runtime migration state: %w", err)
	}
	if err := durability.SyncDir(l.SchedulerDir()); err != nil {
		return fmt.Errorf("sync legacy runtime migration state publication: %w", err)
	}
	return nil
}

func (l Layout) scopedRuntimeExists() (bool, error) {
	gaggles, err := os.ReadDir(l.GagglesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read gaggle runtime directory: %w", err)
	}
	for _, gaggle := range gaggles {
		if !gaggle.IsDir() {
			continue
		}
		scoped := l.ForGaggle(gaggle.Name())
		for _, dir := range []string{scoped.RunsDir(), scoped.WorkcopiesDir()} {
			if _, err := os.Lstat(dir); err == nil {
				return true, nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return false, fmt.Errorf("inspect scoped runtime directory %s: %w", dir, err)
			}
		}
	}
	return false, nil
}

func migrateLegacyDir(legacy, scoped string) (bool, error) {
	if info, err := os.Lstat(legacy); err == nil {
		alias, err := isLegacyRuntimeAlias(legacy, info)
		if err != nil {
			return false, fmt.Errorf("inspect legacy runtime alias %s: %w", legacy, err)
		}
		if alias {
			target, err := ResolveRuntimeAlias(legacy)
			if err != nil {
				return false, fmt.Errorf("resolve legacy runtime alias %s: %w", legacy, err)
			}
			if isGeneratedRuntimeAlias(legacy, target) {
				return false, nil
			}
			scopedAbs, err := filepath.EvalSymlinks(scoped)
			if err != nil {
				return false, err
			}
			if target != scopedAbs {
				return false, fmt.Errorf("legacy runtime alias %s points to %s, want %s", legacy, target, scopedAbs)
			}
			return false, nil
		}
	}
	hasFiles, err := dirHasFiles(legacy)
	if err != nil {
		return false, fmt.Errorf("inspect legacy runtime directory %s: %w", legacy, err)
	}
	if !hasFiles {
		if err := os.Remove(legacy); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("remove empty legacy runtime directory %s: %w", legacy, err)
		}
		return false, os.MkdirAll(scoped, 0o755)
	}

	scopedHasFiles, err := dirHasFiles(scoped)
	if err != nil {
		return false, fmt.Errorf("inspect scoped runtime directory %s: %w", scoped, err)
	}
	if scopedHasFiles {
		return false, fmt.Errorf("cannot migrate legacy runtime %s: destination %s is not empty", legacy, scoped)
	}
	if err := os.Remove(scoped); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("remove empty migration destination %s: %w", scoped, err)
	}
	if err := os.MkdirAll(filepath.Dir(scoped), 0o755); err != nil {
		return false, fmt.Errorf("create migration parent for %s: %w", scoped, err)
	}
	if err := durability.Move(legacy, scoped); err != nil {
		return false, fmt.Errorf("migrate legacy runtime %s to %s: %w", legacy, scoped, err)
	}
	return true, nil
}

func isGeneratedRuntimeAlias(legacy, target string) bool {
	root, err := filepath.EvalSymlinks(filepath.Dir(legacy))
	if err != nil {
		return false
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	return len(parts) == 3 &&
		parts[0] == GagglesDirName &&
		validateGagglePathName(parts[1]) == nil &&
		parts[2] == filepath.Base(legacy)
}

func ensureLegacyRuntimeAlias(legacy, scoped string) error {
	if _, err := os.Lstat(legacy); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect legacy runtime alias %s: %w", legacy, err)
	}
	if err := CreateLegacyRuntimeAlias(legacy, scoped); err != nil {
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
