package runner

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestReconcileChildWriterClosesOnlyNestedOriginalScope(t *testing.T) {
	for _, scenario := range []string{"exact", "joined", "foreign-join", "different-stage", "different-origin", "absent"} {
		t.Run(scenario, func(t *testing.T) {
			id := journal.RunIdentity{RunID: "child-proof", Gaggle: "web", Workflow: "generated", ConfigGeneration: journal.Digest([]byte("config")), WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goobers")), Child: childWorkspaceStart(nil).Child}
			id.Child.AcceptanceID = "trigger-" + id.RunID
			admission, _ := json.Marshal(ChildWorkspaceAdmission{WorkspaceID: "child-owned", ForkSHA: strings.Repeat("1", 40), RepositoryDigest: strings.Repeat("2", 64)})
			jr, err := journal.Create(t.TempDir(), id, map[string][]byte{childWriterPolicyInput: []byte(childWriterPolicy), ChildWorkspaceInputName: admission}, journal.WithInputIntegrity(map[string]apiv1.Integrity{childWriterPolicyInput: apiv1.IntegrityTrusted, ChildWorkspaceInputName: apiv1.IntegrityTrusted}))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = jr.Close() }()
			appendEvent := func(e journal.Event) {
				t.Helper()
				if err := jr.Append(e); err != nil {
					t.Fatal(err)
				}
			}
			appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "work", Attempt: 1})
			scope := "11111111111111111111111111111111"
			stage := id.RunID + ":work"
			if scenario == "different-stage" {
				stage = id.RunID + ":other"
			}
			if scenario != "absent" {
				appendEvent(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: 1, Runner: map[string]any{"kind": childWriterStarted, "writerScope": scope}})
			}
			appendEvent(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "work", Attempt: 1, Runner: map[string]any{"kind": "isolated.child.writer.started"}})
			if scenario == "joined" || scenario == "foreign-join" {
				if scenario == "foreign-join" {
					stage = "wrong"
				}
				appendEvent(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: 1, Runner: map[string]any{"kind": childWriterJoined, "writerScope": scope}})
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
			var origin, pod journal.Event
			for _, e := range events {
				if e.Type == journal.EventStageStarted {
					origin = e
				}
				if e.Runner["kind"] == "isolated.child.writer.started" {
					pod = e
				}
			}
			if scenario == "different-origin" {
				origin.Attempt = 2
			}
			err = ReconcileChildWorkspaceWriter(rd, jr, id, origin, pod)
			valid := scenario == "exact" || scenario == "joined"
			if (err == nil) != valid {
				t.Fatal(scenario, err)
			}
			if valid {
				if err = ReconcileChildWorkspaceWriter(rd, jr, id, origin, pod); err != nil {
					t.Fatal("repeated recovery", err)
				}
				if err = verifyChildWriterJournal(t, jr); err != nil {
					t.Fatal("scope stayed active", err)
				}
			}
		})
	}
}
