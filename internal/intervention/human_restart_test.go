package intervention

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type restartServiceFunc func(context.Context, context.Context, httpapi.Principal, runner.StageRestartPlan) (StageRestartAcceptance, error)

func (f restartServiceFunc) RestartStage(a, e context.Context, p httpapi.Principal, plan runner.StageRestartPlan) (StageRestartAcceptance, error) {
	return f(a, e, p, plan)
}

type restartTestAgent struct {
	mu        sync.Mutex
	envelopes []apiv1.InvocationEnvelope
}

func (a *restartTestAgent) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.envelopes = append(a.envelopes, env)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
func (*restartTestAgent) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestHumanRestartLaunchesOneEpochAfterLostAcknowledgement(t *testing.T) {
	for _, recoverBeforeLaunch := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover-before-launch=%t", recoverBeforeLaunch), func(t *testing.T) {
			machine, err := workflow.Compile(workflow.Definition{Name: "human-restart", Version: 1, DSLVersion: "3.1", Spec: apiv1.WorkflowSpec{Gaggle: "example", Start: "implement", Tasks: []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Workspace: apiv1.WorkspaceScratch, Next: workflow.TerminalComplete}}}}, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatal(err)
			}
			fixture := newInterventionTestFixture(t, machine, machine, true, "source", []journal.Event{{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}, {Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultBlocked)}, {Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}}, interventionDeterministic{})
			var wg sync.WaitGroup
			fixture.service.wg = &wg
			t.Cleanup(wg.Wait)
			manager, err := worktree.NewManager(fixture.layout.ForGaggle("example").WorkcopiesDir())
			if err != nil {
				t.Fatal(err)
			}
			agent := &restartTestAgent{}
			humanRunner, err := runner.New(runner.Config{RunsDir: filepath.Dir(fixture.runDir), ScratchDir: t.TempDir(), Worktrees: manager, StageRestartContext: func(ctx context.Context, _ journal.RunIdentity, _ runner.SecretRegistrar) (context.Context, func(), error) {
				return ctx, func() {}, nil
			}, NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
				return agent, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			fixture.service.stageRestartExecution = func(context.Context, runner.StageRestartPlan) (Execution, error) {
				return Execution{Runner: humanRunner, Machine: machine}, nil
			}
			reg, scrubber := journal.DefaultScrubber()
			reg.Register([]byte("restart-secret-canary"))
			policy, err := interactiveaccess.New([]apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "example"}, Spec: apiv1.GaggleSpec{InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: "https://id.test", Subject: "alice"}}}, Actions: []apiv1.InteractiveAction{"run.intervene", "run.restartStage"}}}}}, nil, interactiveaccess.Dependencies{Registrar: reg})
			if err != nil {
				t.Fatal(err)
			}
			human, err := NewHumanService(fixture.service, policy, humanTestMessages{fixture.runDir}, scrubber)
			if err != nil {
				t.Fatal(err)
			}
			principal := httpapi.Principal{Issuer: "https://id.test", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
			var preflights atomic.Int32
			var lost atomic.Bool
			human.AttachStageRestarts(restartServiceFunc(func(a, e context.Context, _ httpapi.Principal, plan runner.StageRestartPlan) (StageRestartAcceptance, error) {
				if recoverBeforeLaunch && !lost.Load() {
					pending, err := journal.CreateContinuation(filepath.Dir(fixture.runDir), plan.Continuation)
					if err != nil {
						return StageRestartAcceptance{}, err
					}
					if err = pending.Close(); err != nil {
						return StageRestartAcceptance{}, err
					}
				}
				accepted, err := fixture.service.LaunchStageRestart(a, e, plan, func(context.Context, *runner.StageRestartPlan) ([]localscheduler.ClaimEntry, error) {
					preflights.Add(1)
					return nil, nil
				})
				if err == nil && !lost.Swap(true) {
					return StageRestartAcceptance{}, errors.New("simulated lost acknowledgement")
				}
				return accepted, err
			}))
			view, err := human.InspectInteractiveRun(context.Background(), principal, "source")
			if err != nil {
				t.Fatal(err)
			}
			var sequence uint64
			for _, action := range view.Actions {
				if action.Kind == "restart" && action.Available {
					sequence = action.SubjectSequence
				}
			}
			if sequence == 0 {
				t.Fatalf("no restart action: %+v", view)
			}
			saved, err := human.AcceptInteractiveRun(context.Background(), context.Background(), principal, "source", "note", apicontract.InteractiveRunCommand{Kind: "guidance", Stage: "implement", ExpectedSubjectSequence: sequence, Guidance: "Use retained context restart-secret-canary"})
			if err != nil {
				t.Fatal(err)
			}
			command := apicontract.InteractiveRunCommand{Kind: "restart", Stage: "implement", ExpectedSubjectSequence: sequence, GuidanceIDs: []string{saved.Guidance.Request.RequestID}, Rationale: "Apply reviewed fix"}
			if _, err = human.AcceptInteractiveRun(context.Background(), context.Background(), principal, "source", "restart", command); err == nil {
				t.Fatal("lost acknowledgement not simulated")
			}
			wg.Wait()
			result, err := human.AcceptInteractiveRun(context.Background(), context.Background(), principal, "source", "restart", command)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "started" || !result.Accepted || result.ContinuationRunID == "" {
				t.Fatalf("receipt=%+v", result)
			}
			if preflights.Load() != 1 {
				t.Fatalf("successful duplicate reran provider checks: %d", preflights.Load())
			}
			agent.mu.Lock()
			defer agent.mu.Unlock()
			if len(agent.envelopes) != 1 || agent.envelopes[0].Attempt != 2 || !strings.Contains(agent.envelopes[0].InstructionAddendum, "Use retained context") || strings.Contains(agent.envelopes[0].InstructionAddendum, "restart-secret-canary") {
				t.Fatalf("invocations=%+v", agent.envelopes)
			}
			changed := command
			changed.Rationale = "Different intent"
			if _, err = human.AcceptInteractiveRun(context.Background(), context.Background(), principal, "source", "restart", changed); err == nil {
				t.Fatal("changed payload reused epoch")
			}
			admin := principal
			admin.Subject = "admin"
			admin.Roles = []httpapi.Role{httpapi.RoleAdmin}
			if _, err = human.AcceptInteractiveRun(context.Background(), context.Background(), admin, "source", "admin", command); err == nil {
				t.Fatal("instance admin bypassed explicit gaggle membership")
			}

		})
	}
}
