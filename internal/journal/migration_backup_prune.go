package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MigrationBackupMaxAge is the stock retention window for schema migration
// rollback copies.
const MigrationBackupMaxAge = 30 * 24 * time.Hour

// PruneMigrationBackups removes completed migration backups older than the
// retention window. In-progress staging directories and unrecognized entries
// are left untouched.
func PruneMigrationBackups(runsDir string, now time.Time) error {
	root := migrationBackupRoot(filepath.Join(runsDir, "placeholder"))
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("journal: read migration backup directory: %w", err)
	}

	cutoff := now.Add(-MigrationBackupMaxAge)
	for _, entry := range entries {
		if !entry.IsDir() || !isMigrationBackupName(entry.Name()) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("journal: inspect migration backup %s: %w", path, err)
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("journal: remove expired migration backup %s: %w", path, err)
		}
	}
	return nil
}

func isMigrationBackupName(name string) bool {
	versionStart := strings.LastIndex(name, ".v")
	if versionStart <= 0 || !strings.HasSuffix(name, ".bak") {
		return false
	}
	version := name[versionStart+2 : len(name)-len(".bak")]
	if version == "" {
		return false
	}
	_, err := strconv.Atoi(version)
	return err == nil
}
