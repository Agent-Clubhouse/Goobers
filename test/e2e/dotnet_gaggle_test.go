//go:build integration

// PLY-4 (#1093): the reference C#/.NET gaggle's shipped-workflow contract test.
// It drives the REAL runner with a fake agent harness (no live LLM) over the
// ACTUAL shipped config-examples/gaggles/dotnet-service workflow, and proves the
// polyglot CI gate works end to end: the gaggle's declared `ciCommand:
// ["dotnet","test"]` (resolved into the local-ci stage by #1009's
// ApplyGaggleCICommand) runs a real `dotnet build && dotnet test` against a real
// .NET project in the run's worktree and comes back green. Skips when the .NET
// SDK is absent (cloud CI pinning is soft/stretch this sprint) — validated
// locally on a host that has the SDK.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

const dotnetGaggleDir = "../../config-examples/gaggles/dotnet-service"

// dotnetServiceMachine loads the SHIPPED dotnet-service gaggle + implementation
// workflow, resolves the per-gaggle CI command into the local-ci stage exactly
// as the daemon does (#1009), and compiles the result. This is what makes the
// test a "shipped-workflow contract test": the machine under test IS the example
// config, and the local-ci command it runs is proven to be the gaggle's
// `dotnet test` — not a value hard-coded by the test.
func dotnetServiceMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(dotnetGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(dotnetGaggleDir, "workflows", "dotnet-implementation.yaml"))
	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"dotnet-implementer", "dotnet-reviewer"} {
		g := loadYAML[apiv1.Goober](t, filepath.Join(dotnetGaggleDir, "goobers", name, "goober.yaml"))
		goobers[g.Name] = g.Spec
	}

	// Resolve the gaggle's ciCommand into the local-ci stage (the real #1009
	// seam), then assert it actually became `dotnet test` before we run it.
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{wf}}
	instance.ApplyGaggleCICommand(set)
	wf = set.Workflows[0]
	if got := localCICommand(wf); fmt.Sprint(got) != fmt.Sprint([]string{"dotnet", "test"}) {
		t.Fatalf("local-ci command = %v, want [dotnet test] after #1009 gaggle-ciCommand resolution", got)
	}

	m, err := workflow.Compile(workflow.Definition{Name: wf.Name, Version: 1, Spec: wf.Spec}, workflow.WithGoobers(goobers))
	if err != nil {
		t.Fatalf("compile dotnet-service machine: %v", err)
	}
	return m
}

func localCICommand(wf apiv1.Workflow) []string {
	for _, task := range wf.Spec.Tasks {
		if task.Name == "local-ci" && task.Run != nil {
			return task.Run.Command
		}
	}
	return nil
}

func loadYAML[T any](t *testing.T, path string) T {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out T
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return out
}

// newDotnetFixtureRepo builds a bare git repo whose main branch contains the
// real .NET project from testdata, so every stage's worktree (checked out on the
// run branch off main) carries a buildable, testable service for local-ci's
// `dotnet test` to run against.
func newDotnetFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	runSkeletonGit(t, work, "init", "-b", "main")
	runSkeletonGit(t, work, "config", "user.email", "fixture@test")
	runSkeletonGit(t, work, "config", "user.name", "fixture")
	if err := os.CopyFS(work, os.DirFS("testdata/dotnetservice")); err != nil {
		t.Fatalf("copy .NET fixture: %v", err)
	}
	runSkeletonGit(t, work, "add", "-A")
	runSkeletonGit(t, work, "commit", "-m", "seed .NET service")
	bare := filepath.Join(t.TempDir(), "dotnet-service.git")
	runSkeletonGit(t, filepath.Dir(bare), "clone", "--bare", work, bare)
	return bare
}

// newDotnetGaggleRunner mirrors newContinuityRunner but scripts the
// dotnet-service goobers: dotnet-implementer commits a small marker (so the run
// branch has a non-empty diff for the review gate), dotnet-reviewer passes, and
// local-ci runs the real `dotnet test`.
func newDotnetGaggleRunner(t *testing.T, mgr *worktree.Manager, fixtureRepo, runsDir string) *runner.Runner {
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
					if gooberName == "dotnet-implementer" {
						// A non-empty diff so the review gate doesn't empty-diff
						// fast-fail; the buildable service already lives on main.
						if werr := os.WriteFile(filepath.Join(req.Workspace, "CHANGELOG.md"), []byte("- reference change\n"), 0o644); werr != nil {
							return werr
						}
						runSkeletonGit(t, req.Workspace, "add", "CHANGELOG.md")
						runSkeletonGit(t, req.Workspace, "-c", "user.email=impl@test", "-c", "user.name=impl", "commit", "-m", "implement: touch changelog")
						return harness.WriteCompletion(req.Workspace, req.CompletionPath, resultPayload(apiv1.ResultSuccess, "implemented"))
					}
					return harness.WriteCompletion(req.Workspace, req.CompletionPath, verdictPayload(apiv1.VerdictPass, "looks good"))
				},
			}
			recorder, ok := rec.(harness.SpanRecorder)
			if !ok {
				return nil, fmt.Errorf("test double %T does not implement harness.SpanRecorder", rec)
			}
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

// TestDotnetServiceGaggleRunsLocalCIGreen is #1093's headline acceptance: the
// shipped .NET gaggle runs through the real runner + fake harness and its
// local-ci stage's real `dotnet test` comes back green — proving a non-Go stack
// builds and tests through the same machinery, gated only by the per-gaggle CI
// command (#1009) and, at schedule time, the dotnet@9 runner capability
// (#1101).
func TestDotnetServiceGaggleRunsLocalCIGreen(t *testing.T) {
	// Opt-in only. This test runs a real `dotnet test`, which restores NuGet
	// packages over the network and builds the SDK — a dependency deliberately
	// kept OUT of the shared `make ci` gate: cloud CI pinning is soft/stretch
	// this sprint (#1093), and a network-dependent restore has no place in the
	// zero-tolerance flake budget. It is validated locally on a host that has
	// the SDK — set GOOBERS_DOTNET_E2E=1 to run it. An evergreen CI leg pinning
	// the reference gaggle is the tracked stretch follow-up.
	if os.Getenv("GOOBERS_DOTNET_E2E") == "" {
		t.Skip("set GOOBERS_DOTNET_E2E=1 to run the .NET gaggle e2e (opt-in; needs the SDK + network)")
	}
	if _, err := exec.LookPath("dotnet"); err != nil {
		t.Skip("dotnet SDK not available")
	}

	instanceRoot := t.TempDir()
	mgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newDotnetFixtureRepo(t)
	machine := dotnetServiceMachine(t)
	r := newDotnetGaggleRunner(t, mgr, fixtureRepo, runsDir)

	const runID = "run-dotnet-gaggle-1"
	res, err := r.Start(context.Background(), skeletonStartInput(runID, machine))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q (%s: %s), want completed — the .NET local-ci must build+test green",
			res.Phase, res.FailureStage, res.FailureMessage)
	}

	// local-ci ran the real `dotnet test` and finished success.
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
				t.Errorf("local-ci finished %q, want success — dotnet test did not pass", e.Status)
			}
			sawLocalCISuccess = true
		}
	}
	if !sawLocalCISuccess {
		t.Error("no local-ci stage.finished event — the run never reached the .NET build/test gate")
	}
}

// TestDotnetServiceGaggleFailsToScheduleWithoutDotnetCapability is #1093's
// fail-closed acceptance (needs no SDK): the shipped gaggle declares
// `requiredCapabilities: [dotnet@9]`, so a runner that does not claim it is
// refused UP FRONT with a diagnostic naming the missing capability (RRQ-1/#1101)
// — not a cryptic runtime "command not found". A runner that claims it schedules.
func TestDotnetServiceGaggleFailsToScheduleWithoutDotnetCapability(t *testing.T) {
	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(dotnetGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(dotnetGaggleDir, "workflows", "dotnet-implementation.yaml"))
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{wf}}

	err := instance.CheckCapabilityRequirements([]string{"os=linux"}, set)
	if err == nil || !strings.Contains(err.Error(), "dotnet@9") {
		t.Fatalf("a runner not claiming dotnet@9 must fail to schedule with a diagnostic naming it, got %v", err)
	}
	if err := instance.CheckCapabilityRequirements([]string{"dotnet@9"}, set); err != nil {
		t.Fatalf("a runner claiming dotnet@9 must schedule, got %v", err)
	}
}
