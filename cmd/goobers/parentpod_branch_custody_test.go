package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func TestParentPodBranchCustodyDoesNotBorrowSiblingJoin(t *testing.T) {
	f := containedParentFixture(t)
	run, _ := configuredChildStage(t, f)
	_, id, err := parentJournal(run)
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := journal.OpenReadOnly(run.Dir())
	blobs := map[int]*parentInvocationBlobs{}
	for branch := 1; branch <= 2; branch++ {
		seq, origin, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1, Branch: branch}, false)
		if err != nil {
			t.Fatal(err)
		}
		events, _ := reader.Events()
		var started journal.Event
		for _, event := range events {
			if event.Seq == seq {
				started = event
			}
		}
		recorder, err := runner.OwnedBranchRecorder(run, branch)
		if err != nil {
			t.Fatal(err)
		}
		if _, actual, err := parentJournal(recorder); err != nil || actual.RunID != id.RunID {
			t.Fatal(actual, err)
		}
		c := childpod.Contract{Version: 1, Identity: id, ParentOrigin: origin, Stage: "plan", Attempt: 1, PodAttempt: int(seq), StartedAt: started.Time, Ceiling: credentials.NewChildCeiling(false, nil, nil)}
		data, _ := json.Marshal(c)
		b := &parentInvocationBlobs{ParentBlobs: childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}, recorder: recorder, contract: c, contractDigest: journal.Digest(data)}
		if err := b.Put(t.Context(), b.contractDigest, data); err != nil {
			t.Fatal(err)
		}
		if err := b.record(parentPodWriterStarted); err != nil {
			t.Fatal(err)
		}
		blobs[branch] = b
	}
	if err := blobs[1].record(parentPodWriterJoined); err != nil {
		t.Fatal(err)
	}
	if err := verifyParentPodBranchCustody(reader, 1); err != nil {
		t.Fatal("joined branch refused", err)
	}
	if err := verifyParentPodBranchCustody(reader, 2); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal("sibling borrowed join", err)
	}
	if err := verifyParentPodCustody(reader); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal("whole-run recovery borrowed join", err)
	}
	if err := blobs[2].record(parentPodWriterJoined); err != nil {
		t.Fatal(err)
	}
	if err := verifyParentPodCustody(reader); err != nil {
		t.Fatal(err)
	}
}
