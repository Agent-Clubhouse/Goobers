package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

func TestAzureReplayLargeMissingManifestDefersScanUntilReady(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "journal")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 1024 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%020d-seed.ndjson", i)), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	x := &azureReplayIndex{root: root, streams: []string{"journal"}, start: start}
	done := make(chan error, 1)
	go func() { done <- x.awaitLargeColdBacklogStart(t.Context()) }()
	select {
	case err := <-done:
		t.Fatalf("large cold scan started before readiness: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(start)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ready signal did not release cold scan")
	}
}

// Reconstruction may hold the root index lock, but a newly emitted daemon
// record must still be durably accepted on a separately bounded path.
func TestAzureReplayColdIndexLockDoesNotBlockDurableStartupAdmission(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "journal"), 0o700); err != nil {
		t.Fatal(err)
	}
	blocker, err := platformlock.TryAcquire(filepath.Join(root, ".replay-index.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Release() }()
	cfg := azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20, start: make(chan struct{})}
	s, err := newAzureReplaySpool(cfg, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.close(ctx)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := s.submit(ctx, []byte("{\"startup\":true}\n")); err != nil {
		t.Fatalf("cold index lock blocked durable startup admission: %v", err)
	}
	entries, err := os.ReadDir(bootstrapDir(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	var durable int
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != azureReplayFileSuffix {
			continue
		}
		_, payload, err := readAzureReplayFile(filepath.Join(bootstrapDir(root, "journal"), entry.Name()))
		if err != nil || string(payload) != "{\"startup\":true}\n" {
			t.Fatalf("bootstrap receipt is not durable: %q %v", payload, err)
		}
		durable++
	}
	if durable != 1 {
		t.Fatalf("bootstrap files=%d want 1", durable)
	}
}

func TestAzureReplayBootstrapSurvivesShutdownAndReplaysAfterRestart(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "journal"), 0o700); err != nil {
		t.Fatal(err)
	}
	blocker, err := platformlock.TryAcquire(filepath.Join(root, ".replay-index.lock"))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	cfg := azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20, start: start}
	first, err := newAzureReplaySpool(cfg, func(context.Context, []byte) error { return nil })
	if err != nil {
		_ = blocker.Release()
		t.Fatal(err)
	}
	payload := []byte("{\"restart\":true}\n")
	if err := first.submit(t.Context(), payload); err != nil {
		_ = blocker.Release()
		t.Fatal(err)
	}
	closeCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	if err := first.close(closeCtx); err != nil {
		cancel()
		_ = blocker.Release()
		t.Fatal(err)
	}
	cancel()
	if err := blocker.Release(); err != nil {
		t.Fatal(err)
	}
	close(start)
	delivered := make(chan []byte, 4)
	second, err := newAzureReplaySpool(cfg, func(_ context.Context, body []byte) error {
		delivered <- append([]byte(nil), body...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.close(context.Background()) }()
	select {
	case got := <-delivered:
		if string(got) != string(payload) {
			t.Fatalf("replayed bootstrap payload=%q want=%q", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap file was not migrated and delivered")
	}
}

func TestAzureReplayBootstrapBoundRejectsAndCountsOverflow(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "journal"), 0o700); err != nil {
		t.Fatal(err)
	}
	blocker, err := platformlock.TryAcquire(filepath.Join(root, ".replay-index.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Release() }()
	cfg := azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20, start: make(chan struct{})}
	s, err := newAzureReplaySpool(cfg, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.close(ctx)
	}()
	payload := []byte("{\"value\":\"" + strings.Repeat("x", 600<<10) + "\"}\n")
	if err := s.submit(t.Context(), payload); err != nil {
		t.Fatalf("first bounded bootstrap batch: %v", err)
	}
	if err := s.submit(t.Context(), payload); err == nil {
		t.Fatal("bootstrap exceeded its byte bound")
	}
	if got := s.admissionFailures.Load(); got != 1 {
		t.Fatalf("rejected bootstrap batch was not counted: %d", got)
	}
}

func TestAzureReplayBootstrapBoundSharedAcrossIndependentHandles(t *testing.T) {
	root := t.TempDir()
	payload := []byte("{\"value\":\"" + strings.Repeat("x", 600<<10) + "\"}\n")
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"first.ndjson", "second.ndjson"} {
		x := &azureReplayIndex{root: root}
		x.bootstrapOpen.Store(true)
		s := &azureReplaySpool{index: x, stream: "journal", cfg: azureReplayConfig{maxBytes: 1 << 20}}
		go func(name string) {
			<-start
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			results <- s.submitBootstrap(ctx, name, time.Now(), payload)
		}(name)
	}
	close(start)
	var admitted, rejected int
	for range 2 {
		select {
		case err := <-results:
			if err == nil {
				admitted++
			} else if strings.Contains(err.Error(), "bootstrap bound reached") {
				rejected++
			} else {
				t.Fatalf("independent bootstrap writer failed unexpectedly: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("independent bootstrap writers did not release the root lock")
		}
	}
	if admitted != 1 || rejected != 1 {
		t.Fatalf("shared bootstrap bound admitted=%d rejected=%d", admitted, rejected)
	}
	entries, err := os.ReadDir(bootstrapDir(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	var files int
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), azureReplayFileSuffix) {
			files++
		}
	}
	if files != 1 {
		t.Fatalf("independent writers published %d files despite the shared bound", files)
	}
}

func TestAzureReplayBootstrapGateClosesUnderMigrationLock(t *testing.T) {
	root := t.TempDir()
	x := &azureReplayIndex{root: root}
	x.bootstrapOpen.Store(true)
	s := &azureReplaySpool{index: x, stream: "journal", cfg: azureReplayConfig{maxBytes: 1 << 20}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	unlock, err := lockBootstrap(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.submitBootstrap(ctx, "startup.ndjson", time.Now(), []byte("{}\n")) }()
	x.bootstrapOpen.Store(false) // Migration closes the gate while holding this lock.
	unlock()
	select {
	case err := <-done:
		if !errors.Is(err, errAzureBootstrapReady) {
			t.Fatalf("late bootstrap writer did not switch to normal admission: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("late bootstrap writer did not release")
	}
	if entries, err := os.ReadDir(bootstrapDir(root, "journal")); err == nil && len(entries) != 0 {
		t.Fatalf("late bootstrap file was stranded after migration: %v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestAzureReplayBootstrapLeftByOtherProcessIsMigrated(t *testing.T) {
	root := t.TempDir()
	start := make(chan struct{})
	close(start)
	delivered := make(chan []byte, 4)
	cfg := azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20, start: start}
	s, err := newAzureReplaySpool(cfg, func(_ context.Context, body []byte) error {
		delivered <- append([]byte(nil), body...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.close(context.Background()) }()
	if err := s.index.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockBootstrap(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	dir := bootstrapDir(root, "journal")
	if err := ensureBootstrapStream(dir); err != nil {
		unlock()
		t.Fatal(err)
	}
	payload := []byte("{\"external\":true}\n")
	if _, err := writeAzureReplayBatch(dir, "external.ndjson", time.Now(), payload); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if !s.index.hasBootstrapFiles() {
		t.Fatal("cross-process bootstrap publication was invisible")
	}
	if stats := s.stats(); stats.AccountingReady {
		t.Fatalf("unindexed cross-process bootstrap batch was reported as fully accounted: %+v", stats)
	}
	if stats := InspectAzureReplayRoot(root); stats.AccountingReady {
		t.Fatalf("external replay inspection missed unindexed bootstrap batch: %+v", stats)
	}
	if err := s.index.migrateBootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stats := s.stats(); !stats.AccountingReady || stats.PendingRecords != 1 {
		t.Fatalf("migrated bootstrap batch was not accounted: %+v", stats)
	}
	s.signal()
	select {
	case got := <-delivered:
		if string(got) != string(payload) {
			t.Fatalf("cross-process replay payload=%q want=%q", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cross-process bootstrap file was not delivered")
	}
}

// A rename is durable even when a later file aborts the SQLite transaction.
// Reopening must discover the already-moved file and migrate the remainder;
// neither file may be silently omitted from the rebuilt manifest.
func TestAzureReplayBootstrapRecoversMidMigrationRollback(t *testing.T) {
	root := t.TempDir()
	dir := bootstrapDir(root, "journal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "journal"), 0o700); err != nil {
		t.Fatal(err)
	}
	firstName, secondName := "0001.ndjson", "0002.ndjson"
	firstPayload, secondPayload := []byte("{\"id\":1}\n"), []byte("{\"id\":2}\n")
	for _, batch := range []struct {
		name string
		body []byte
	}{{firstName, firstPayload}, {secondName, secondPayload}} {
		if _, err := writeAzureReplayBatch(dir, batch.name, time.Now(), batch.body); err != nil {
			t.Fatal(err)
		}
	}
	// The second destination forces a deterministic failure after the first
	// rename, before the manifest transaction can commit.
	collision := filepath.Join(root, "journal", secondName)
	if _, err := writeAzureReplayBatch(filepath.Dir(collision), secondName, time.Now(), []byte("{\"collision\":true}\n")); err != nil {
		t.Fatal(err)
	}
	x := &azureReplayIndex{root: root, streams: []string{"journal"}}
	if err := x.open(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := x.migrateBootstrap(t.Context()); err == nil {
		t.Fatal("destination collision did not abort migration")
	}
	if _, err := os.Stat(filepath.Join(root, "journal", firstName)); err != nil {
		t.Fatalf("first rename was not exercised: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, secondName)); err != nil {
		t.Fatalf("unmoved second batch was not retained: %v", err)
	}
	var indexed int
	if err := x.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM files WHERE stream='journal' AND name=?`, firstName).Scan(&indexed); err != nil || indexed != 0 {
		t.Fatalf("rolled-back move appeared committed: rows=%d err=%v", indexed, err)
	}
	if err := x.closeDatabases(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(collision); err != nil {
		t.Fatal(err)
	}
	reopened := &azureReplayIndex{root: root, streams: []string{"journal"}}
	if err := reopened.open(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.closeDatabases() }()
	if err := reopened.migrateBootstrap(t.Context()); err != nil {
		t.Fatalf("reopen failed to finish migration: %v", err)
	}
	for _, batch := range []struct {
		name string
		body []byte
	}{{firstName, firstPayload}, {secondName, secondPayload}} {
		_, payload, err := readAzureReplayFile(filepath.Join(root, "journal", batch.name))
		if err != nil || string(payload) != string(batch.body) {
			t.Fatalf("recovered %s payload=%q err=%v", batch.name, payload, err)
		}
	}
	var files, records int
	if err := reopened.db.QueryRowContext(t.Context(), `SELECT files,records FROM totals WHERE stream='journal'`).Scan(&files, &records); err != nil || files != 2 || records != 2 {
		t.Fatalf("reopened manifest files=%d records=%d err=%v", files, records, err)
	}
}

func TestAzureReplayExternalInspectionMarksBootstrapAccountingPartial(t *testing.T) {
	root := t.TempDir()
	cfg := azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20}
	s, err := newAzureReplaySpool(cfg, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.index.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stats := InspectAzureReplayRoot(root); !stats.AccountingReady {
		t.Fatalf("idle manifest was not ready: %+v", stats)
	}
	dir := bootstrapDir(root, "journal")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureBootstrapStream(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := writeAzureReplayBatch(dir, "external.ndjson", time.Now(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if stats := InspectAzureReplayRoot(root); stats.AccountingReady {
		t.Fatalf("external inspection treated a partial manifest as complete: %+v", stats)
	}
}

func TestAzureReplayUnavailableStorageStartsAndRecovers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "obstructed")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	sent := make(chan struct{}, 1)
	spool, err := newAzureReplaySpool(azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20},
		func(context.Context, []byte) error {
			select {
			case sent <- struct{}{}:
			default:
			}
			return nil
		})
	if err != nil {
		t.Fatalf("runtime storage failure must not fail startup: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = spool.close(ctx)
	})
	if stats := spool.stats(); stats.AccountingReady {
		t.Fatalf("unavailable storage reported healthy: %+v", stats)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	err = spool.submit(ctx, []byte("{}\n"))
	cancel()
	if err == nil || spool.admissionFailures.Load() != 1 {
		t.Fatal("failed storage admission was not reported")
	}
	// Repair only this fixture file. No process restart or manual exporter reset.
	if err = os.Remove(root); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for !spool.stats().AccountingReady {
		if !waitJournalCatchup(ctx, 10*time.Millisecond) {
			t.Fatal("background initialization did not recover")
		}
	}
	if err = spool.submit(ctx, []byte("{}\n")); err != nil {
		t.Fatalf("storage did not recover: %v", err)
	}
	select {
	case <-sent:
	case <-ctx.Done():
		t.Fatal("recovered spool did not deliver")
	}
}
