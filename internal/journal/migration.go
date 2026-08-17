package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/platform/durability"
)

// SchemaInfo is the inspectable schema metadata persisted in schema.json.
type SchemaInfo struct {
	Version       int              `json:"version"`
	MinimumBinary string           `json:"minimumBinary"`
	Migration     *SchemaMigration `json:"migration,omitempty"`
}

// SchemaMigration marks a transaction whose payload changes have not committed.
// Version is set to -1 while this is present so binaries that do not understand
// the marker fail closed instead of opening a partially migrated journal.
type SchemaMigration struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type journalMigration struct {
	minimumBinary string
	apply         func(string) error
}

// journalMigrations is append-only. The slice index is the source version and
// index+1 is the destination version. Version 1 adds schema.json around the
// original v1 payload formats without rewriting immutable journal content.
var journalMigrations = []journalMigration{
	{
		minimumBinary: "6c4c43c6",
		apply:         func(string) error { return nil },
	},
}

func minimumBinaryForJournalSchema(schemaVersion int) string {
	if schemaVersion < 1 || schemaVersion > len(journalMigrations) {
		return ""
	}
	return journalMigrations[schemaVersion-1].minimumBinary
}

func ensureJournalSchema(dir string) (SchemaInfo, error) {
	return ensureJournalSchemaWithOperations(dir, writeSchemaInfo, restoreJournalBackup)
}

func ensureJournalSchemaWithOperations(
	dir string,
	writeSchema func(string, SchemaInfo) error,
	restoreBackup func(string, int) error,
) (SchemaInfo, error) {
	info, exists, err := readSchemaInfo(dir)
	if err != nil {
		return SchemaInfo{}, err
	}
	if done, err := admitJournalSchema(dir, info, exists); done || err != nil {
		return info, err
	}

	held, err := acquireJournalLockPath(filepath.Join(dir, fileSchemaLock), dir, "schema migration")
	if err != nil {
		return SchemaInfo{}, fmt.Errorf("journal: acquire schema migration lock: %w", err)
	}
	defer releaseJournalLock(held)

	info, exists, err = readSchemaInfo(dir)
	if err != nil {
		return SchemaInfo{}, err
	}
	if done, err := admitJournalSchema(dir, info, exists); done || err != nil {
		return info, err
	}

	writerLock, err := acquireRunLock(dir)
	if err != nil {
		return SchemaInfo{}, fmt.Errorf("journal: wait for active writer before schema migration: %w", err)
	}
	defer releaseRunLock(writerLock)

	info, exists, err = readSchemaInfo(dir)
	if err != nil {
		return SchemaInfo{}, err
	}
	if info.Migration != nil {
		if err := restoreBackup(dir, info.Migration.From); err != nil {
			return SchemaInfo{}, fmt.Errorf("journal: restore interrupted schema migration: %w", err)
		}
		info, exists, err = readSchemaInfo(dir)
		if err != nil {
			return SchemaInfo{}, err
		}
		if done, admitErr := admitJournalSchema(dir, info, exists); done || admitErr != nil {
			return info, admitErr
		}
	}

	if err := validateJournalPayloadSchemas(dir); err != nil {
		return SchemaInfo{}, err
	}
	sourceVersion := info.Version
	if err := backupJournal(dir, sourceVersion); err != nil {
		return SchemaInfo{}, err
	}
	inProgress := SchemaInfo{
		Version:       -1,
		MinimumBinary: minimumBinaryForJournalSchema(CurrentSchemaVersion),
		Migration: &SchemaMigration{
			From: sourceVersion,
			To:   CurrentSchemaVersion,
		},
	}
	if err := writeSchema(dir, inProgress); err != nil {
		return SchemaInfo{}, fmt.Errorf("journal: record schema migration transaction: %w", err)
	}

	for i := sourceVersion; i < len(journalMigrations); i++ {
		if err := journalMigrations[i].apply(dir); err != nil {
			migrationErr := fmt.Errorf("journal: apply migration %d: %w", i+1, err)
			return SchemaInfo{}, rollbackMigration(dir, sourceVersion, migrationErr, restoreBackup)
		}
	}
	info = SchemaInfo{
		Version:       CurrentSchemaVersion,
		MinimumBinary: minimumBinaryForJournalSchema(CurrentSchemaVersion),
	}
	if err := writeSchema(dir, info); err != nil {
		migrationErr := fmt.Errorf("journal: record schema version %d: %w", CurrentSchemaVersion, err)
		return SchemaInfo{}, rollbackMigration(dir, sourceVersion, migrationErr, restoreBackup)
	}
	return info, nil
}

func admitJournalSchema(dir string, info SchemaInfo, exists bool) (bool, error) {
	if info.Migration != nil {
		if info.Version != -1 {
			return false, fmt.Errorf("journal: schema migration marker has invalid version %d", info.Version)
		}
		if info.MinimumBinary == "" {
			return false, errors.New("journal: schema.json minimumBinary is required")
		}
		if info.Migration.From < 0 || info.Migration.To <= info.Migration.From {
			return false, fmt.Errorf(
				"journal: invalid schema migration transaction from version %d to %d",
				info.Migration.From, info.Migration.To,
			)
		}
		if info.Migration.To > CurrentSchemaVersion {
			return false, fmt.Errorf(
				"journal: migration to schema version %d is newer than supported version %d; minimum binary is %s",
				info.Migration.To, CurrentSchemaVersion, info.MinimumBinary,
			)
		}
		return false, nil
	}
	if info.Version > CurrentSchemaVersion {
		minimum := info.MinimumBinary
		if minimum == "" {
			minimum = fmt.Sprintf("a Goobers build supporting journal schema %d", info.Version)
		}
		return false, fmt.Errorf(
			"journal: schema version %d is newer than supported version %d; minimum binary is %s",
			info.Version, CurrentSchemaVersion, minimum,
		)
	}
	if info.Version < 0 {
		return false, fmt.Errorf("journal: invalid schema version %d", info.Version)
	}
	if exists && info.Version == 0 {
		return false, errors.New(
			"journal: schema version 0 is valid only for legacy journals without schema.json; remove the invalid manifest or restore the journal from backup",
		)
	}
	if exists && info.MinimumBinary == "" {
		return false, errors.New("journal: schema.json minimumBinary is required")
	}
	if info.Version == CurrentSchemaVersion {
		return true, nil
	}
	return false, nil
}

func readSchemaInfo(dir string) (SchemaInfo, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, fileSchema))
	if errors.Is(err, os.ErrNotExist) {
		return SchemaInfo{}, false, nil
	}
	if err != nil {
		return SchemaInfo{}, false, fmt.Errorf("journal: read schema.json: %w", err)
	}
	var manifest struct {
		Version       *int             `json:"version"`
		MinimumBinary string           `json:"minimumBinary"`
		Migration     *SchemaMigration `json:"migration,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return SchemaInfo{}, false, fmt.Errorf("journal: parse schema.json: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return SchemaInfo{}, false, fmt.Errorf("journal: parse schema.json: %w", err)
	}
	if manifest.Version == nil {
		return SchemaInfo{}, false, errors.New("journal: schema.json version is required")
	}
	return SchemaInfo{
		Version:       *manifest.Version,
		MinimumBinary: manifest.MinimumBinary,
		Migration:     manifest.Migration,
	}, true, nil
}

func writeSchemaInfo(dir string, info SchemaInfo) error {
	return writeSchemaInfoWithWriter(dir, info, writeFileAtomic)
}

func writeSchemaInfoWithWriter(
	dir string,
	info SchemaInfo,
	write func(string, []byte, os.FileMode) error,
) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("journal: marshal schema.json: %w", err)
	}
	data = append(data, '\n')
	if err := write(filepath.Join(dir, fileSchema), data, 0o644); err != nil {
		return fmt.Errorf("journal: write schema.json: %w", err)
	}
	return nil
}

func validateJournalPayloadSchemas(dir string) error {
	runData, err := os.ReadFile(filepath.Join(dir, fileRunYAML))
	if err != nil {
		return fmt.Errorf("journal: read run.yaml: %w", err)
	}
	var run struct {
		Schema string `json:"schema"`
	}
	if err := yaml.Unmarshal(runData, &run); err != nil {
		return fmt.Errorf("journal: parse run.yaml: %w", err)
	}
	if run.Schema != RunSchema {
		return unsupportedPayloadSchema("run", run.Schema, RunSchema)
	}

	stateData, err := os.ReadFile(filepath.Join(dir, fileState))
	if err == nil {
		var state struct {
			Schema string `json:"schema"`
		}
		// A corrupt checkpoint remains recoverable from the event log. Only a
		// successfully parsed, explicitly unsupported state schema is rejected.
		if json.Unmarshal(stateData, &state) == nil && state.Schema != StateSchema {
			return unsupportedPayloadSchema("state", state.Schema, StateSchema)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("journal: read state.json: %w", err)
	}

	events, _, err := readEvents(filepath.Join(dir, fileEvents))
	if err != nil {
		return err
	}
	return validateEventSchemas(events)
}

func validateEventSchemas(events []Event) error {
	for _, event := range events {
		if event.Schema != EventSchema {
			return unsupportedPayloadSchema("event", event.Schema, EventSchema)
		}
	}
	return nil
}

func unsupportedPayloadSchema(kind, found, supported string) error {
	return fmt.Errorf(
		"journal: %s schema %q is unsupported (supported %q); minimum binary is a Goobers build supporting %s",
		kind, found, supported, found,
	)
}

func backupJournal(dir string, version int) error {
	backupRoot := migrationBackupRoot(dir)
	backup := migrationBackupPath(dir, version)
	backupExists := false
	if info, err := os.Stat(backup); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("journal: migration backup %s is not a directory", backup)
		}
		backupExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("journal: inspect migration backup: %w", err)
	}
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		return fmt.Errorf("journal: create migration backup directory: %w", err)
	}
	staging := backup + ".tmp"
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("journal: clear incomplete migration backup: %w", err)
	}
	if err := copyJournalTree(dir, staging); err != nil {
		return fmt.Errorf("journal: create migration backup: %w", err)
	}
	if backupExists {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("journal: replace stale migration backup: %w", err)
		}
	}
	if err := durability.Move(staging, backup); err != nil {
		return fmt.Errorf("journal: commit migration backup: %w", err)
	}
	if err := durability.SyncDir(backupRoot); err != nil {
		return fmt.Errorf("journal: sync migration backup directory: %w", err)
	}
	return nil
}

func rollbackMigration(
	dir string,
	sourceVersion int,
	migrationErr error,
	restoreBackup func(string, int) error,
) error {
	info, exists, err := readSchemaInfo(dir)
	if err != nil {
		return fmt.Errorf("%w; inspect rollback schema authority: %v", migrationErr, err)
	}
	if exists &&
		info.Version == CurrentSchemaVersion &&
		info.MinimumBinary == minimumBinaryForJournalSchema(CurrentSchemaVersion) &&
		info.Migration == nil {
		return migrationErr
	}
	if !exists ||
		info.Version != -1 ||
		info.Migration == nil ||
		info.Migration.From != sourceVersion ||
		info.Migration.To != CurrentSchemaVersion {
		return fmt.Errorf(
			"%w; rollback refused because schema.json no longer records migration %d to %d",
			migrationErr, sourceVersion, CurrentSchemaVersion,
		)
	}
	if err := restoreBackup(dir, sourceVersion); err != nil {
		return fmt.Errorf("%w; rollback failed: %v", migrationErr, err)
	}
	return migrationErr
}

func restoreJournalBackup(dir string, version int) error {
	return restoreJournalBackupWithCopy(dir, version, copyJournalEntry)
}

func restoreJournalBackupWithCopy(
	dir string,
	version int,
	copyEntry func(string, string) error,
) error {
	backup := migrationBackupPath(dir, version)
	backupInfo, backupHasSchema, err := readSchemaInfo(backup)
	if err != nil {
		return fmt.Errorf("read migration backup schema: %w", err)
	}
	if backupHasSchema != (version > 0) {
		return fmt.Errorf("migration backup schema presence does not match version %d", version)
	}
	if backupHasSchema && (backupInfo.Version != version || backupInfo.Migration != nil) {
		return fmt.Errorf("migration backup schema version is %d, want %d", backupInfo.Version, version)
	}
	if err := validateJournalPayloadSchemas(backup); err != nil {
		return fmt.Errorf("validate migration backup: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case fileLock, fileSchemaLock, fileSchema:
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}

	backupEntries, err := os.ReadDir(backup)
	if err != nil {
		return err
	}
	for _, entry := range backupEntries {
		if entry.Name() == fileSchema {
			continue
		}
		if err := copyEntry(
			filepath.Join(backup, entry.Name()),
			filepath.Join(dir, entry.Name()),
		); err != nil {
			return err
		}
	}
	if err := durability.SyncDir(dir); err != nil {
		return err
	}

	backupSchema := filepath.Join(backup, fileSchema)
	data, err := os.ReadFile(backupSchema)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Remove(filepath.Join(dir, fileSchema)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return durability.SyncDir(dir)
	}
	if err != nil {
		return err
	}
	schemaStat, err := os.Stat(backupSchema)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, fileSchema), data, schemaStat.Mode().Perm())
}

func migrationBackupRoot(dir string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(dir)), ".journal-backups")
}

func migrationBackupPath(dir string, version int) string {
	return filepath.Join(
		migrationBackupRoot(dir),
		fmt.Sprintf("%s.v%d.bak", filepath.Base(dir), version),
	)
}

func copyJournalTree(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", source)
	}
	if err := os.Mkdir(destination, info.Mode().Perm()); err != nil {
		return err
	}
	return copyJournalEntries(source, destination, true)
}

func copyJournalEntries(source, destination string, root bool) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if root && (entry.Name() == fileLock || entry.Name() == fileSchemaLock ||
			entry.Name() == fileSchema+".tmp" || entry.Name() == fileStateTemp) {
			continue
		}
		if err := copyJournalEntry(
			filepath.Join(source, entry.Name()),
			filepath.Join(destination, entry.Name()),
		); err != nil {
			return err
		}
	}
	return durability.SyncDir(destination)
}

func copyJournalEntry(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("refusing to back up symlink %s", source)
	case info.IsDir():
		if err := os.Mkdir(destination, info.Mode().Perm()); err != nil {
			return err
		}
		return copyJournalEntries(source, destination, false)
	case info.Mode().IsRegular():
		return copyJournalFile(source, destination, info.Mode().Perm())
	default:
		return fmt.Errorf("refusing to back up non-regular path %s", source)
	}
}

func copyJournalFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := syncFile(output); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}
