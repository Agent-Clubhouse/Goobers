package main

import (
	"bytes"
	"context"
	"errors"
	"os"
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
)

func TestRejectedReloadFencesSubsequentDeterministicCLIStage(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	applied, err := configDirectoryDigest(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	reloader := &configReloader{
		layout:        layout,
		setup:         &schedulerSetup{InstanceLog: instanceLog},
		appliedDigest: applied,
	}
	rejectedPath := filepath.Join(layout.ConfigDir(), "rejected-generation.yaml")
	if err := os.WriteFile(rejectedPath, []byte("kind: rejected\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rejected, err := configDirectoryDigest(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := reloader.reject(rejected, errors.New("adding a gaggle requires restart")); err != nil {
		t.Fatal(err)
	}

	// The direct CLI boundary still refuses the rejected on-disk generation.
	var stdout, stderr bytes.Buffer
	t.Setenv(executor.InstanceRootEnvVar, root)
	t.Setenv(executor.AppliedConfigDigestEnvVar, applied)
	if code := run([]string{"version"}, &stdout, &stderr); code != 1 {
		t.Fatalf("fenced stage exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, fragment := range []string{configGenerationMismatchCode, rejected, applied, "refusing to read"} {
		if !strings.Contains(stderr.String(), fragment) {
			t.Errorf("fenced stage stderr %q does not contain %q", stderr.String(), fragment)
		}
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Type != journal.EventConfigReloadRejected {
		t.Fatalf("rejected reload was not journaled: %+v", events)
	}

	// Now drive the production launch boundary: Runner -> ShellExecutor ->
	// goobers subprocess. The executor injects its applied digest and consumes
	// the CLI's structured built-in error report into stage.finished.
	runID := "rejected-config-stage"
	runDir := filepath.Join(layout.RunsDir(), runID)
	r := newConfigReloadFenceRunner(t, layout, applied)
	result, err := r.Start(context.Background(), runner.StartInput{
		RunID: runID, Machine: configReloadFenceMachine(t), Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase == journal.PhaseCompleted {
		t.Fatalf("fenced deterministic stage completed: %+v", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	runEvents, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range runEvents {
		if event.Type == journal.EventStageFinished && event.Stage == "observe-config" {
			if event.Error == nil || event.Error.Code != configGenerationMismatchCode {
				t.Fatalf("stage.finished error = %+v, want named %s", event.Error, configGenerationMismatchCode)
			}
			for _, fragment := range []string{rejected, applied, "refusing to read"} {
				if !strings.Contains(event.Error.Message, fragment) {
					t.Errorf("journaled stage error %q does not contain %q", event.Error.Message, fragment)
				}
			}
			return
		}
	}
	t.Fatalf("no observe-config stage.finished in run journal: %+v", runEvents)
}

func TestDeterministicStageConfigDigestFailsClosed(t *testing.T) {
	digest, err := deterministicStageConfigDigest(filepath.Join(t.TempDir(), "missing"))
	if err == nil || digest != "" || !strings.Contains(err.Error(), "digest deterministic-stage config") {
		t.Fatalf("digest missing config: digest=%q err=%v", digest, err)
	}
}

func newConfigReloadFenceRunner(t *testing.T, layout instance.Layout, appliedDigest string) *runner.Runner {
	t.Helper()
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, registrar runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(resolver, nil, registrar)
			if err != nil {
				return nil, err
			}
			shell, err := executor.NewShellExecutor(injector, rec)
			if err != nil {
				return nil, err
			}
			shell.InstanceRoot = layout.Root
			shell.AppliedConfigDigest = appliedDigest
			shell.SelfBin = executable
			shell.ScratchDir = filepath.Join(layout.WorkcopiesDir(), "scratch")
			return shell, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: manager,
		RunsDir:   layout.RunsDir(), ScratchDir: filepath.Join(layout.WorkcopiesDir(), "scratch"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func configReloadFenceMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "reload-fence", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "observe-config",
			Tasks: []apiv1.Task{{
				Name: "observe-config", Type: apiv1.TaskDeterministic, Goal: "observe the applied config",
				Run: &apiv1.DeterministicRun{Command: []string{"goobers", "validate"}, Workspace: apiv1.WorkspaceScratch},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return machine
}
