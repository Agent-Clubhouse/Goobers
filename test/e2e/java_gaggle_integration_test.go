//go:build integration

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func javaServiceMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	gaggle := loadYAML[apiv1.Gaggle](t, filepath.Join(javaGaggleDir, "gaggle.yaml"))
	wf := loadYAML[apiv1.Workflow](t, filepath.Join(javaGaggleDir, "workflows", "java-implementation.yaml"))
	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"java-implementer", "java-reviewer"} {
		goober := loadYAML[apiv1.Goober](t, filepath.Join(javaGaggleDir, "goobers", name, "goober.yaml"))
		goobers[goober.Name] = goober.Spec
	}

	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}, Workflows: []apiv1.Workflow{wf}}
	instance.ApplyGaggleCICommand(set)
	wf = set.Workflows[0]
	wantCommand := []string{"mvn", "-B", "-q", "verify"}
	if got := localCICommand(wf); fmt.Sprint(got) != fmt.Sprint(wantCommand) {
		t.Fatalf("local-ci command = %v, want %v after gaggle ciCommand resolution", got, wantCommand)
	}

	machine, err := workflow.Compile(
		workflow.Definition{Name: wf.Name, Version: 1, Spec: wf.Spec},
		workflow.WithGoobers(goobers),
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile java-service machine: %v", err)
	}
	return machine
}

func newJavaFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	runSkeletonGit(t, work, "init", "-b", "main")
	runSkeletonGit(t, work, "config", "user.email", "fixture@test")
	runSkeletonGit(t, work, "config", "user.name", "fixture")
	if err := os.CopyFS(work, os.DirFS("testdata/javaservice")); err != nil {
		t.Fatalf("copy Java fixture: %v", err)
	}
	runSkeletonGit(t, work, "add", "-A")
	runSkeletonGit(t, work, "commit", "-m", "seed Java service")
	bare := filepath.Join(t.TempDir(), "java-service.git")
	runSkeletonGit(t, filepath.Dir(bare), "clone", "--bare", work, bare)
	return bare
}

func TestIntegrationJavaServiceGaggleRunsLocalCIGreen(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_JAVA_E2E")
	testdep.Require(t, "java", "mvn")

	instanceRoot := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	runner := newPolyglotGaggleRunner(t, manager, newJavaFixtureRepo(t), runsDir, "java-implementer")

	const runID = "run-java-gaggle-1"
	result, err := runner.Start(context.Background(), skeletonStartInput(runID, javaServiceMachine(t)))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q (%s: %s), want completed", result.Phase, result.FailureStage, result.FailureMessage)
	}

	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == "local-ci" {
			if event.Status != string(apiv1.ResultSuccess) {
				t.Fatalf("local-ci finished %q, want success", event.Status)
			}
			return
		}
	}
	t.Fatal("no local-ci stage.finished event")
}
