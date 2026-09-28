package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAzureReplayShutdownRetainsRemoteFailures(t *testing.T) {
	for _, scenario := range []string{"refused", "unauthorized", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				code := http.StatusServiceUnavailable
				if scenario == "unauthorized" {
					code = http.StatusUnauthorized
				}
				w.WriteHeader(code)
			}))
			defer server.Close()
			if scenario == "refused" {
				server.Close()
			}
			root := t.TempDir()
			client, err := New(t.Context(), Config{ServiceName: "offline-shutdown",
				AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
				AzureMonitorTraces:           true, AzureMonitorReplayRoot: root, Batch: true,
				AzureMonitorReplayMaxAge: time.Hour, AzureMonitorReplayMaxBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			defer func() { _ = client.Shutdown(ctx) }()
			_, span, err := client.StartRun(ctx, RunAttributes{Gaggle: "test", WorkflowID: "fixture", RunID: "0123456789abcdef0123456789abcdef"})
			if err != nil {
				t.Fatal(err)
			}
			span.End()
			if err = client.Shutdown(ctx); err != nil {
				t.Fatalf("remote delivery failed daemon shutdown: %v", err)
			}
			stats := InspectAzureReplayRoot(root)
			if !stats.AccountingReady || stats.PendingRecords != 1 {
				t.Fatalf("undelivered span was not retained: %+v", stats)
			}
		})
	}
}

func TestAzureReplaySpoolReplaysAfterRestartAndSkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	cfg := azureReplayConfig{dir: dir, maxAge: 72 * time.Hour, maxBytes: 1 << 20}
	unavailable := errors.New("fixture destination unavailable")
	first, err := newAzureReplaySpool(cfg, func(context.Context, []byte) error { return unavailable })
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("{\"stable\":\"record-id\"}\n")
	if err := first.submit(context.Background(), payload); err != nil {
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
	s := testAzureReplaySpool(t, dir, base)
	s.cfg.maxBytes = 1 << 20
	if err := s.submit(context.Background(), []byte("{\"record\":1}\n")); err != nil {
		t.Fatal(err)
	}
	files, err := s.filesLockedForTest()
	if err != nil || len(files) != 1 {
		t.Fatalf("initial files = %v, %v", files, err)
	}
	s.cfg.maxBytes = files[0].size + 8
	s.now = func() time.Time { return base.Add(time.Second) }
	if err := s.submit(context.Background(), []byte("{\"record\":2}\n")); err != nil {
		t.Fatal(err)
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
	traces := testAzureReplaySpool(t, filepath.Join(root, "traces"), base)
	traces.cfg.root = root
	if err := traces.submit(context.Background(), []byte("{\"stream\":\"traces\"}\n")); err != nil {
		t.Fatal(err)
	}
	files, err := traces.filesLockedForTest()
	if err != nil || len(files) != 1 {
		t.Fatalf("trace spool files = %v, %v", files, err)
	}

	journal := testAzureReplaySpool(t, filepath.Join(root, "journal"), base.Add(time.Second))
	journal.cfg.root = root
	journal.cfg.maxBytes = files[0].size + 8
	traces.cfg.maxBytes = journal.cfg.maxBytes
	if err := journal.submit(context.Background(), []byte("{\"stream\":\"journal\"}\n")); err != nil {
		t.Fatal(err)
	}
	traceFiles, _ := traces.filesLockedForTest()
	journalFiles, _ := journal.filesLockedForTest()
	if len(traceFiles) != 0 || len(journalFiles) != 1 || journal.stats().PrunedBytes != 1 {
		t.Fatalf("aggregate byte pruning: traces=%d journal=%d stats=%+v", len(traceFiles), len(journalFiles), journal.stats())
	}
}

func TestAzureReplaySpoolUsesAtomicPrivateFiles(t *testing.T) {
	dir := t.TempDir()
	s := testAzureReplaySpool(t, dir, time.Now())
	_ = s.submit(context.Background(), []byte("{\"record\":true}\n"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var spoolEntries []os.DirEntry
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), azureReplayFileSuffix) {
			spoolEntries = append(spoolEntries, entry)
		}
	}
	entries = spoolEntries
	if len(entries) != 1 {
		t.Fatalf("spool entries = %v", entries)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	// NTFS privacy is inherited from the instance-root ACL, not Unix mode bits.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
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
	if err := s.submit(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for s.retried.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
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

func testAzureReplaySpool(t testing.TB, dir string, now time.Time) *azureReplaySpool {
	t.Helper()
	s := &azureReplaySpool{
		cfg:  azureReplayConfig{dir: dir, maxAge: 72 * time.Hour, maxBytes: 1 << 20},
		send: func(context.Context, []byte) error { return errors.New("fixture destination unavailable") },
		now:  func() time.Time { return now }, wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	_ = os.MkdirAll(dir, 0o700)
	t.Cleanup(func() {
		if s.index != nil {
			_ = s.index.release(context.Background())
		}
	})
	return s
}

type azureReplayFile struct {
	path string
	size int64
}

// Inspect files directly in assertions without using the manifest under test.
func (s *azureReplaySpool) filesLockedForTest() ([]azureReplayFile, error) {
	entries, err := os.ReadDir(s.cfg.dir)
	if err != nil {
		return nil, err
	}
	var files []azureReplayFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), azureReplayFileSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		files = append(files, azureReplayFile{path: filepath.Join(s.cfg.dir, entry.Name()), size: info.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func TestAzureReplaySpoolConcurrentSubmissionsRemainWhole(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
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

func TestAzureReplayMetadataSupportsLegacyAndValidatesPayloadAtDelivery(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	header := `{"schema":"` + azureReplaySchema + `","createdAt":"` + s.now().UTC().Format(time.RFC3339Nano) + `"`
	legacy := filepath.Join(s.cfg.dir, "000-legacy.ndjson")
	if err := os.WriteFile(legacy, []byte(header+"}\n{}\n{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, count, err := readAzureReplayMetadata(legacy)
	if err != nil || count != 2 {
		t.Fatalf("legacy count=%d err=%v", count, err)
	}
	corrupt := filepath.Join(s.cfg.dir, "001-corrupt.ndjson")
	if err := os.WriteFile(corrupt, []byte(header+",\"records\":1}\nnot-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, count, err = readAzureReplayMetadata(corrupt)
	if err != nil || count != 1 {
		t.Fatalf("header-only count=%d err=%v", count, err)
	}
	if _, _, err := readAzureReplayFile(corrupt); err == nil {
		t.Fatal("corrupt payload accepted for delivery")
	}
	s.send = func(context.Context, []byte) error { return nil }
	if err := s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := s.stats(); stats.Delivered != 2 || stats.Malformed != 1 || stats.PendingRecords != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}
