//go:build integration

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationPythonServiceGaggleRunsPytestGreen(t *testing.T) {
	// Opt-in only (like the .NET/Java reference-gaggle integration tests):
	// GOOBERS_PYTHON_E2E gates this to a host a human deliberately prepared,
	// so an environment that fails these checks after opting in is a real
	// host misconfiguration — fail loud (t.Fatalf), don't skip. The
	// integration tier forbids raw t.Skip/t.Skipf entirely (test/integration's
	// dependency guard); testdep.Require only covers PATH presence, so the
	// Python 3.12 + pytest requirements are enforced here as hard failures.
	testdep.RequireEnv(t, "GOOBERS_PYTHON_E2E")
	testdep.Require(t, "python3")
	version, err := exec.Command("python3", "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("python3 --version: %v: %s", err, version)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(version)), "Python 3.12.") {
		t.Fatalf("requires Python 3.12, got %s", strings.TrimSpace(string(version)))
	}
	if output, err := exec.Command("python3", "-m", "pytest", "--version").CombinedOutput(); err != nil {
		t.Fatalf("requires pytest for Python 3.12: %v: %s", err, output)
	}

	t.Setenv("VIRTUAL_ENV", "/custom/python-venv")
	t.Setenv("PYTHONPATH", "/custom/python-modules")
	t.Setenv("PIP_CACHE_DIR", "/custom/python-pip-cache")

	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(pythonGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(pythonGaggleDir, "workflows", "python-implementation.yaml"))
	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"python-implementer", "python-reviewer"} {
		goober := loadYAML[apiv1.Goober](t, filepath.Join(pythonGaggleDir, "goobers", name, "goober.yaml"))
		goobers[goober.Name] = goober.Spec
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{wf}}
	instance.ApplyGaggleCICommand(set)
	wf = set.Workflows[0]
	machine, err := workflow.Compile(
		workflow.Definition{Name: wf.Name, Version: 1, Spec: wf.Spec},
		workflow.WithGoobers(goobers),
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile python-service machine: %v", err)
	}

	instanceRoot := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	localRunner := newPythonGaggleRunner(t, manager, newPythonFixtureRepo(t), runsDir)
	result, err := localRunner.Start(context.Background(), runner.StartInput{
		RunID:                "run-python-gaggle-1",
		Machine:              machine,
		Gaggle:               gaggle.Name,
		Trigger:              journal.Trigger{Kind: journal.TriggerManual},
		RepoRef:              gaggle.Spec.Project,
		RequiredCapabilities: instance.WorkflowRequiredCapabilities(gaggle, wf),
	})
	if err != nil {
		t.Fatalf("start python-service workflow: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q (%s: %s), want completed", result.Phase, result.FailureStage, result.FailureMessage)
	}

	reader, err := journal.OpenRead(filepath.Join(runsDir, "run-python-gaggle-1"))
	if err != nil {
		t.Fatalf("open run journal: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("read run journal: %v", err)
	}
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == "local-ci" {
			if event.Status != string(apiv1.ResultSuccess) {
				t.Fatalf("local-ci status = %q, want success", event.Status)
			}
			return
		}
	}
	t.Fatal("run never reached the pytest local-ci stage")
}

func newPythonFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	runSkeletonGit(t, work, "init", "-b", "main")
	runSkeletonGit(t, work, "config", "user.email", "fixture@test")
	runSkeletonGit(t, work, "config", "user.name", "fixture")
	if err := os.CopyFS(work, os.DirFS("testdata/pythonservice")); err != nil {
		t.Fatalf("copy Python fixture: %v", err)
	}
	runSkeletonGit(t, work, "add", "-A")
	runSkeletonGit(t, work, "commit", "-m", "seed Python service")
	bare := filepath.Join(t.TempDir(), "python-service.git")
	runSkeletonGit(t, filepath.Dir(bare), "clone", "--bare", work, bare)
	return bare
}

func newPythonGaggleRunner(t *testing.T, manager *worktree.Manager, fixtureRepo, runsDir string) *runner.Runner {
	t.Helper()
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	localRunner, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, injectorErr := credentials.NewInjector(resolver, nil, reg)
			if injectorErr != nil {
				return nil, injectorErr
			}
			return executor.NewShellExecutor(injector, rec)
		},
		NewAgentic: func(gooberName string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
			injector, injectorErr := credentials.NewInjector(resolver, nil, reg)
			if injectorErr != nil {
				return nil, injectorErr
			}
			adapter := &harnesstest.FakeAdapter{
				Transcript: []byte("fake Python harness session\n"),
				Act: func(_ context.Context, request harness.RunRequest) error {
					if gooberName == "python-implementer" {
						if err := os.WriteFile(filepath.Join(request.Workspace, "CHANGELOG.md"), []byte("- reference change\n"), 0o644); err != nil {
							return err
						}
						runSkeletonGit(t, request.Workspace, "add", "CHANGELOG.md")
						runSkeletonGit(t, request.Workspace, "-c", "user.email=impl@test", "-c", "user.name=impl", "commit", "-m", "implement reference change")
						return harnesstest.WriteCompletion(request.Workspace, request.CompletionPath, resultPayload(apiv1.ResultSuccess, "implemented"))
					}
					return harnesstest.WriteCompletion(request.Workspace, request.CompletionPath, verdictPayload(apiv1.VerdictPass, "looks good"))
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
			registryScrubber, ok := reg.(journal.Scrubber)
			if !ok {
				return nil, fmt.Errorf("test double %T does not implement journal.Scrubber", reg)
			}
			return harness.NewExecutor(
				adapter,
				injector,
				recorder,
				rec,
				harness.NewContextResolver(direr, runsDir),
				journal.Chain(registryScrubber, journal.NewPatternScrubber()),
				"you are the "+gooberName+" fixture goober",
			)
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    manager,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return localRunner
}
