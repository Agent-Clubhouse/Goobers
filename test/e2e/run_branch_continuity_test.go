// Regression coverage for issue #133 (run-branch continuity): before the fix,
// internal/runner/buildEnvelope created every stage's worktree as a detached
// checkout of `main`, so a file the implement stage produced was invisible to
// local-ci and the reviewer gate — local-ci ran against pristine main and was
// vacuously green. These tests drive the REAL runner (like #29's walking
// skeleton) but with an implementer fake that actually writes + commits a file
// into the run branch, then assert the later stages see it — which only holds
// once each stage's worktree checks out the shared run branch
// (providers.BranchName) instead of main.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

const continuityFile = "IMPLEMENTED"

// continuityMachine is #29's skeleton shape, except local-ci reads the file the
// implement stage committed from the stage's worktree, so the deterministic
// stage fails unless its worktree carries the implement stage's commit.
// implement (agentic) -> review (agentic gate, passes) ->
// local-ci (deterministic, reads the file) -> terminal.
func continuityMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement the item", Retry: &apiv1.RetryPolicy{MaxAttempts: 2}, Next: "review"},
			{
				Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "verify the implement stage's change is present",
				Run: &apiv1.DeterministicRun{
					Command: e2eTestCommand(t),
					Env: map[string]string{
						e2eCommandHelperMode: "read-file",
						e2eCommandHelperPath: continuityFile,
					},
				},
			},
		},
		Gates: []apiv1.Gate{
			{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{"pass": "local-ci", "needs-changes": "implement", "fail": workflow.TargetAbort},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "run-branch-continuity", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile continuity machine: %v", err)
	}
	return m
}

// newContinuityRunner builds a real runner sharing the given worktree Manager,
// with an implementer fake that writes + commits continuityFile into its
// worktree (which #133's fix puts on the run branch) and a reviewer fake that
// passes. Sharing one Manager across runs is what lets the concurrency test
// prove distinct runs never collide on the branch.
func newContinuityRunner(t *testing.T, mgr *worktree.Manager, fixtureRepo, runsDir string) *runner.Runner {
	t.Helper()
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	r, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, ierr := credentials.NewInjector(resolver, nil, reg)
			if ierr != nil {
				return nil, ierr
			}
			return executor.NewShellExecutor(injector, rec)
		},
		NewAgentic: func(gooberName string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
			injector, ierr := credentials.NewInjector(resolver, nil, reg)
			if ierr != nil {
				return nil, ierr
			}
			adapter := &harness.FakeAdapter{
				Transcript: []byte("fake harness session for " + gooberName + "\n"),
				Act: func(_ context.Context, req harness.RunRequest) error {
					if gooberName == "coder" {
						// Write + commit the change into the worktree. #133's fix
						// puts this worktree on the run branch, so the commit
						// advances that branch and later stages inherit it.
						if werr := os.WriteFile(filepath.Join(req.Workspace, continuityFile), []byte("done\n"), 0o644); werr != nil {
							return werr
						}
						runSkeletonGit(t, req.Workspace, "add", continuityFile)
						runSkeletonGit(t, req.Workspace, "-c", "user.email=impl@test", "-c", "user.name=impl", "commit", "-m", "implement: add "+continuityFile)
						return harness.WriteCompletion(req.Workspace, req.CompletionPath, resultPayload(apiv1.ResultSuccess, "implemented"))
					}
					return harness.WriteCompletion(req.Workspace, req.CompletionPath, verdictPayload(apiv1.VerdictPass, "looks good"))
				},
			}
			recorder, ok := rec.(harness.SpanRecorder)
			if !ok {
				return nil, fmt.Errorf("test double %T does not implement harness.SpanRecorder", rec)
			}
			// rec is this run's own *journal.Run, which satisfies Dir()
			// structurally — harness.NewContextResolver pairs that with
			// runsDir (this test's own instance layout) for cross-run
			// resolution (#103/T3), mirroring cmd/goobers/runnerwiring.go's
			// production wiring.
			direr, ok := rec.(interface{ Dir() string })
			if !ok {
				return nil, fmt.Errorf("test double %T does not implement Dir() string", rec)
			}
			contextResolver := harness.NewContextResolver(direr, runsDir)
			registryScrubber, ok := reg.(journal.Scrubber)
			if !ok {
				return nil, fmt.Errorf("test double %T does not implement journal.Scrubber", reg)
			}
			scrubber := journal.Chain(registryScrubber, journal.NewPatternScrubber())
			return harness.NewExecutor(adapter, injector, recorder, rec, contextResolver, scrubber, "you are the "+gooberName+" fixture goober")
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    mgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

// TestRunBranchContinuityLocalCISeesImplementDiff is #133's headline
// regression: local-ci reads a file the implement stage committed, and the run
// completes only because local-ci's worktree checked out the run branch (not a
// pristine main). On the pre-#133 detach-at-main behavior local-ci's file read
// would fail and the run would not reach completed.
func TestRunBranchContinuityLocalCISeesImplementDiff(t *testing.T) {
	instanceRoot := t.TempDir()
	mgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newSkeletonFixtureRepo(t)
	machine := continuityMachine(t)
	r := newContinuityRunner(t, mgr, fixtureRepo, runsDir)

	const runID = "run-continuity-1"
	res, err := r.Start(context.Background(), skeletonStartInput(runID, machine))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed — local-ci must see the implement stage's commit (run-branch continuity, #133)", res.Phase)
	}
	if res.FinalState != "local-ci" {
		t.Fatalf("finalState = %q, want local-ci", res.FinalState)
	}

	// local-ci ran and finished success (its IMPLEMENTED read found the file):
	// the deterministic stage evaluated the run's actual diff.
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	sawLocalCISuccess := false
	for _, e := range events {
		if e.Type == journal.EventStageFinished && e.Stage == "local-ci" {
			if e.Status != string(apiv1.ResultSuccess) {
				t.Errorf("local-ci finished %q, want success — it did not see the implement diff", e.Status)
			}
			sawLocalCISuccess = true
		}
	}
	if !sawLocalCISuccess {
		t.Error("no local-ci stage.finished event — the run never reached local-ci")
	}
}

// TestRunBranchContinuityConcurrentRunsDistinctBranches proves the branch is
// per-run: two runs with distinct run ids, sharing one worktree Manager (one
// managed clone), both complete without colliding on the run branch —
// providers.BranchName keys on the run id, so their branches differ. If the
// runner shared a single branch across runs, the second run's worktree add
// would fail (git forbids one branch in two live worktrees) or clobber the
// first's commits.
func TestRunBranchContinuityConcurrentRunsDistinctBranches(t *testing.T) {
	instanceRoot := t.TempDir()
	mgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newSkeletonFixtureRepo(t)
	machine := continuityMachine(t)

	runIDs := []string{"run-concurrent-a", "run-concurrent-b"}
	// Distinct run ids -> distinct branches (the property under test).
	if providers.BranchName(machine.Def.Name, runIDs[0]) == providers.BranchName(machine.Def.Name, runIDs[1]) {
		t.Fatal("precondition: distinct run ids must yield distinct branch names")
	}

	var wg sync.WaitGroup
	errs := make([]error, len(runIDs))
	phases := make([]journal.RunPhase, len(runIDs))
	for i, id := range runIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			r := newContinuityRunner(t, mgr, fixtureRepo, runsDir)
			res, err := r.Start(context.Background(), skeletonStartInput(id, machine))
			errs[i], phases[i] = err, res.Phase
		}(i, id)
	}
	wg.Wait()

	for i, id := range runIDs {
		if errs[i] != nil {
			t.Errorf("run %s: Start error %v (a branch collision across runs would surface here)", id, errs[i])
		}
		if phases[i] != journal.PhaseCompleted {
			t.Errorf("run %s: phase = %q, want completed", id, phases[i])
		}
	}
}
