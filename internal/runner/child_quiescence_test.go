package runner

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func childWriterJournal(t *testing.T, policy bool) *journal.Run {
	t.Helper()
	id := journal.RunIdentity{RunID: "child-proof", Gaggle: "web", Workflow: "generated", ConfigGeneration: journal.Digest([]byte("config")), WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goobers")), Child: childWorkspaceStart(nil).Child}
	id.Child.AcceptanceID = "trigger-" + id.RunID
	inputs := map[string][]byte{}
	if policy {
		inputs[childWriterPolicyInput] = []byte(childWriterPolicy)
	}
	jr, err := journal.Create(t.TempDir(), id, inputs, journal.WithInputIntegrity(map[string]apiv1.Integrity{childWriterPolicyInput: apiv1.IntegrityTrusted}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	return jr
}

func verifyChildWriterJournal(t *testing.T, jr *journal.Run) error {
	t.Helper()
	rd, err := journal.OpenReadOnly(jr.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	return VerifyChildWorkspaceQuiescence(rd, id, events)
}

func TestChildWriterCustodyRequiresPersistedActualAcknowledgement(t *testing.T) {
	for _, scenario := range []string{"joined", "absent", "active", "unobserved"} {
		t.Run(scenario, func(t *testing.T) {
			jr := childWriterJournal(t, true)
			env := apiv1.InvocationEnvelope{RunID: "child-proof", TaskID: "work", Attempt: 1}
			_, err := invokeChildWriter(t.Context(), true, jr, env, func(ctx context.Context) (apiv1.ResultEnvelope, error) {
				if scenario != "absent" {
					done := invoke.RegisterWorkspaceWriter(ctx)
					switch scenario {
					case "joined":
						done(nil)
					case "unobserved":
						done(errors.New("writer still alive"))
					}
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			})
			if (err == nil) != (scenario == "joined") {
				t.Fatalf("invocation=%v", err)
			}
			if scenario != "joined" {
				_, retryErr := invokeChildWriter(t.Context(), true, jr, env, func(context.Context) (apiv1.ResultEnvelope, error) {
					t.Fatal("retry launched while earlier writer remained unresolved")
					return apiv1.ResultEnvelope{}, nil
				})
				if retryErr == nil {
					t.Fatal("unresolved writer permitted retry")
				}
			}
			if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
			if err := jr.Close(); err != nil {
				t.Fatal(err)
			}
			if err := verifyChildWriterJournal(t, jr); (err == nil) != (scenario == "joined") {
				t.Fatalf("terminal recovery ignored writer proof: %v", err)
			}
		})
	}
}

func TestChildWriterCustodyDoesNotTrustTerminalStatusOrForeignScope(t *testing.T) {
	for _, scenario := range []string{"missing-policy", "open-scope", "foreign-stage"} {
		t.Run(scenario, func(t *testing.T) {
			jr := childWriterJournal(t, scenario != "missing-policy")
			event := journal.Event{Type: journal.EventRunnerAnnotation, Stage: "work", Attempt: 1, Runner: map[string]any{"kind": childWriterStarted, "writerScope": "11111111111111111111111111111111"}}
			if err := jr.Append(event); err != nil {
				t.Fatal(err)
			}
			if scenario == "foreign-stage" {
				event.Stage = "other"
				event.Runner = map[string]any{"kind": childWriterJoined, "writerScope": "11111111111111111111111111111111"}
				if err := jr.Append(event); err != nil {
					t.Fatal(err)
				}
			}
			if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
				t.Fatal(err)
			}
			if err := verifyChildWriterJournal(t, jr); err == nil {
				t.Fatal("terminal status supplied missing cleanup proof")
			}
		})
	}
}
