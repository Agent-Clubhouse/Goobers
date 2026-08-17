package journal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/version"
)

const legacyFixtureRunID = "4bf92f3577b34da6a3ce929d0e0e4736"

func copyLegacyJournalFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), legacyFixtureRunID)
	if err := copyJournalTree(filepath.Join("testdata", "v0"), dir); err != nil {
		t.Fatalf("copy legacy journal fixture: %v", err)
	}
	return dir
}

func TestOpenReadMigratesLegacyJournalWithBackup(t *testing.T) {
	if CurrentSchemaVersion != len(journalMigrations) {
		t.Fatalf("current schema version %d != migration count %d", CurrentSchemaVersion, len(journalMigrations))
	}
	for i, migration := range journalMigrations {
		if migration.minimumBinary == "" {
			t.Fatalf("migration to schema version %d has no minimum binary", i+1)
		}
	}
	originalVersion := version.Version
	version.Version = "v99.0.0"
	defer func() { version.Version = originalVersion }()
	dir := copyLegacyJournalFixture(t)
	eventsBefore, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}

	reader, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead legacy journal: %v", err)
	}
	info := reader.Schema()
	if info.Version != CurrentSchemaVersion || info.MinimumBinary != "6c4c43c6" {
		t.Fatalf("schema = %+v, want version %d with minimum binary 6c4c43c6", info, CurrentSchemaVersion)
	}

	var persisted SchemaInfo
	schemaBytes, err := os.ReadFile(filepath.Join(dir, fileSchema))
	if err != nil {
		t.Fatalf("read migrated schema.json: %v", err)
	}
	if err := json.Unmarshal(schemaBytes, &persisted); err != nil {
		t.Fatalf("parse migrated schema.json: %v", err)
	}
	if persisted != info {
		t.Fatalf("persisted schema = %+v, reader schema = %+v", persisted, info)
	}

	backup := filepath.Join(migrationBackupRoot(dir), filepath.Base(dir)+".v0.bak")
	if _, err := os.Stat(filepath.Join(backup, fileSchema)); !os.IsNotExist(err) {
		t.Fatalf("legacy backup unexpectedly contains schema.json: %v", err)
	}
	eventsBackup, err := os.ReadFile(filepath.Join(backup, fileEvents))
	if err != nil {
		t.Fatalf("read legacy backup: %v", err)
	}
	if string(eventsBackup) != string(eventsBefore) {
		t.Fatal("migration backup does not preserve legacy events")
	}
	for _, name := range []string{fileLock, fileSchemaLock, fileSchema + ".tmp", fileStateTemp} {
		path := filepath.Join(backup, dirInputs, "reserved", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read nested reserved-name fixture %s: %v", name, err)
		}
		if string(data) != name+"\n" {
			t.Fatalf("nested reserved-name fixture %s = %q", name, data)
		}
	}

	if _, err := OpenRead(dir); err != nil {
		t.Fatalf("second OpenRead: %v", err)
	}
}

func TestOpenReadWaitsForWriterBeforeMigratingLegacyJournal(t *testing.T) {
	dir := copyLegacyJournalFixture(t)
	lock, err := acquireRunLock(dir)
	if err != nil {
		t.Fatalf("acquire writer lock: %v", err)
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			releaseRunLock(lock)
		}
	}()

	type openResult struct {
		reader *Reader
		err    error
	}
	opened := make(chan openResult, 1)
	go func() {
		reader, err := OpenRead(dir)
		opened <- openResult{reader: reader, err: err}
	}()

	select {
	case result := <-opened:
		t.Fatalf("OpenRead returned while writer lock was held: reader=%v err=%v", result.reader, result.err)
	case <-time.After(200 * time.Millisecond):
	}

	backup := filepath.Join(migrationBackupRoot(dir), filepath.Base(dir)+".v0.bak")
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("migration backup created while writer lock was held: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileSchema)); !os.IsNotExist(err) {
		t.Fatalf("schema committed while writer lock was held: %v", err)
	}

	updatedAt := time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC)
	event, err := json.Marshal(Event{
		Schema: EventSchema,
		Seq:    3,
		Type:   EventRunResumed,
		Time:   updatedAt,
		Status: string(PhaseCompleted),
		Target: "review",
	})
	if err != nil {
		t.Fatalf("marshal writer event: %v", err)
	}
	events, err := os.OpenFile(filepath.Join(dir, fileEvents), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open writer events: %v", err)
	}
	if _, err := events.Write(append(event, '\n')); err != nil {
		_ = events.Close()
		t.Fatalf("append writer event: %v", err)
	}
	if err := events.Sync(); err != nil {
		_ = events.Close()
		t.Fatalf("sync writer event: %v", err)
	}
	if err := events.Close(); err != nil {
		t.Fatalf("close writer events: %v", err)
	}
	if err := writeStateAtomic(dir, State{
		Schema:       StateSchema,
		RunID:        legacyFixtureRunID,
		Phase:        PhaseRunning,
		MachineState: "review",
		LastSeq:      3,
		UpdatedAt:    updatedAt,
	}); err != nil {
		t.Fatalf("replace writer checkpoint: %v", err)
	}

	releaseRunLock(lock)
	lockHeld = false

	select {
	case result := <-opened:
		if result.err != nil {
			t.Fatalf("OpenRead after writer release: %v", result.err)
		}
		if result.reader.Schema().Version != CurrentSchemaVersion {
			t.Fatalf("schema version = %d, want %d", result.reader.Schema().Version, CurrentSchemaVersion)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenRead did not migrate after writer released its lock")
	}

	backupEvents, _, err := readEvents(filepath.Join(backup, fileEvents))
	if err != nil {
		t.Fatalf("read backup events: %v", err)
	}
	stateData, err := os.ReadFile(filepath.Join(backup, fileState))
	if err != nil {
		t.Fatalf("read backup state: %v", err)
	}
	var backupState State
	if err := json.Unmarshal(stateData, &backupState); err != nil {
		t.Fatalf("parse backup state: %v", err)
	}
	if len(backupEvents) != 3 || backupEvents[2].Seq != 3 || backupState.LastSeq != 3 {
		t.Fatalf("backup is not the writer's final consistent state: events=%d state.lastSeq=%d", len(backupEvents), backupState.LastSeq)
	}
}

func TestOpenReadCurrentJournalDoesNotCreateMigrationLock(t *testing.T) {
	run, root := newRun(t)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, testIdentity().RunID)
	if _, err := OpenRead(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileSchemaLock)); !os.IsNotExist(err) {
		t.Fatalf("current journal open created a migration lock: %v", err)
	}
}

func TestOpenReadRejectsNewerJournalVersionBeforeBackup(t *testing.T) {
	dir := copyLegacyJournalFixture(t)
	future := SchemaInfo{Version: CurrentSchemaVersion + 1, MinimumBinary: "v2.0.0"}
	data, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileSchema), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = OpenRead(dir)
	if err == nil {
		t.Fatal("OpenRead accepted a newer journal schema")
	}
	for _, want := range []string{"version 2", "supported version 1", "minimum binary is v2.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("OpenRead error %q does not contain %q", err, want)
		}
	}
	backupRoot := migrationBackupRoot(dir)
	if _, statErr := os.Stat(backupRoot); !os.IsNotExist(statErr) {
		t.Fatalf("unsupported schema created a backup before failing: %v", statErr)
	}
}

func TestOpenReadRejectsMalformedSchemaManifestBeforeBackup(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name:     "missing version",
			manifest: `{"minimumBinary":"v1.0.0"}`,
			want:     "schema.json version is required",
		},
		{
			name:     "unknown field",
			manifest: `{"version":0,"minimumBinary":"v1.0.0","unexpected":true}`,
			want:     `unknown field "unexpected"`,
		},
		{
			name:     "explicit legacy version",
			manifest: `{"version":0,"minimumBinary":"v1.0.0"}`,
			want:     "schema version 0 is valid only for legacy journals without schema.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := copyLegacyJournalFixture(t)
			if err := os.WriteFile(filepath.Join(dir, fileSchema), []byte(tt.manifest), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := OpenRead(dir)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("OpenRead error = %v, want diagnostic containing %q", err, tt.want)
			}
			if _, statErr := os.Stat(migrationBackupRoot(dir)); !os.IsNotExist(statErr) {
				t.Fatalf("malformed manifest created a backup before failing: %v", statErr)
			}
		})
	}
}

func TestOpenReadAdmitsNewerManifestBeforePayloadLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "future")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(SchemaInfo{
		Version:       CurrentSchemaVersion + 1,
		MinimumBinary: "v2.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileSchema), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = OpenRead(dir)
	if err == nil {
		t.Fatal("OpenRead accepted a newer manifest without run.yaml")
	}
	for _, want := range []string{"version 2", "supported version 1", "minimum binary is v2.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("OpenRead error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "run.yaml") {
		t.Fatalf("OpenRead inspected the current payload layout before rejecting the manifest: %v", err)
	}
}

func TestOpenReadIdentifiesUnrelatedDirectory(t *testing.T) {
	_, err := OpenRead(t.TempDir())
	if !errors.Is(err, ErrNotRunDirectory) {
		t.Fatalf("OpenRead error = %v, want ErrNotRunDirectory", err)
	}
}

func TestOpenReadRollsBackInterruptedMigrationBeforeRetry(t *testing.T) {
	dir := copyLegacyJournalFixture(t)
	runBefore, err := os.ReadFile(filepath.Join(dir, fileRunYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileSchema+".tmp"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	backupStaging := filepath.Join(
		migrationBackupRoot(dir), filepath.Base(dir)+".v0.bak.tmp",
	)
	if err := os.MkdirAll(backupStaging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupStaging, "partial"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backupJournal(dir, 0); err != nil {
		t.Fatalf("create pre-migration backup: %v", err)
	}
	if err := writeSchemaInfo(dir, SchemaInfo{
		Version:       -1,
		MinimumBinary: "v1.0.0",
		Migration:     &SchemaMigration{From: 0, To: CurrentSchemaVersion},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte("partially migrated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "partial-output"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead after interrupted migration: %v", err)
	}
	if reader.Schema().Version != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", reader.Schema().Version, CurrentSchemaVersion)
	}
	runAfter, err := os.ReadFile(filepath.Join(dir, fileRunYAML))
	if err != nil {
		t.Fatal(err)
	}
	if string(runAfter) != string(runBefore) {
		t.Fatal("interrupted migration payload was not restored before retry")
	}
	if _, err := os.Stat(filepath.Join(dir, "partial-output")); !os.IsNotExist(err) {
		t.Fatalf("partial migration output survives rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileSchema+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("stale schema temp survives migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupStaging, "partial")); !os.IsNotExist(err) {
		t.Fatalf("stale backup staging content survives migration: %v", err)
	}
}

func TestOpenReadRollsBackFailedMigration(t *testing.T) {
	dir := copyLegacyJournalFixture(t)
	runBefore, err := os.ReadFile(filepath.Join(dir, fileRunYAML))
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, dirInputs, "reserved", fileLock)
	nestedBefore, err := os.ReadFile(nested)
	if err != nil {
		t.Fatal(err)
	}

	original := journalMigrations[0].apply
	defer func() { journalMigrations[0].apply = original }()
	failingMigration := func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte("partial"), 0o644); err != nil {
			return err
		}
		if err := os.Remove(nested); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "partial-output"), []byte("partial"), 0o644); err != nil {
			return err
		}
		return errors.New("injected migration failure")
	}
	journalMigrations[0].apply = failingMigration
	_, err = OpenRead(dir)
	journalMigrations[0].apply = original
	if err == nil || !strings.Contains(err.Error(), "injected migration failure") {
		t.Fatalf("OpenRead error = %v, want injected failure", err)
	}

	runAfter, err := os.ReadFile(filepath.Join(dir, fileRunYAML))
	if err != nil {
		t.Fatal(err)
	}
	if string(runAfter) != string(runBefore) {
		t.Fatal("failed migration did not restore run.yaml")
	}
	nestedAfter, err := os.ReadFile(nested)
	if err != nil {
		t.Fatal(err)
	}
	if string(nestedAfter) != string(nestedBefore) {
		t.Fatal("failed migration did not restore nested input")
	}
	if _, err := os.Stat(filepath.Join(dir, "partial-output")); !os.IsNotExist(err) {
		t.Fatalf("partial migration output survives rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileSchema)); !os.IsNotExist(err) {
		t.Fatalf("legacy schema authority was not restored: %v", err)
	}

	event, err := json.Marshal(Event{
		Schema: EventSchema,
		Seq:    3,
		Type:   EventRunResumed,
		Time:   fixedClock()(),
		Status: string(PhaseRunning),
		Target: "review",
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := os.OpenFile(filepath.Join(dir, fileEvents), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Write(append(event, '\n')); err != nil {
		_ = events.Close()
		t.Fatal(err)
	}
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}
	eventsAfterAppend, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}

	journalMigrations[0].apply = failingMigration
	_, err = OpenRead(dir)
	journalMigrations[0].apply = original
	if err == nil || !strings.Contains(err.Error(), "injected migration failure") {
		t.Fatalf("second OpenRead error = %v, want injected failure", err)
	}
	eventsAfterRetry, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	if string(eventsAfterRetry) != string(eventsAfterAppend) {
		t.Fatal("second rollback restored a stale backup over newer journal events")
	}

	if _, err := OpenRead(dir); err != nil {
		t.Fatalf("OpenRead after rollback: %v", err)
	}
}

func TestFinalManifestPostRenameFailureDoesNotRollback(t *testing.T) {
	dir := copyLegacyJournalFixture(t)
	originalApply := journalMigrations[0].apply
	defer func() { journalMigrations[0].apply = originalApply }()
	journalMigrations[0].apply = func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "migrated-output"), []byte("complete"), 0o644)
	}

	postRenameErr := errors.New("injected parent directory sync failure")
	writes := 0
	writeSchema := func(dir string, info SchemaInfo) error {
		writes++
		if writes != 2 {
			return writeSchemaInfo(dir, info)
		}
		return writeSchemaInfoWithWriter(dir, info, func(path string, data []byte, mode os.FileMode) error {
			tmp := path + ".tmp"
			if err := writeFileSynced(tmp, data, mode); err != nil {
				return err
			}
			if err := durability.ReplaceFile(tmp, path); err != nil {
				return err
			}
			return postRenameErr
		})
	}
	restoreCalls := 0
	_, err := ensureJournalSchemaWithOperations(
		dir,
		writeSchema,
		func(dir string, version int) error {
			restoreCalls++
			return restoreJournalBackup(dir, version)
		},
	)
	if !errors.Is(err, postRenameErr) {
		t.Fatalf("migration error = %v, want post-rename failure", err)
	}
	if restoreCalls != 0 {
		t.Fatalf("post-rename failure started %d rollback(s), want none", restoreCalls)
	}
	info, exists, err := readSchemaInfo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || info.Version != CurrentSchemaVersion || info.Migration != nil {
		t.Fatalf("schema authority = %+v, exists=%t, want stable current schema", info, exists)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "migrated-output")); err != nil || string(data) != "complete" {
		t.Fatalf("completed migration payload = %q, %v", data, err)
	}

	journalMigrations[0].apply = originalApply
	if _, err := OpenRead(dir); err != nil {
		t.Fatalf("OpenRead after post-rename failure: %v", err)
	}
}

func TestInterruptedRollbackKeepsMigrationMarkerAuthoritative(t *testing.T) {
	dir := copyLegacyJournalFixture(t)
	originalApply := journalMigrations[0].apply
	defer func() { journalMigrations[0].apply = originalApply }()
	migrationErr := errors.New("injected migration failure")
	journalMigrations[0].apply = func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte("partial"), 0o644); err != nil {
			return err
		}
		return migrationErr
	}
	restoreErr := errors.New("injected rollback copy failure")
	_, err := ensureJournalSchemaWithOperations(
		dir,
		writeSchemaInfo,
		func(dir string, version int) error {
			return restoreJournalBackupWithCopy(dir, version, func(string, string) error {
				return restoreErr
			})
		},
	)
	if !errors.Is(err, migrationErr) ||
		!strings.Contains(err.Error(), restoreErr.Error()) {
		t.Fatalf("migration error = %v, want migration and rollback failures", err)
	}
	info, exists, err := readSchemaInfo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !exists ||
		info.Version != -1 ||
		info.Migration == nil ||
		info.Migration.From != 0 ||
		info.Migration.To != CurrentSchemaVersion {
		t.Fatalf("schema authority after interrupted rollback = %+v, exists=%t", info, exists)
	}
	if _, err := os.Stat(filepath.Join(dir, fileRunYAML)); !os.IsNotExist(err) {
		t.Fatalf("rollback interruption did not expose partial restoration: %v", err)
	}

	journalMigrations[0].apply = originalApply
	reader, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead did not recover interrupted rollback: %v", err)
	}
	if reader.Schema().Version != CurrentSchemaVersion {
		t.Fatalf("recovered schema version = %d, want %d", reader.Schema().Version, CurrentSchemaVersion)
	}
	if identity, err := reader.Identity(); err != nil || identity.RunID != legacyFixtureRunID {
		t.Fatalf("recovered identity = %+v, %v", identity, err)
	}
}

func TestRecoverRevalidatesSchemaAfterWriterLock(t *testing.T) {
	run, root := newRun(t)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, testIdentity().RunID)
	eventsBefore, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireRunLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			releaseRunLock(lock)
		}
	}()

	recovered := make(chan error, 1)
	go func() {
		r, _, err := Recover(dir)
		if r != nil {
			_ = r.Close()
		}
		recovered <- err
	}()
	select {
	case err := <-recovered:
		t.Fatalf("Recover returned before writer released its lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := writeSchemaInfo(dir, SchemaInfo{
		Version:       CurrentSchemaVersion + 1,
		MinimumBinary: "v2.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	releaseRunLock(lock)
	lockHeld = false

	select {
	case err := <-recovered:
		if err == nil || !strings.Contains(err.Error(), "newer than supported") {
			t.Fatalf("Recover error = %v, want unsupported schema diagnostic", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recover did not return after writer released its lock")
	}
	eventsAfter, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	if string(eventsAfter) != string(eventsBefore) {
		t.Fatal("Recover mutated events after schema changed while waiting")
	}
}

func TestJournalProtectionMigratesBeforeTakingWriterLock(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(string) error
	}{
		{
			name: "ingest",
			run: func(dir string) error {
				return WithPruneProtection(dir, func() error { return nil })
			},
		},
		{
			name: "retention",
			run: func(dir string) error {
				reserved, err := ReserveTerminalForPrune(dir)
				if err == nil && !reserved {
					return errors.New("terminal legacy run was not reserved")
				}
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := copyLegacyJournalFixture(t)
			done := make(chan error, 1)
			go func() { done <- test.run(dir) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("operation deadlocked while migrating a legacy journal")
			}
		})
	}
}

func TestReserveTerminalForPruneRejectsFutureSchemaWithoutMutation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "future")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(SchemaInfo{
		Version:       CurrentSchemaVersion + 1,
		MinimumBinary: "v2.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileSchema), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ReserveTerminalForPrune(dir); err == nil {
		t.Fatal("retention accepted a newer journal schema")
	}
	for _, name := range []string{fileLock, fileSchemaLock, filePruning} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("retention created %s before rejecting the schema: %v", name, err)
		}
	}
}
