package livejournal

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestExplicitBranchCannotOverrideVerifiedCustody(t *testing.T) {
	for _, branch := range []int{0, 1, 2} {
		w, dir := testWriter(t)
		req := openBatch("run-branch", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
		req.Ops[1].Branch = 2
		_, err := w.Emit(WithRequestBranch(context.Background(), branch), req)
		if branch != 2 {
			if err == nil {
				t.Fatalf("overrode verified branch %d", branch)
			}
			if journal.Recorded(dir + "/" + req.RunID) {
				t.Fatal("invalid later op partially opened batch")
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}

func TestExplicitBranchRejectsInvalidAttributionBeforeWriting(t *testing.T) {
	for _, op := range []Op{
		{Kind: OpAppend, Branch: -1},
		{Kind: OpTranscriptCheckpoint, Branch: 1},
		{Kind: OpAppend, Branch: 1, Event: &journal.Event{Branch: 2}},
	} {
		w, _ := testWriter(t)
		req := openBatch("run-invalid-branch", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
		op.Key = "invalid"
		req.Ops = append(req.Ops, op)
		if _, err := w.Emit(context.Background(), req); err == nil {
			t.Fatalf("accepted invalid attribution: %+v", op)
		}
		if w.IsOpen(req.RunID) {
			t.Fatal("invalid batch acquired writer")
		}
	}
}
