package dslmigrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
)

// BackupName identifies the one outstanding transaction per instance, outside
// scanned config. There is
// no append-only history: rollback consumes it; an operator may archive/remove
// it after accepting the migration. Never overwrite an unconsumed backup.
const BackupName = ".dsl21-migration-backup.json"
const maxBackupBytes = 8 << 20
const maxBackupFiles = 128
const transactionLockName = ".dsl21-migration.lock"

// FileChange is one source-preserving workflow edit within a bounded transaction.
type FileChange struct {
	Path          string // slash-separated, relative to the instance config directory
	Before, After string
}

type backupFile struct {
	Path         string `json:"path"`
	Before       string `json:"before"`
	BeforeSHA256 string `json:"beforeSHA256"`
	AfterSHA256  string `json:"afterSHA256"`
	Mode         uint32 `json:"mode"`
}

type migrationBackup struct {
	Version int          `json:"version"`
	Files   []backupFile `json:"files"`
}

func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func lockMigration(root string) (func(), error) {
	path := filepath.Join(root, transactionLockName)
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, fmt.Errorf("acquire migration lock %s (inspect a stale lock before removing it): %w", path, err)
	}
	return func() { _ = os.Remove(path) }, nil
}

// checkedWorkflowPath rejects traversal and symlink components, including the
// config directory, for both new transactions and untrusted backup manifests.
func checkedWorkflowPath(root, relative string) (string, error) {
	if relative == "." || path.IsAbs(relative) || path.Clean(relative) != relative || strings.ContainsAny(relative, "\\:") || relative == ".." || strings.HasPrefix(relative, "../") {
		return "", fmt.Errorf("unsafe migration path %q", relative)
	}
	if ext := filepath.Ext(relative); ext != ".yaml" && ext != ".yml" {
		return "", fmt.Errorf("migration path is not YAML: %q", relative)
	}
	path := root
	for _, component := range append([]string{"config"}, strings.Split(relative, "/")...) {
		path = filepath.Join(path, component)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("migration refuses symlink %s", path)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("migration requires a regular file: %s", path)
	}
	return path, nil
}

// WriteWithBackup preflights all originals before writing any source. Once the
// backup is durably saved, a partial write failure leaves it available for
// Rollback. A rollback accepts both original and migrated bytes, so interruption
// in either direction is recoverable without overwriting subsequent user edits.
func WriteWithBackup(root string, changes []FileChange) error {
	return writeWithBackup(root, changes, journal.WriteFileAtomic)
}

func writeWithBackup(root string, changes []FileChange, write func(string, []byte, os.FileMode) error) error {
	if len(changes) == 0 {
		return nil
	}
	if len(changes) > maxBackupFiles {
		return fmt.Errorf("migration exceeds %d-file backup bound; split the cohort", maxBackupFiles)
	}
	unlock, err := lockMigration(root)
	if err != nil {
		return err
	}
	defer unlock()
	backup := migrationBackup{Version: 1}
	seen := map[string]bool{}
	for _, change := range changes {
		if seen[change.Path] {
			return fmt.Errorf("duplicate migration path %q", change.Path)
		}
		seen[change.Path] = true
		path, err := checkedWorkflowPath(root, change.Path)
		if err != nil {
			return err
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(current) != change.Before {
			return fmt.Errorf("%s changed since preview; no files written", change.Path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		backup.Files = append(backup.Files, backupFile{Path: change.Path, Before: change.Before, BeforeSHA256: digest(change.Before), AfterSHA256: digest(change.After), Mode: uint32(info.Mode().Perm())})
	}
	data, err := json.Marshal(backup)
	if err != nil {
		return err
	}
	if len(data) > maxBackupBytes {
		return fmt.Errorf("migration exceeds %d-byte backup bound; split the cohort", maxBackupBytes)
	}
	backupPath := filepath.Join(root, BackupName)
	file, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create backup (never overwrites an existing backup): %w", err)
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(backupPath) // no source writes have begun
		return fmt.Errorf("save backup: %w", err)
	}
	if err := durability.SyncDir(root); err != nil {
		return fmt.Errorf("sync backup directory: %w; no sources written; backup retained", err)
	}
	for i, change := range changes {
		path, err := checkedWorkflowPath(root, change.Path)
		if err == nil {
			var current []byte
			current, err = os.ReadFile(path)
			if err == nil && string(current) != change.Before {
				err = fmt.Errorf("source changed after backup")
			}
		}
		if err == nil {
			err = write(path, []byte(change.After), os.FileMode(backup.Files[i].Mode))
		}
		if err != nil {
			return fmt.Errorf("write %s: %w; backup retained at %s; run goobers fix --rollback after resolving the error", change.Path, err, backupPath)
		}
	}
	return nil
}

func readBackup(root string) (*migrationBackup, error) {
	path := filepath.Join(root, BackupName)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBackupBytes {
		return nil, fmt.Errorf("backup must be a regular file of at most %d bytes", maxBackupBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBackupBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBackupBytes {
		return nil, fmt.Errorf("backup exceeds size bound")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var backup migrationBackup
	if err := dec.Decode(&backup); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("backup contains trailing data")
	}
	if backup.Version != 1 || len(backup.Files) == 0 || len(backup.Files) > maxBackupFiles {
		return nil, fmt.Errorf("unsupported or invalid backup")
	}
	seen := map[string]bool{}
	for _, file := range backup.Files {
		if seen[file.Path] || file.Mode > 0777 || digest(file.Before) != file.BeforeSHA256 {
			return nil, fmt.Errorf("corrupt backup entry %q", file.Path)
		}
		if decoded, err := hex.DecodeString(file.AfterSHA256); err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("invalid destination hash for %q", file.Path)
		}
		seen[file.Path] = true
	}
	return &backup, nil
}

// Rollback restores original bytes and modes only if every destination still
// matches this transaction's before/after hash. It consumes the sole backup on
// success. Failed or interrupted restores retain the recoverable transaction.
func Rollback(root string) error {
	unlock, err := lockMigration(root)
	if err != nil {
		return err
	}
	defer unlock()
	backup, err := readBackup(root)
	if err != nil {
		return fmt.Errorf("read migration backup: %w", err)
	}
	check := func(file backupFile) (string, error) {
		path, err := checkedWorkflowPath(root, file.Path)
		if err != nil {
			return "", err
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		hash := digest(string(current))
		if hash != file.BeforeSHA256 && hash != file.AfterSHA256 {
			return "", fmt.Errorf("%s changed since migration; refusing to overwrite user edits", file.Path)
		}
		return path, nil
	}
	for _, file := range backup.Files {
		if _, err := check(file); err != nil {
			return err
		}
	}
	for _, file := range backup.Files {
		path, err := check(file)
		if err != nil {
			return err
		}
		if err := journal.WriteFileAtomic(path, []byte(file.Before), os.FileMode(file.Mode)); err != nil {
			return fmt.Errorf("restore %s (backup retained): %w", file.Path, err)
		}
	}
	if err := os.Remove(filepath.Join(root, BackupName)); err != nil {
		return fmt.Errorf("sources restored but backup cleanup failed: %w", err)
	}
	return durability.SyncDir(root)
}
