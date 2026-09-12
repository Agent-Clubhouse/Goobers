package livejournal

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestEmitRefusesDifferentInstanceReservation(t *testing.T) {
	for _, ids := range []struct{ name, original, incoming string }{
		{"different", strings.Repeat("a", 32), strings.Repeat("b", 32)},
		{"dropped", strings.Repeat("a", 32), ""},
		{"legacy-not-upgraded", "", strings.Repeat("b", 32)},
	} {
		t.Run(ids.name, func(t *testing.T) {
			for _, reopen := range []bool{false, true} {
				t.Run(map[bool]string{false: "open", true: "reopened"}[reopen], func(t *testing.T) {
					writer, runsDir := testWriter(t)
					batch := openBatch("instance-reservation", time.Now())
					batch.Open.Identity.InstanceID = ids.original
					if _, err := writer.Emit(context.Background(), batch); err != nil {
						t.Fatal(err)
					}
					if reopen {
						writer.Close()
					}
					batch.Open.Identity.InstanceID = ids.incoming
					batch.Ops = []Op{appendOp("foreign-instance-op", time.Now(), journal.Event{Type: journal.EventRunnerAnnotation})}
					if _, err := writer.Emit(context.Background(), batch); err == nil || !strings.Contains(err.Error(), "instance identity") {
						t.Fatalf("foreign reservation error = %v, want instance identity mismatch", err)
					}
					if events := readEvents(t, runsDir, batch.RunID); len(events) != 2 {
						t.Fatalf("foreign reservation appended events: %+v", events)
					}
					batch.Open.Identity.InstanceID = ids.original
					if _, err := writer.Emit(context.Background(), batch); err != nil {
						t.Fatalf("matching instance could not continue: %v", err)
					}
				})
			}
		})
	}
}

func TestEmitConflictingTerminalReservationDoesNotRetainRun(t *testing.T) {
	writer, runsDir := testWriter(t)
	batch := openBatch("terminal-reservation", time.Now())
	batch.Open.Identity.InstanceID = strings.Repeat("a", 32)
	batch.Ops = append(batch.Ops, appendOp("finish", time.Now(), journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
	}))
	if _, err := writer.Emit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	// Even entirely duplicate operations cannot adopt a conflicting header.
	batch.Ops = batch.Ops[2:]
	batch.Open.Identity.InstanceID = strings.Repeat("b", 32)
	if _, err := writer.Emit(context.Background(), batch); err == nil || !strings.Contains(err.Error(), "instance identity") {
		t.Fatalf("foreign terminal reservation error = %v", err)
	}
	if writer.IsOpen(batch.RunID) {
		t.Fatal("refusal retained a terminal run in the unbounded live map")
	}
	if events := readEvents(t, runsDir, batch.RunID); len(events) != 3 {
		t.Fatalf("foreign terminal reservation changed journal: %+v", events)
	}
	batch.Open.Identity.InstanceID = strings.Repeat("a", 32)
	response, err := writer.Emit(context.Background(), batch)
	if err != nil || !response.Terminal || response.Applied != 0 || response.Deduplicated != 1 {
		t.Fatalf("matching terminal replay = %+v, %v", response, err)
	}
}
