package livejournal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type recordingJournalBackend struct {
	journal.Backend
	creates int
	appends int
}

func (b *recordingJournalBackend) CreateRun(ctx context.Context, request journal.CreateRunRequest) (journal.RunHandle, journal.OwnerToken, error) {
	b.creates++
	return b.Backend.CreateRun(ctx, request)
}

func (b *recordingJournalBackend) AppendBatch(ctx context.Context, run journal.RunHandle, owner journal.OwnerToken, events []journal.Event) (int, error) {
	b.appends += len(events)
	return b.Backend.AppendBatch(ctx, run, owner, events)
}

func TestWriterUsesJournalBackend(t *testing.T) {
	runsDir := filepath.Join(t.TempDir(), "runs")
	resolver := func(gaggle string) (string, bool) { return runsDir, gaggle == "web" }
	backend := &recordingJournalBackend{Backend: journal.NewFileBackend(journal.FileBackendConfig{ResolveRunsDir: resolver})}
	writer, err := NewWriter(resolver, WithBackend(backend))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	request := openBatch("run-backend", time.Now())
	if _, err := writer.Emit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if backend.creates != 1 || backend.appends != 1 {
		t.Fatalf("backend calls: creates=%d appends=%d", backend.creates, backend.appends)
	}
}
