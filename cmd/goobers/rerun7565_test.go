package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

type rerunRequestReader struct{ readservice.Reader }

type rerunRequestAgent struct {
	calls     atomic.Int32
	reviews   atomic.Int32
	preserved bool
}

func (a *rerunRequestAgent) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	a.calls.Add(1)
	if a.preserved {
		data, err := os.ReadFile(filepath.Join(env.Workspace, "reviewed.txt"))
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("preserved implementation missing: %w", err)
		}
		if string(data) != "reviewed implementation\n" {
			return apiv1.ResultEnvelope{}, fmt.Errorf("preserved implementation changed: %q", data)
		}
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
func (a *rerunRequestAgent) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	a.reviews.Add(1)
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestRerunHTTPAfterLocalCI(t *testing.T) {
	for _, tc := range []struct {
		name               string
		mode               apiv1.WorkspaceMode
		preserve, conflict bool
	}{
		{name: "lost-custody", mode: apiv1.WorkspaceRepo},
		{name: "preserved", mode: apiv1.WorkspaceRepo, preserve: true},
		{name: "claim-conflict", mode: apiv1.WorkspaceRepo, preserve: true, conflict: true},
		{name: "scratch-with-automated-gate", mode: apiv1.WorkspaceScratch},
		{name: "readonly-with-automated-gate", mode: apiv1.WorkspaceRepoReadOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine, err := workflow.Compile(workflow.Definition{Name: "implementation", Version: 1, Spec: apiv1.WorkflowSpec{
				Gaggle: "example", Start: "implement", Tasks: []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goal: "implement", Goober: "coder", Workspace: tc.mode, Next: "review"}},
				Gates: []apiv1.Gate{
					{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer", Workspace: tc.mode}, Branches: map[string]string{"pass": "local-ci", "fail": workflow.TargetEscalate, "needs-changes": workflow.TargetEscalate}},
					{Name: "local-ci", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "status-equals"}, Branches: map[string]string{"pass": workflow.TerminalComplete, "fail": workflow.TargetEscalate}},
				}}}, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			events := []journal.Event{
				{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
				{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
				{Type: journal.EventGateStarted, Gate: "review", Attempt: 1},
				{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Target: "local-ci"},
				{Type: journal.EventGateStarted, Gate: "local-ci", Attempt: 4},
				{Type: journal.EventGateEvaluated, Gate: "local-ci", Attempt: 4, Verdict: "fail", Target: workflow.TargetEscalate},
				{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
			}
			fixture := newInterventionWiringFixture(t, machine, "run-parked", events, interventionDeterministic{}, func(cfg *intervention.Config) { cfg.WaitGroup = &wg })
			defer wg.Wait()
			scoped := fixture.layout.ForGaggle("example")
			source := createRecoveryCleanupSource(t)
			manager, err := worktree.NewManager(scoped.WorkcopiesDir())
			if err != nil {
				t.Fatal(err)
			}
			agent := &rerunRequestAgent{preserved: tc.preserve}
			runRunner, err := runner.New(runner.Config{
				NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
					return agent, nil
				},
				Automated: gate.NewAutomatedEvaluator(), Worktrees: manager, RunsDir: scoped.RunsDir(), ScratchDir: t.TempDir(),
				RepoCloneURL: func(apiv1.RepoRef) (string, error) { return source, nil },
				FinalizeTerminal: func(runID string, _ journal.RunPhase) error {
					return releaseClaimsForRun(fixture.layout, fixture.instanceLog, runID)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			snapshot := fixture.definitions.Snapshot()
			snapshot.runners = map[string]*runner.Runner{"example": runRunner}
			fixture.definitions.Replace(snapshot)
			fixture.runnerRegistry.Replace(snapshot.runners)
			branch := providers.BranchName(machine.Def.Name, "run-parked")
			if tc.mode == apiv1.WorkspaceRepo {
				wt, err := manager.Create(t.Context(), worktree.CreateOptions{RepoURL: source, RunID: "run-parked-implement", OwnerRunID: "run-parked", BaseRef: "main", Branch: branch})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(wt.Path, "reviewed.txt"), []byte("reviewed implementation\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runTestGit(t, wt.Path, "add", "reviewed.txt")
				runTestGit(t, wt.Path, "commit", "-m", "implementation")
				if err := wt.Remove(t.Context(), worktree.RemoveOptions{}); err != nil {
					t.Fatal(err)
				}
				if _, err := manager.FinalizeRun(t.Context(), "run-parked"); err != nil {
					t.Fatal(err)
				}
				if !tc.preserve {
					_, err := manager.WithExistingMirror(t.Context(), source, func(repository string) error { runTestGit(t, repository, "branch", "-D", branch); return nil })
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, kind := range []journal.EventType{journal.EventClaimAcquired, journal.EventClaimReleased} {
				if err := fixture.instanceLog.Append(journal.Event{Type: kind, Name: "123", Gaggle: "example", RunID: "run-parked", Workflow: machine.Def.Name, Runner: map[string]any{"claimProvider": "github", "claimExternalId": "123"}}); err != nil {
					t.Fatal(err)
				}
			}
			key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "123"}
			ledgerPath := filepath.Join(fixture.layout.SchedulerDir(), claimLedgerFileName)
			if tc.conflict {
				ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
				if err != nil {
					t.Fatal(err)
				}
				if ok, _, err := ledger.ClaimScoped(key, "other-run", machine.Def.Name, time.Hour); err != nil || !ok {
					t.Fatalf("claim competitor: %v %v", ok, err)
				}
			}
			handler, err := httpapi.NewHandler(rerunRequestReader{}, httpapi.AllowAll, log.New(io.Discard, "", 0), httpapi.WithInterventions(fixture.service))
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			refused := tc.mode == apiv1.WorkspaceRepo && !tc.preserve || tc.conflict
			for attempt := 0; attempt < 2; attempt++ {
				req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/runs/run-parked/stages/implement/rerun", strings.NewReader(`{"actor":"operator","instructionAddendum":"Fix formatting and rerun ordinary gates"}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set(httpapi.HeaderIdempotencyKey, "rerun-local-ci")
				response, err := server.Client().Do(req)
				if err != nil {
					t.Fatalf("rerun transport: %v", err)
				}
				body, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if refused {
					code := "rerun_custody_unavailable"
					if tc.conflict {
						code = "claim_unavailable"
					}
					if response.StatusCode != http.StatusConflict || !strings.Contains(string(body), code) {
						t.Fatalf("status %d: %s", response.StatusCode, body)
					}
				} else if response.StatusCode != http.StatusOK {
					t.Fatalf("status %d: %s", response.StatusCode, body)
				}
				wg.Wait()
			}
			rd, err := journal.OpenRead(fixture.runDir)
			if err != nil {
				t.Fatal(err)
			}
			got, err := rd.Events()
			if err != nil {
				t.Fatal(err)
			}
			if refused {
				if len(got) != len(events)+1 {
					t.Fatalf("refused request changed journal: %d events, want %d", len(got), len(events)+1)
				}
				if agent.calls.Load() != 0 {
					t.Fatal("refused request invoked agent")
				}
			} else {
				counts := map[journal.EventType]int{}
				for _, e := range got {
					if e.Type == journal.EventStageRerunRequested || e.Type == journal.EventRunnerAnnotation && e.Runner["kind"] == "intervention.idempotency" {
						counts[e.Type]++
					}
				}
				if counts[journal.EventStageRerunRequested] != 1 || counts[journal.EventRunnerAnnotation] != 1 || agent.calls.Load() != 1 || agent.reviews.Load() != 1 {
					t.Fatalf("duplicate request or skipped review: %v calls=%d reviews=%d", counts, agent.calls.Load(), agent.reviews.Load())
				}
				phase, err := rd.Phase()
				if err != nil || phase != journal.PhaseCompleted {
					t.Fatalf("rerun phase: %s %v", phase, err)
				}
			}
			ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			claim, held := ledger.LookupScoped(key)
			if tc.conflict {
				if !held || claim.RunID != "other-run" {
					t.Fatal("changed competing claim")
				}
			} else if held {
				t.Fatalf("leaked claim: %+v", claim)
			}
		})
	}
}
