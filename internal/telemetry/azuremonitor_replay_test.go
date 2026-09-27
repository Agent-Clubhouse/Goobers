package telemetry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAzureReplaySpoolReplaysAfterRestartAndSkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	cfg := azureReplayConfig{dir: dir, maxAge: 72 * time.Hour, maxBytes: 1 << 20}
	unavailable := errors.New("fixture destination unavailable")
	first, err := newAzureReplaySpool(cfg, func(context.Context, []byte) error { return unavailable })
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("{\"stable\":\"record-id\"}\n")
	if err := first.submit(context.Background(), payload); !errors.Is(err, unavailable) {
		t.Fatalf("submit error = %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := first.close(closeCtx); !errors.Is(err, unavailable) {
		t.Fatalf("close error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "00000000000000000000-poison.ndjson"), []byte("not a replay file"), 0o600); err != nil {
		t.Fatal(err)
	}

	delivered := make(chan []byte, 1)
	second, err := newAzureReplaySpool(cfg, func(_ context.Context, body []byte) error {
		delivered <- append([]byte(nil), body...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-delivered:
		if string(got) != string(payload) {
			t.Fatalf("replayed payload = %q, want %q", got, payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending batch was not replayed after restart")
	}
	closeCtx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := second.close(closeCtx); err != nil {
		t.Fatal(err)
	}
	stats := second.stats()
	if stats.Malformed != 1 || stats.Delivered != 1 || stats.PendingRecords != 0 {
		t.Fatalf("replay stats = %+v", stats)
	}
}

func TestAzureReplaySpoolAgeAndByteBounds(t *testing.T) {
	base := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s := testAzureReplaySpool(dir, base)
	s.cfg.maxBytes = 1 << 20
	if err := s.submit(context.Background(), []byte("{\"record\":1}\n")); err == nil {
		t.Fatal("fixture send unexpectedly succeeded")
	}
	files, err := s.filesLockedForTest()
	if err != nil || len(files) != 1 {
		t.Fatalf("initial files = %v, %v", files, err)
	}
	s.cfg.maxBytes = files[0].size + 8
	s.now = func() time.Time { return base.Add(time.Second) }
	if err := s.submit(context.Background(), []byte("{\"record\":2}\n")); err == nil {
		t.Fatal("fixture send unexpectedly succeeded")
	}
	stats := s.stats()
	if stats.PrunedBytes != 1 || stats.PendingRecords != 1 {
		t.Fatalf("byte-bound stats = %+v", stats)
	}

	s.now = func() time.Time { return base.Add(73 * time.Hour) }
	if err := s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats = s.stats()
	if stats.PrunedAge != 1 || stats.PendingRecords != 0 {
		t.Fatalf("age-bound stats = %+v", stats)
	}
}

func TestAzureReplayByteBoundSpansAllSignalDirectories(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	traces := testAzureReplaySpool(filepath.Join(root, "traces"), base)
	traces.cfg.root = root
	if err := traces.submit(context.Background(), []byte("{\"stream\":\"traces\"}\n")); err == nil {
		t.Fatal("fixture send unexpectedly succeeded")
	}
	files, err := traces.filesLockedForTest()
	if err != nil || len(files) != 1 {
		t.Fatalf("trace spool files = %v, %v", files, err)
	}

	journal := testAzureReplaySpool(filepath.Join(root, "journal"), base.Add(time.Second))
	journal.cfg.root = root
	journal.cfg.maxBytes = files[0].size + 8
	traces.cfg.maxBytes = journal.cfg.maxBytes
	if err := journal.submit(context.Background(), []byte("{\"stream\":\"journal\"}\n")); err == nil {
		t.Fatal("fixture send unexpectedly succeeded")
	}
	traceFiles, _ := traces.filesLockedForTest()
	journalFiles, _ := journal.filesLockedForTest()
	if len(traceFiles) != 0 || len(journalFiles) != 1 || journal.stats().PrunedBytes != 1 {
		t.Fatalf("aggregate byte pruning: traces=%d journal=%d stats=%+v", len(traceFiles), len(journalFiles), journal.stats())
	}
}

func TestAzureReplaySpoolUsesAtomicPrivateFiles(t *testing.T) {
	dir := t.TempDir()
	s := testAzureReplaySpool(dir, time.Now())
	_ = s.submit(context.Background(), []byte("{\"record\":true}\n"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), azureReplayFileSuffix) {
		t.Fatalf("spool entries = %v", entries)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("spool file mode = %o, want private", info.Mode().Perm())
	}
}

func TestAzureReplayStatsCountRecordsRatherThanBatchFiles(t *testing.T) {
	root := t.TempDir()
	s, err := newAzureReplaySpool(
		azureReplayConfig{dir: filepath.Join(root, "journal"), maxAge: 72 * time.Hour, maxBytes: 1 << 20},
		func(context.Context, []byte) error { return errors.New("fixture destination unavailable") },
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.close(ctx)
	}()
	payload := []byte("{\"record\":1}\n{\"record\":2}\n")
	if err := s.submit(context.Background(), payload); err == nil {
		t.Fatal("fixture send unexpectedly succeeded")
	}
	stats := s.stats()
	if stats.Accepted != 2 || stats.PendingRecords != 2 || stats.Retried == 0 {
		t.Fatalf("record-level replay stats = %+v", stats)
	}
	inspected := InspectAzureReplayRoot(root)
	if inspected.PendingRecords != 2 || inspected.Accepted != 2 || inspected.Retried == 0 {
		t.Fatalf("aggregate replay inspection = %+v", inspected)
	}
}

func testAzureReplaySpool(dir string, now time.Time) *azureReplaySpool {
	s := &azureReplaySpool{
		cfg:  azureReplayConfig{dir: dir, maxAge: 72 * time.Hour, maxBytes: 1 << 20},
		send: func(context.Context, []byte) error { return errors.New("fixture destination unavailable") },
		now:  func() time.Time { return now }, wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	_ = os.MkdirAll(dir, 0o700)
	return s
}

func (s *azureReplaySpool) filesLockedForTest() ([]azureReplayFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.filesLocked()
}

func TestAzureReplaySpoolConcurrentSubmissionsRemainWhole(t *testing.T) {
	s := testAzureReplaySpool(t.TempDir(), time.Now())
	const submissions = 20
	var wg sync.WaitGroup
	for range submissions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.submit(context.Background(), []byte("{\"record\":true}\n"))
		}()
	}
	wg.Wait()
	stats := s.stats()
	if stats.PendingRecords != submissions || stats.Malformed != 0 {
		t.Fatalf("concurrent replay stats = %+v", stats)
	}
}
