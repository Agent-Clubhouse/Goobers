package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

func TestParentPodCustodyRequiresExactHostJoinedReceipt(t *testing.T) {
	f := containedParentFixture(t)
	run, env := configuredChildStage(t, f)
	_, id, err := parentJournal(run)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	started, err := childPodStarted(reader, "plan", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	contract := childpod.Contract{Version: 1, Identity: id, ParentOrigin: env.ChildWorkflowOrigin, Stage: "plan", Attempt: 1, PodAttempt: int(started.Seq), StartedAt: started.Time, Ceiling: credentials.NewChildCeiling(false, nil, nil)}
	blobs := &parentInvocationBlobs{ParentBlobs: childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}, recorder: run, hostFork: &recovery.ChildSnapshot{}}
	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(data)
	if err = blobs.Put(t.Context(), digest, data); err != nil {
		t.Fatal(err)
	}
	if err = blobs.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	if err = verifyParentPodCustody(reader); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal("unjoined physical custody accepted", err)
	}
	if err = run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err = verifyParentPodCustody(reader); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal("stage completion acknowledged physical writer", err)
	}
	if err = blobs.record(parentPodWriterJoined); err != nil {
		t.Fatal(err)
	}
	if err = verifyParentPodCustody(reader); err != nil {
		t.Fatal("verified custody refused", err)
	}
	// A duplicate or forged host receipt cannot create a fresh successful scope.
	if err = blobs.record(parentPodWriterJoined); err != nil {
		t.Fatal(err)
	}
	if err = verifyParentPodCustody(reader); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal("duplicate receipt accepted", err)
	}
}
