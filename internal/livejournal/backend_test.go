package livejournal

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type recordingJournalBackend struct {
	journal.Backend
	creates int
	appends int
}

func (b *recordingJournalBackend) CreateRun(dir string, id journal.RunIdentity, inputs map[string][]byte, opts ...journal.Option) (*journal.Run, journal.OwnerToken, error) {
	b.creates++
	return b.Backend.CreateRun(dir, id, inputs, opts...)
}

func (b *recordingJournalBackend) AppendBatch(run *journal.Run, owner journal.OwnerToken, events []journal.Event) error {
	b.appends += len(events)
	return b.Backend.AppendBatch(run, owner, events)
}

func TestWriterUsesJournalBackend(t *testing.T) {
	backend := &recordingJournalBackend{Backend: journal.NewFileBackend()}
	writer, _ := testWriter(t, WithBackend(backend))
	request := openBatch("run-backend", time.Now())
	if _, err := writer.Emit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if backend.creates != 1 || backend.appends != 1 {
		t.Fatalf("backend calls: creates=%d appends=%d", backend.creates, backend.appends)
	}
}
