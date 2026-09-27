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
	applied, err := deterministicStageConfigDigest(layout.ConfigDir(), "example")
	if err != nil {
		t.Fatal(err)
	}
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
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
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(executor.AppliedConfigDigestEnvVar, applied)
	if code := run([]string{"version"}, &stdout, &stderr); code != 1 {
		t.Fatalf("fenced stage exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	rejectedScoped, err := deterministicStageConfigDigest(layout.ConfigDir(), "example")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{configGenerationMismatchCode, rejectedScoped, applied, "refusing to read"} {
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
		RunID: runID, Machine: configReloadFenceMachine(t, root), Gaggle: "example",
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
			for _, fragment := range []string{rejectedScoped, applied, "refusing to read"} {
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
	digest, err := deterministicStageConfigDigest(filepath.Join(t.TempDir(), "missing"), "example")
	if err == nil || digest != "" || !strings.Contains(err.Error(), "digest deterministic-stage config") {
		t.Fatalf("digest missing config: digest=%q err=%v", digest, err)
	}
}

func TestDeterministicStageConfigDigestIgnoresSiblingGaggleReload(t *testing.T) {
	root := initDeterministicDemo(t)
	configDir := instance.NewLayout(root).ConfigDir()
	siblingPath := addConfigReloadFenceSiblingGaggle(t, configDir)

	applied, err := deterministicStageConfigDigest(configDir, "example")
	if err != nil {
		t.Fatal(err)
	}
	r := newConfigReloadFenceRunner(t, instance.NewLayout(root), applied)
	wholeBefore, err := configDirectoryDigest(configDir)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := os.ReadFile(siblingPath)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := strings.Replace(string(sibling), "spec:\n", "spec:\n  displayName: Reloaded\n", 1)
	if err := os.WriteFile(siblingPath, []byte(reloaded), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, report, err := instance.LoadConfigDir(configDir); err != nil {
		t.Fatalf("sibling gaggle reload is invalid: %v (%+v)", err, report)
	}
	wholeAfter, err := configDirectoryDigest(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if wholeAfter == wholeBefore {
		t.Fatal("sibling gaggle edit did not move the instance-wide reload digest")
	}

	t.Setenv(executor.InstanceRootEnvVar, root)
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(executor.AppliedConfigDigestEnvVar, applied)
	if err := enforceAppliedStageConfig(); err != nil {
		t.Fatalf("sibling gaggle reload fenced active run: %v", err)
	}

	result, err := r.Start(context.Background(), runner.StartInput{
		RunID: "sibling-gaggle-reload", Machine: configReloadFenceMachine(t, root), Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("active run did not complete after sibling gaggle reload: %+v", result)
	}

	relevantPath := filepath.Join(configDir, "gaggles", "example", "workflows", "default-implement.yaml")
	relevant, err := os.ReadFile(relevantPath)
	if err != nil {
		t.Fatal(err)
	}
	unapplied := strings.Replace(string(relevant), "spec:\n", "spec:\n  displayName: Unapplied\n", 1)
	if err := os.WriteFile(relevantPath, []byte(unapplied), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := enforceAppliedStageConfig(); err == nil {
		t.Fatal("unapplied edit to the active run's own gaggle was not fenced")
	}
}

func addConfigReloadFenceSiblingGaggle(t *testing.T, configDir string) string {
	t.Helper()
	source := filepath.Join(configDir, "gaggles", "example")
	sibling := filepath.Join(configDir, "gaggles", "other")
	if err := os.CopyFS(sibling, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(sibling, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, []byte(strings.ReplaceAll(string(content), "example", "other")), 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(configDir, "manifest.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(manifest), "    - example\n", "    - example\n    - other\n", 1)
	if updated == string(manifest) {
		t.Fatal("demo manifest did not list example gaggle")
	}
	if err := os.WriteFile(manifestPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, report, err := instance.LoadConfigDir(configDir); err != nil {
		t.Fatalf("sibling gaggle fixture is invalid: %v (%+v)", err, report)
	}
	return filepath.Join(sibling, "workflows", "default-implement.yaml")
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

func configReloadFenceMachine(t *testing.T, root string) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "reload-fence", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "observe-config",
			Tasks: []apiv1.Task{{
				Name: "observe-config", Type: apiv1.TaskDeterministic, Goal: "observe the applied config",
				Run: &apiv1.DeterministicRun{Command: []string{"goobers", "validate", root}, Workspace: apiv1.WorkspaceScratch},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return machine
}
