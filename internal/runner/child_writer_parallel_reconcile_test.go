package runner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestReconcileChildWriterPreservesInterleavedSiblingCustody(t *testing.T) {
	id := journal.RunIdentity{RunID: "child-proof", Gaggle: "web", Workflow: "generated", ConfigGeneration: journal.Digest([]byte("config")), WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goobers")), Child: childWorkspaceStart(nil).Child}
	id.Child.AcceptanceID = "trigger-" + id.RunID
	admission, _ := json.Marshal(ChildWorkspaceAdmission{WorkspaceID: "child-owned", ForkSHA: strings.Repeat("1", 40), RepositoryDigest: strings.Repeat("2", 64)})
	jr, err := journal.Create(t.TempDir(), id, map[string][]byte{childWriterPolicyInput: []byte(childWriterPolicy), ChildWorkspaceInputName: admission}, journal.WithInputIntegrity(map[string]apiv1.Integrity{childWriterPolicyInput: apiv1.IntegrityTrusted, ChildWorkspaceInputName: apiv1.IntegrityTrusted}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()
	// Both repository scopes start before either pod marker. The first scope
	// must be matched by branch, not merely by its position in the journal.
	for _, event := range []journal.Event{
		{Type: journal.EventStageStarted, Stage: "left", Attempt: 1, Branch: 1},
		{Type: journal.EventStageStarted, Stage: "right", Attempt: 1, Branch: 2},
		{Type: journal.EventRunnerAnnotation, Stage: "child-proof:left", Attempt: 1, Branch: 1, Runner: map[string]any{"kind": childWriterStarted, "writerScope": strings.Repeat("1", 32)}},
		{Type: journal.EventRunnerAnnotation, Stage: "child-proof:right", Attempt: 1, Branch: 2, Runner: map[string]any{"kind": childWriterStarted, "writerScope": strings.Repeat("2", 32)}},
		{Type: journal.EventRunnerAnnotation, Stage: "left", Attempt: 1, Branch: 1, Runner: map[string]any{"kind": "isolated.child.writer.started"}},
		{Type: journal.EventRunnerAnnotation, Stage: "right", Attempt: 1, Branch: 2, Runner: map[string]any{"kind": "isolated.child.writer.started"}},
	} {
		if err := jr.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	rd, err := journal.OpenReadOnly(jr.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err = rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	origins, pods := map[int]journal.Event{}, map[int]journal.Event{}
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			origins[event.Branch] = event
		}
		if event.Runner["kind"] == "isolated.child.writer.started" {
			pods[event.Branch] = event
		}
	}
	foreign := pods[1]
	foreign.Branch = 2
	if err := ReconcileChildWorkspaceWriter(rd, jr, id, origins[1], foreign); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal("foreign pod branch authorized recovery", err)
	}
	for i := 0; i < 2; i++ {
		if err := ReconcileChildWorkspaceWriter(rd, jr, id, origins[1], pods[1]); err != nil {
			t.Fatal("exact left recovery must be repeatable with sibling pending", err)
		}
	}
	if err := verifyChildWriterJournal(t, jr); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal("left recovery settled the whole family", err)
	}
	if err := ReconcileChildWorkspaceWriter(rd, jr, id, origins[2], pods[2]); err != nil {
		t.Fatal("exact right recovery", err)
	}
	if err := verifyChildWriterJournal(t, jr); err != nil {
		t.Fatal("settled siblings retained a writer", err)
	}
}
