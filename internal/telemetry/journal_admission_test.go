package telemetry

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestJournalLogsConcurrentAdmissionDoesNotDropOnWorkerContention(t *testing.T) {
	exporter := &journalTestExporter{started: make(chan struct{}), release: make(chan struct{})}
	client := journalTestClient(t, exporter)
	client.Commit(journal.CommittedEvent{JournalID: "concurrent", Body: []byte("{}")})
	waitJournalStarted(t, exporter.started)
	// Holding the consumer state lock must neither stall nor reject producers.
	client.journalLogs.mu.Lock()
	var producers sync.WaitGroup
	for producer := range 32 {
		producers.Go(func() {
			for i := range 16 {
				client.Commit(journal.CommittedEvent{JournalID: "concurrent", Seq: uint64(producer*16 + i + 1), Body: []byte("{}")})
			}
		})
	}
	producers.Wait()
	client.journalLogs.mu.Unlock()
	close(exporter.release)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.journalLogs.flush(ctx); err != nil {
		t.Fatal(err)
	}
	stats := client.JournalExportStats()
	if stats.Accepted != 513 || stats.Dropped != 0 || stats.QueuedRecords != 0 || stats.QueuedBytes != 0 {
		t.Fatalf("admission lost records or leaked reservations: %+v", stats)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.records) != 513 {
		t.Fatalf("delivered=%d want=513", len(exporter.records))
	}
}
