package parallelworkspace

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

func TestVerifySourceBoundaryUsesSuppliedEvents(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	meta := sourceMetadata{Parallel: "fan", Sequence: 3, Snapshot: recovery.ChildSnapshot{Record: recovery.Record{CreatedAt: at}}}
	good := journal.Event{Seq: 3, Type: journal.EventParallelStarted, Parallel: "fan", Time: at}
	if err := verifySourceBoundary([]journal.Event{{Seq: 2}, good}, meta); err != nil {
		t.Fatal(err)
	}
	if err := verifySourceBoundary([]journal.Event{{Seq: 2}}, meta); err == nil {
		t.Fatal("missing boundary accepted")
	}
	bad := good
	bad.Parallel = "other"
	if err := verifySourceBoundary([]journal.Event{bad}, meta); err == nil {
		t.Fatal("changed boundary accepted")
	}
}
