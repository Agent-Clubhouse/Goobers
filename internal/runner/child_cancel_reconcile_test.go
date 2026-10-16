package runner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestChildCancellationRequiresRepositoryWriterProof(t *testing.T) {
	for _, scenario := range []string{"joined", "open", "missing-policy", "foreign-join"} {
		t.Run(scenario, func(t *testing.T) {
			id := journal.RunIdentity{RunID: "child-proof", Gaggle: "web", Workflow: "generated", ConfigGeneration: journal.Digest([]byte("config")), WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goobers")), Child: childWorkspaceStart(nil).Child}
			id.Child.AcceptanceID = "trigger-" + id.RunID
			admission, err := json.Marshal(ChildWorkspaceAdmission{WorkspaceID: "child-owned", ForkSHA: strings.Repeat("1", 40), RepositoryDigest: strings.Repeat("2", 64)})
			if err != nil {
				t.Fatal(err)
			}
			inputs := map[string][]byte{ChildWorkspaceInputName: admission}
			if scenario != "missing-policy" {
				inputs[childWriterPolicyInput] = []byte(childWriterPolicy)
			}
			jr, err := journal.Create(t.TempDir(), id, inputs, journal.WithInputIntegrity(map[string]apiv1.Integrity{ChildWorkspaceInputName: apiv1.IntegrityTrusted, childWriterPolicyInput: apiv1.IntegrityTrusted}))
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
			scope := strings.Repeat("1", 32)
			appendEvent(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "child-proof:work", Attempt: 1, Runner: map[string]any{"kind": childWriterStarted, "writerScope": scope}})
			if scenario == "joined" || scenario == "foreign-join" {
				stage := "child-proof:work"
				if scenario == "foreign-join" {
					stage = "child-proof:other"
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
			before := jr.Seq()
			result, err := FinalizeCancelledChild(rd, jr, id, time.Now())
			if scenario != "joined" {
				if err == nil || jr.Seq() != before {
					t.Fatal("cancelled without original writer proof", result, err)
				}
				return
			}
			if err != nil || result.Phase != journal.PhaseAborted {
				t.Fatal(result, err)
			}
			before = jr.Seq()
			result, err = FinalizeCancelledChild(rd, jr, id, time.Now())
			if err != nil || result.Phase != journal.PhaseAborted || jr.Seq() != before {
				t.Fatal("cancellation replay changed journal", result, err)
			}
		})
	}
}
