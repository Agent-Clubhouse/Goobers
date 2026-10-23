//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
)

type pgTerminalExecutor struct{ reviews int }

func (*pgTerminalExecutor) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
func (s *pgTerminalExecutor) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	s.reviews++
	if s.reviews == 1 {
		return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges}, nil
	}
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

type pgLostTerminalReply struct {
	*livejournal.Writer
	lost, calls, cancellations int
	original                   livejournal.TerminalInterventionRequest
}

func (s *pgLostTerminalReply) CommitTerminalIntervention(ctx context.Context, req livejournal.TerminalInterventionRequest) (journal.TerminalInterventionReceipt, error) {
	if len(req.Transition.Cancellation) > 0 {
		s.cancellations++
		return s.Writer.CommitTerminalIntervention(ctx, req)
	}
	s.calls++
	s.original = req
	receipt, err := s.Writer.CommitTerminalIntervention(ctx, req)
	if err == nil && s.calls <= s.lost {
		return journal.TerminalInterventionReceipt{}, errors.New("PG response lost after commit")
	}
	return receipt, err
}

func pgTerminalInput(id string) engine.RunInput {
	preview := true
	return engine.RunInput{RunID: id, Gaggle: "web", WorkflowName: "terminal", Version: 1, DSLVersion: "3.1", LiveJournal: true, PreviewFeaturesEnabled: &preview,
		HITL: &engine.HITLPolicy{Enabled: true, WaitSeconds: 10}, TriggerKind: string(journal.TriggerManual),
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fixture", Name: "repo", Branch: "main"},
		Spec: apiv1.WorkflowSpec{Gaggle: "web", Start: "implement", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Tasks: []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Workspace: apiv1.WorkspaceScratch, Next: "review"}},
			Gates: []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"}, Branches: map[string]string{"pass": "", "needs-changes": "@escalate", "fail": "@abort"}}}}}
}

func TestPostgresEngineTerminalInterventionsCommitBeforeAcknowledgement(t *testing.T) {
	// Gate the tagged root, not a nested fixture: strict CI must never report
	// PASS when every role-backed case was silently skipped.
	if os.Getenv("GOOBERS_TEST_POSTGRES_URL") == "" {
		if os.Getenv("TESTDEP_STRICT") == "1" {
			t.Fatal("GOOBERS_TEST_POSTGRES_URL is required in strict CI")
		}
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set (local PG16 required)")
	}

	for index, action := range []string{"approve", "override", "rerun", "deny", "cancel"} {
		t.Run(action, func(t *testing.T) {
			config := postgresJournalConfig(t)
			var role string
			if err := config.Pool.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
				t.Fatalf("current_user=%q error=%v", role, err)
			}
			writer, backend, root := postgresLiveWriter(t, config)
			seam := &pgLostTerminalReply{Writer: writer, lost: 4}
			if action == "cancel" {
				seam.lost = 100
			}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(&engine.Activities{Journal: seam, Goober: &pgTerminalExecutor{}, Workspaces: receiptGateWorkspace(t.TempDir())})
			input := pgTerminalInput(fmt.Sprintf("%032x", index+910))
			intent := engine.HITLIntent{Protocol: engine.HITLProtocol, Version: engine.HITLProtocolVersion, Kind: engine.HITLResolveEscalation, RunID: input.RunID, RequestID: "terminal-" + action, Actor: "operator", ExpectedTerminalGeneration: 1, Gate: "review", Resolution: action, Decision: "pass", Rationale: "reviewed"}
			if action == "rerun" {
				intent.Kind, intent.Stage, intent.InstructionAddendum = engine.HITLRerunStage, "implement", "fix review"
				intent.Gate, intent.Resolution, intent.Decision = "", "", ""
			}
			if action == "cancel" {
				intent.Resolution = "approve"
			}
			completed := false
			var updateErr error
			var submit func()
			submit = func() {
				value, err := env.QueryWorkflow(engine.HITLStateQuery)
				var state engine.HITLState
				if err == nil {
					err = value.Get(&state)
				}
				if err != nil || state.Phase != "awaiting-operator" {
					env.RegisterDelayedCallback(submit, 100*time.Millisecond)
					return
				}
				if action == "cancel" {
					env.RegisterDelayedCallback(env.CancelWorkflow, 2*time.Second)
				}
				env.UpdateWorkflow(engine.HITLUpdateName, engine.HITLIntentUpdateID(intent), &testsuite.TestUpdateCallback{
					OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
						completed, updateErr = true, err
						if action != "cancel" && err == nil {
							if seam.calls != 5 {
								t.Errorf("acknowledged before retry recovered: calls=%d", seam.calls)
							}
							reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: input.RunID})
							if err != nil {
								t.Fatal(err)
							}
							events, err := reader.Events()
							if err != nil {
								t.Fatal(err)
							}
							found := false
							for _, event := range events {
								if event.Runner["idempotencyKey"] == intent.RequestID {
									found = true
								}
							}
							if !found {
								t.Error("acknowledged before durable terminal marker")
							}
						}
					}}, intent)
			}
			env.RegisterDelayedCallback(submit, 100*time.Millisecond)
			env.ExecuteWorkflow(engine.Run, input)
			if !completed {
				t.Fatalf("Update never completed: workflow=%v", env.GetWorkflowError())
			}
			if action == "cancel" {
				if updateErr == nil || env.GetWorkflowError() == nil || seam.cancellations != 1 {
					t.Fatalf("cancel update=%v workflow=%v reconciliations=%d", updateErr, env.GetWorkflowError(), seam.cancellations)
				}
				if _, err := writer.CommitTerminalIntervention(t.Context(), seam.original); !errors.Is(err, journal.ErrInterventionCancelled) {
					t.Fatalf("late retry=%v", err)
				}
			} else if updateErr != nil || env.GetWorkflowError() != nil {
				t.Fatalf("update=%v workflow=%v", updateErr, env.GetWorkflowError())
			}
			reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: input.RunID})
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			transitions, markers := 0, 0
			for _, event := range events {
				if event.Type == journal.EventRunResumed || event.Type == journal.EventStageRerunRequested {
					transitions++
				}
				if event.Runner["idempotencyKey"] == intent.RequestID && event.Runner["kind"] != "intervention.cancelled" {
					markers++
				}
			}
			expected := 1
			if action == "deny" {
				expected = 0
			}
			if transitions != expected || markers != 1 {
				t.Fatalf("transitions=%d want=%d markers=%d", transitions, expected, markers)
			}
			if action == "cancel" && events[len(events)-1].Status != "aborted" {
				t.Fatalf("cancel tail=%+v", events[len(events)-1])
			}
			assertNoLocalJournal(t, root)
		})
	}
}
