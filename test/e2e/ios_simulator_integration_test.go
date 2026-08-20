//go:build integration

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationIOSSimulatorWorkflowRunsXCUITestGreen(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_IOS_SIMULATOR_E2E")
	testdep.Require(t, "xcodebuild", "xcrun")

	result, events := runIOSSimulatorWorkflow(t, "run-ios-simulator-green", false)
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q (%s: %s), want completed", result.Phase, result.FailureStage, result.FailureMessage)
	}

	var sawStage, sawGate bool
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == "run-xcuitest" {
			sawStage = true
			if event.Status != string(apiv1.ResultSuccess) {
				t.Fatalf("run-xcuitest status = %q, want success", event.Status)
			}
			for _, key := range []string{"xcodeVersion", "simulatorRuntime", "simulatorName"} {
				value, _ := event.Outputs[key].(string)
				if strings.TrimSpace(value) == "" {
					t.Errorf("run-xcuitest output %q = %v, want recorded local tool version", key, event.Outputs[key])
				}
			}
		}
		if event.Type == journal.EventGateEvaluated && event.Gate == "xcuitest-gate" {
			sawGate = true
			if event.Verdict != gate.OutcomePass {
				t.Fatalf("xcuitest-gate verdict = %q, want pass", event.Verdict)
			}
		}
	}
	if !sawStage || !sawGate {
		t.Fatalf("journal missing stage/gate terminal evidence: stage=%t gate=%t", sawStage, sawGate)
	}
}

func TestIntegrationIOSSimulatorWorkflowFailsGateWithXCResultDiagnostics(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_IOS_SIMULATOR_E2E")
	testdep.Require(t, "xcodebuild", "xcrun")

	result, events := runIOSSimulatorWorkflow(t, "run-ios-simulator-failing", true)
	if result.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q (%s: %s), want aborted by failed gate", result.Phase, result.FailureStage, result.FailureMessage)
	}

	var sawStage, sawGate bool
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == "run-xcuitest" {
			sawStage = true
			if event.Status != string(apiv1.ResultFailure) {
				t.Fatalf("run-xcuitest status = %q, want failure", event.Status)
			}
			if event.Error == nil || event.Error.Code != "ios_tests_failed" ||
				!strings.Contains(event.Error.Message, "deliberate XCUITest failure") {
				t.Fatalf("run-xcuitest error = %+v, want parsed xcresult failure diagnostic", event.Error)
			}
		}
		if event.Type == journal.EventGateEvaluated && event.Gate == "xcuitest-gate" {
			sawGate = true
			if event.Verdict != gate.OutcomeFail {
				t.Fatalf("xcuitest-gate verdict = %q, want fail", event.Verdict)
			}
		}
	}
	if !sawStage || !sawGate {
		t.Fatalf("journal missing failed stage/gate evidence: stage=%t gate=%t", sawStage, sawGate)
	}
}

func runIOSSimulatorWorkflow(t *testing.T, runID string, failingTest bool) (runner.Result, []journal.Event) {
	t.Helper()

	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(iosSimulatorGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(iosSimulatorGaggleDir, "workflows", "ios-simulator-test.yaml"))
	machine, err := workflow.Compile(
		workflow.Definition{Name: wf.Name, Version: 1, Spec: wf.Spec},
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile iOS simulator workflow: %v", err)
	}

	instanceRoot := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newIOSSimulatorFixtureRepo(t, failingTest)
	goobersBinary := buildGoobersBinary(t)

	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("new credential resolver: %v", err)
	}
	localRunner, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, injectorErr := credentials.NewInjector(resolver, nil, reg)
			if injectorErr != nil {
				return nil, injectorErr
			}
			shell, shellErr := executor.NewShellExecutor(injector, rec)
			if shellErr != nil {
				return nil, shellErr
			}
			shell.SelfBin = goobersBinary
			return shell, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: manager,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	result, err := localRunner.Start(context.Background(), runner.StartInput{
		RunID:                runID,
		Machine:              machine,
		Gaggle:               gaggle.Name,
		Trigger:              journal.Trigger{Kind: journal.TriggerManual},
		RepoRef:              gaggle.Spec.Project,
		RequiredCapabilities: instance.WorkflowRequiredCapabilities(gaggle, wf),
	})
	if err != nil {
		t.Fatalf("start iOS simulator workflow: %v", err)
	}

	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("open run journal: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("read run journal: %v", err)
	}
	return result, events
}

func newIOSSimulatorFixtureRepo(t *testing.T, failingTest bool) string {
	t.Helper()
	work := t.TempDir()
	runSkeletonGit(t, work, "init", "-b", "main")
	runSkeletonGit(t, work, "config", "user.email", "fixture@test")
	runSkeletonGit(t, work, "config", "user.name", "fixture")
	if err := os.CopyFS(work, os.DirFS("testdata/iossimulator")); err != nil {
		t.Fatalf("copy iOS fixture: %v", err)
	}
	if failingTest {
		path := filepath.Join(work, "GoobersIOSUITests", "SmokeUITests.swift")
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read XCUITest fixture: %v", err)
		}
		const marker = "        let app = XCUIApplication()"
		replacement := "        XCTFail(\"deliberate XCUITest failure\")\n" + marker
		updated := strings.Replace(string(source), marker, replacement, 1)
		if updated == string(source) {
			t.Fatalf("XCUITest fixture marker %q not found", marker)
		}
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			t.Fatalf("write failing XCUITest fixture: %v", err)
		}
	}
	runSkeletonGit(t, work, "add", "-A")
	runSkeletonGit(t, work, "commit", "-m", "seed iOS app")
	bare := filepath.Join(t.TempDir(), "ios-app.git")
	runSkeletonGit(t, filepath.Dir(bare), "clone", "--bare", work, bare)
	return bare
}

func buildGoobersBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "goobers")
	command := exec.Command("go", "build", "-o", binary, "./cmd/goobers")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build goobers binary: %v\n%s", err, output)
	}
	return binary
}
