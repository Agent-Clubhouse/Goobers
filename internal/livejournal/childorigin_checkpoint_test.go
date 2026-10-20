package livejournal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Regression: a durable stage-start append followed by a failed checkpoint
// must retain its identity on the same writer, a borrowed handle, and reopen.
func TestChildOriginCheckpointRetry(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		for _, continuation := range []bool{false, true} {
			for _, reopen := range []bool{false, true} {
				t.Run(fmt.Sprintf("adopted=%v/continuation=%v/reopen=%v", adopted, continuation, reopen), func(t *testing.T) {
					testChildOriginCheckpointRetry(t, adopted, continuation, reopen)
				})
			}
		}
	}
}

func testChildOriginCheckpointRetry(t *testing.T, adopted, continuation, reopen bool) {
	w, dir := testWriter(t)
	batch := openBatch("run-checkpoint-origin", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	start := batch.Ops[1]
	batch.Ops = batch.Ops[:1]
	if _, err := w.Emit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	start.ChildStageStart = &ChildStageStart{}
	req := EmitRequest{RunID: batch.RunID, Gaggle: batch.Gaggle, Ops: []Op{start}}
	wantStarts := 1
	if continuation {
		if _, err := w.Emit(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		start.Key += "-continuation"
		start.Event.Attempt++
		start.ChildStageStart.Continuation = true
		req.Ops = []Op{start}
		wantStarts++
	}
	var borrowed *journal.Run
	var release func()
	if adopted {
		w.CloseIdle(0)
		var err error
		borrowed, _, err = journal.Recover(filepath.Join(dir, batch.RunID))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = borrowed.Close() })
		release, err = w.Adopt(batch.RunID, batch.Gaggle, borrowed)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	tmp := filepath.Join(dir, batch.RunID, "state.json.tmp")
	if err := os.Mkdir(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	if response, err := w.Emit(context.Background(), req); err == nil || len(response.ChildOrigins) != 0 {
		t.Fatalf("failed checkpoint returned receipt: %+v %v", response, err)
	}
	events := readEvents(t, dir, batch.RunID)
	first := events[len(events)-1]
	if first.Type != journal.EventStageStarted {
		t.Fatal("probe did not reach durable append")
	}
	if !adopted && w.IsOpen(batch.RunID) {
		t.Fatal("failed owned writer remained cached")
	}
	if reopen {
		if adopted {
			release()
			if err := borrowed.Close(); err != nil {
				t.Fatal(err)
			}
		}
		w.CloseIdle(0)
	}
	// Even a recovered durable event cannot confer a receipt while the
	// checkpoint failure persists. This retry must not append again either.
	if response, err := w.Emit(context.Background(), req); err == nil || len(response.ChildOrigins) != 0 {
		t.Fatalf("uncertain retry returned receipt: %+v %v", response, err)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	retried, err := w.Emit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := journal.ChildWorkflowOriginForEvent(batch.RunID, first)
	if err != nil || len(retried.ChildOrigins) != 1 || retried.ChildOrigins[0].Origin != *origin {
		t.Fatalf("recovered origin changed: %+v %v", retried, err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(dir, batch.RunID))
	if err != nil {
		t.Fatal(err)
	}
	state, err := reader.State()
	if err != nil || state.LastSeq < first.Seq {
		t.Fatalf("receipt preceded checkpoint recovery: %+v %v", state, err)
	}
	events = readEvents(t, dir, batch.RunID)
	count := 0
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			count++
		}
	}
	if count != wantStarts || len(retried.ChildOrigins) != 1 || retried.ChildOrigins[0].Seq != first.Seq || retried.Deduplicated != 1 || retried.Applied != 0 {
		t.Fatalf("same emit key duplicated committed origin: starts=%d originalSeq=%d retry=%+v", count, first.Seq, retried)
	}
}
