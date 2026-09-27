package livejournal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type committedSink struct{ events chan journal.CommittedEvent }

func (s committedSink) Commit(event journal.CommittedEvent) {
	select {
	case s.events <- event:
	default:
		panic("test committed-event queue is full")
	}
}

func TestCommittedEventsCoverLiveRecoveryAndAdoptionOnce(t *testing.T) {
	w, runsDir := testWriter(t)
	sink := committedSink{events: make(chan journal.CommittedEvent, 20)}
	stop, err := journal.RegisterCommittedEventSink(filepath.Dir(runsDir), "", sink)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	at := time.Now().UTC()
	batch := openBatch("run-live-export", at)
	if _, err := w.Emit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Emit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 {
		t.Fatalf("initial and duplicate batch exported %d records, want 2", len(sink.events))
	}
	w.CloseIdle(0)
	if _, err := w.Emit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 {
		t.Fatal("recovering and deduplicating live journal re-exported history")
	}
	w.CloseIdle(0)

	jr := runnerHandle(t, runsDir, "run-adopt-export")
	release, err := w.Adopt("run-adopt-export", "web", jr)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	artifact := EmitRequest{RunID: "run-adopt-export", Gaggle: "web",
		Ops: []Op{stageArtifactOp("one-artifact")}}
	for range 2 {
		if _, err := w.Emit(context.Background(), artifact); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.events) != 4 {
		t.Fatalf("adopted handle should export started and artifact once, got %d total", len(sink.events))
	}
	for _, want := range []struct {
		run string
		seq uint64
	}{
		{"run-live-export", 1}, {"run-live-export", 2},
		{"run-adopt-export", 1}, {"run-adopt-export", 2},
	} {
		event := <-sink.events
		if event.RunID != want.run || event.JournalID != want.run || event.Seq != want.seq {
			t.Fatalf("wrong committed event: %+v", event)
		}
	}
}
