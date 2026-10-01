package telemetry

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// journalRecordIDReceiver counts each journal record ID an Azure Monitor
// ingestion endpoint receives, so a test can tell a duplicate send apart from
// a second record.
type journalRecordIDReceiver struct {
	mu  sync.Mutex
	ids map[string]int
}

func newJournalRecordIDServer(t *testing.T) (*journalRecordIDReceiver, *httptest.Server) {
	t.Helper()
	receiver := &journalRecordIDReceiver{ids: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, line := range bytesLines(body) {
			var envelope struct {
				Data struct {
					BaseData struct {
						Properties map[string]string `json:"properties"`
					} `json:"baseData"`
				} `json:"data"`
			}
			if len(line) == 0 || json.Unmarshal(line, &envelope) != nil {
				continue
			}
			properties := envelope.Data.BaseData.Properties
			if properties["goobers.journal.kind"] != "run" {
				continue
			}
			receiver.mu.Lock()
			receiver.ids[properties["goobers.telemetry.record_id"]]++
			receiver.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return receiver, server
}

// #6058: a single-gaggle instance keeps root/runs as a compatibility alias of
// gaggles/<gaggle>/runs. Journal catch-up keyed its cursor by path, so it
// exported every run journal once through each path and Log Analytics held
// two copies of every run record, both from the same process.
func TestJournalCatchupLegacyRunsAliasExportsEachRecordOnce(t *testing.T) {
	root := t.TempDir()
	scoped := filepath.Join(root, "gaggles", "production", "runs")
	if err := os.MkdirAll(scoped, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("gaggles", "production", "runs"), filepath.Join(root, "runs")); err != nil {
		t.Skipf("cannot create the runs compatibility alias here: %v", err)
	}
	receiver, server := newJournalRecordIDServer(t)
	client, err := New(context.Background(), Config{
		AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
		AzureMonitorHTTPClient:       server.Client(), AzureMonitorJournalLogs: true, JournalLogs: true, JournalLogsOnly: true,
		AzureMonitorReplayRoot:   filepath.Join(root, "telemetry-export", "azure-monitor"),
		AzureMonitorReplayMaxAge: time.Hour, AzureMonitorReplayMaxBytes: 32 << 20,
		JournalRoot: root, JournalInstanceID: "instance",
	})
	if err != nil {
		t.Fatal(err)
	}
	const runID = "0123456789abcdef0123456789abcdef"
	run, err := journal.Create(scoped, journal.RunIdentity{RunID: runID, Gaggle: "production", Workflow: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err = run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	stats := client.JournalExportStats()
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	// Create's run_started record plus the five appended stage records.
	if len(receiver.ids) != 6 {
		t.Fatalf("received %d distinct run records, want 6", len(receiver.ids))
	}
	for id, count := range receiver.ids {
		if count != 1 {
			t.Errorf("record %s sent %d times", id, count)
		}
	}
	if stats.Accepted != 6 || stats.ExportFailures != 0 || stats.Dropped != 0 {
		t.Fatalf("stats=%+v, want 6 accepted and no failures or drops", stats)
	}
}

func TestCanonicalJournalDirKeepsRealRunsDirectory(t *testing.T) {
	path := t.TempDir()
	for _, dir := range []string{filepath.Join(path, "runs"), filepath.Join(path, "gaggles", "production", "runs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, dir := range []string{
		filepath.Join("runs", "0123456789abcdef0123456789abcdef"),
		filepath.Join("gaggles", "production", "runs", "0123456789abcdef0123456789abcdef"),
		"scheduler",
	} {
		if got := canonicalJournalDir(root, dir); got != dir {
			t.Errorf("canonicalJournalDir(%q) = %q, want it unchanged", dir, got)
		}
	}
}

func journalCatchupStageFixture(t *testing.T, exporter *journalTestExporter) (*journalCatchup, *os.Root, *sql.DB) {
	t.Helper()
	rootPath, spool := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	since := time.Now().Add(-time.Hour)
	log, _, err := journal.OpenInstanceLog(filepath.Join(rootPath, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err = log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	if err = log.Close(); err != nil {
		t.Fatal(err)
	}
	client := journalTestClient(t, exporter)
	db, _, err := openJournalCursorStore(t.Context(), spool, since)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	source := &journalCatchup{pipeline: client.journalLogs, root: rootPath, spool: spool, since: since,
		maxAge: time.Hour, hints: make(chan journalCatchupHint, 10)}
	return source, root, db
}

// #6058: every short-lived stage CLI reported "journal catch-up unavailable"
// and exportFailures=1. Shutdown cancels the source while discovery still has a
// batch in flight; the cancellation surfaced as a batch error and was counted
// as an export failure, turning the shutdown health record into a loss warning.
func TestJournalCatchupShutdownCancellationIsNotAnExportFailure(t *testing.T) {
	source, root, db := journalCatchupStageFixture(t, &journalTestExporter{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	source.process(ctx, root, db, journalCatchupHint{dir: "scheduler"})
	if got := source.pipeline.failures.Load(); got != 0 {
		t.Fatalf("shutdown cancellation counted %d export failures", got)
	}
	if len(source.hints) != 0 {
		t.Fatal("a stopping source re-offered its interrupted batch")
	}
	cursor, err := loadJournalCursor(t.Context(), db, "scheduler")
	if err != nil || cursor.seq != 0 {
		t.Fatalf("interrupted batch advanced the cursor: %+v %v", cursor, err)
	}
}

// The differential half: the same batch failing while the source is running is
// a real failure and stays counted.
func TestJournalCatchupRunningBatchFailureIsStillCounted(t *testing.T) {
	source, root, db := journalCatchupStageFixture(t, &journalTestExporter{exportErr: errors.New("collector unavailable")})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		// process paces its retry; stop it once the failure is recorded.
		for source.pipeline.failures.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	source.process(ctx, root, db, journalCatchupHint{dir: "scheduler"})
	// One failed export call plus the catch-up batch that was not admitted.
	if got := source.pipeline.failures.Load(); got != 2 {
		t.Fatalf("running batch failure counted %d times, want 2", got)
	}
}

// #6058: a stage CLI shutting down while the daemon uploads the shared spool's
// remaining batches logged "journal logs shutdown: azure monitor replay batches
// are leased". Those batches are durable and owned by a live upload, so this
// process has nothing left to send and its shutdown is not a failure.
func TestAzureReplayCloseWithBatchesLeasedElsewhereIsClean(t *testing.T) {
	root := t.TempDir()
	cfg := azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	owner, err := newAzureReplaySpool(cfg, func(ctx context.Context, _ []byte) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.submit(t.Context(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("owner never claimed its batch")
	}
	stage, err := newAzureReplaySpool(cfg, func(context.Context, []byte) error {
		t.Error("stage sent a batch another upload owns")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = stage.close(ctx); err != nil {
		t.Fatalf("closing beside a live upload: %v", err)
	}
	close(release)
	if err = owner.close(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := InspectAzureReplayRoot(root); stats.PendingRecords != 0 {
		t.Fatalf("owner's batch was not delivered: %+v", stats)
	}
}
