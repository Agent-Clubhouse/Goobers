package childpublication

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func restartPublicationTarget(t *testing.T, q *triggerqueue.Store, target Target) Target {
	t.Helper()
	result := triggerqueue.ChildResult{Receipt: []byte("sealed result for " + target.Identity.RunID)}
	result.ReceiptDigest = journal.Digest(result.Receipt)
	if err := q.KeepChildResult(t.Context(), target.Child, result); err != nil {
		t.Fatal(err)
	}
	at := target.Child.UpdatedAt.Add(time.Second)
	if err := q.SetChildState(t.Context(), target.Child.Identity, triggerqueue.ChildStateUpdate{ExecutionRunID: target.Identity.RunID, Expected: target.Child.State, State: triggerqueue.ChildFailed, ResultRef: result.ReceiptDigest}, at); err != nil {
		t.Fatal(err)
	}
	raw := []byte("bounded retained human plan")
	epoch, _, err := q.BeginChildRestart(t.Context(), triggerqueue.ChildRestartRequest{Identity: target.Child.Identity, RunID: strings.Repeat("d", 32), SourceRunID: target.Identity.RunID, SourceTerminalSeq: 5, SourceResultRef: result.ReceiptDigest, Actor: "issuer:human", Stage: "repair", Plan: raw, PlanDigest: journal.Digest(raw)}, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	target.Child, err = q.GetChild(t.Context(), target.Child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	lineage := *target.Identity.Child
	lineage.ExecutionEpoch, lineage.PriorResultRef, lineage.RestartDigest = epoch.Epoch, epoch.SourceResultRef, epoch.RequestDigest
	target.Identity.Child = &lineage
	target.Identity.ContinuedFromRunID, target.Identity.RunID = target.Identity.RunID, epoch.RunID
	target.Identity.SourceTerminalSeq, target.Identity.Operator, target.Identity.RequestedTarget = epoch.SourceTerminalSeq, epoch.Actor, epoch.Stage
	target.Head = "factory/children/" + epoch.RunID
	return target
}

func TestPublicationRejectsStaleAndChangedEpochBeforeProvider(t *testing.T) {
	q, original, _ := publicationFixture(t)
	target := restartPublicationTarget(t, q, original)
	publisher := Publisher{Queue: q, Git: publicationGitFake{}}
	if _, err := publisher.validateTarget(t.Context(), original); err == nil {
		t.Fatal("old execution selected new slot")
	}
	if _, err := publisher.validateTarget(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"actor", "result", "request", "source", "sequence"} {
		changed := target
		lineage := *target.Identity.Child
		changed.Identity.Child = &lineage
		switch field {
		case "actor":
			changed.Identity.Operator = "issuer:other"
		case "result":
			lineage.PriorResultRef = journal.Digest([]byte("other"))
		case "request":
			lineage.RestartDigest = journal.Digest([]byte("other"))
		case "source":
			changed.Identity.ContinuedFromRunID = strings.Repeat("e", 32)
		case "sequence":
			changed.Identity.SourceTerminalSeq++
		}
		if _, err := publisher.validateTarget(t.Context(), changed); err == nil {
			t.Fatal("changed epoch accepted", field)
		}
	}
}
