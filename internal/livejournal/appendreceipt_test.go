package livejournal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestAppendReceiptCheckpointRetry(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		for _, reopen := range []bool{false, true} {
			t.Run(fmt.Sprintf("adopted=%v/reopen=%v", adopted, reopen), func(t *testing.T) { testAppendReceiptCheckpointRetry(t, adopted, reopen) })
		}
	}
}

func TestAppendReceiptIsEventIdentityAcrossReplay(t *testing.T) {
	w, _ := testWriter(t)
	batch := openBatch("run-append-receipt", time.Now())
	batch.Ops[1].Receipt = true
	batch.Ops = append(batch.Ops, appendOp("tail", time.Now(), journal.Event{Type: journal.EventRunnerAnnotation}))
	first, err := w.Emit(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.AppendReceipts) != 1 || first.AppendReceipts[0].Seq >= first.Seq {
		t.Fatalf("receipt used watermark: %+v", first)
	}
	for range 2 {
		got, err := w.Emit(context.Background(), batch)
		if err != nil || !reflect.DeepEqual(got.AppendReceipts, first.AppendReceipts) || got.Applied != 0 {
			t.Fatalf("receipt changed: %+v %v", got, err)
		}
		w.CloseIdle(0)
	}
}

func testAppendReceiptCheckpointRetry(t *testing.T, adopted, reopen bool) {
	w, dir := testWriter(t)
	batch := openBatch("run-checkpoint-origin", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	start := batch.Ops[1]
	batch.Ops = batch.Ops[:1]
	if _, err := w.Emit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	start.Receipt = true
	req := EmitRequest{RunID: batch.RunID, Gaggle: batch.Gaggle, Ops: []Op{start}}
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
	if response, err := w.Emit(context.Background(), req); err == nil || len(response.AppendReceipts) != 0 {
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
	if response, err := w.Emit(context.Background(), req); err == nil || len(response.AppendReceipts) != 0 {
		t.Fatalf("uncertain retry returned receipt: %+v %v", response, err)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	retried, err := w.Emit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(retried.AppendReceipts) != 1 || retried.AppendReceipts[0].Seq != first.Seq {
		t.Fatalf("recovered identity changed: %+v", retried)
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
	if count != 1 || len(retried.AppendReceipts) != 1 || retried.AppendReceipts[0].Seq != first.Seq || retried.Deduplicated != 1 || retried.Applied != 0 {
		t.Fatalf("same emit key duplicated committed origin: starts=%d originalSeq=%d retry=%+v", count, first.Seq, retried)
	}
}
