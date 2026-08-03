//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func TestDrainDaemonRunsForceKillsProcessGroupAndResumesCheckpoint(t *testing.T) {
	for _, tt := range []struct {
		name    string
		timeout time.Duration
		force   func() <-chan struct{}
	}{
		{name: "timeout", timeout: 50 * time.Millisecond},
		{name: "repeated signal", force: func() <-chan struct{} {
			force := make(chan struct{})
			time.AfterFunc(50*time.Millisecond, func() { close(force) })
			return force
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			runsDir := filepath.Join(root, "runs")
			manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
			if err != nil {
				t.Fatal(err)
			}
			pidsPath := filepath.Join(root, "stage.pids")
			command := fmt.Sprintf(
				`if [ -f %q ]; then exit 0; fi; sleep 60 & child=$!; printf '%%s %%s\n' "$$" "$child" > %q; wait`,
				pidsPath, pidsPath,
			)
			machine, err := workflow.Compile(workflow.Definition{
				Name: "implementation", Version: 1,
				Spec: apiv1.WorkflowSpec{
					Gaggle: "example",
					Start:  "implement",
					Tasks: []apiv1.Task{{
						Name: "implement", Type: apiv1.TaskDeterministic, Goal: "run until shutdown",
						Run:   &apiv1.DeterministicRun{Command: []string{"sh", "-c", command}, Workspace: apiv1.WorkspaceScratch},
						Next:  workflow.TerminalComplete,
						Retry: &apiv1.RetryPolicy{MaxAttempts: 2},
					}},
				},
			}, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatal(err)
			}
			runRunner, err := runner.New(runner.Config{
				NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
					resolver, resolveErr := credentials.NewResolver(nil)
					if resolveErr != nil {
						return nil, resolveErr
					}
					injector, injectErr := credentials.NewInjector(resolver, nil, reg)
					if injectErr != nil {
						return nil, injectErr
					}
					return executor.NewShellExecutor(injector, rec)
				},
				Worktrees:  manager,
				ScratchDir: filepath.Join(root, "scratch"),
				RunsDir:    runsDir,
			})
			if err != nil {
				t.Fatal(err)
			}

			const runID = "hard-shutdown-process-tree"
			registry := newDaemonRunnerRegistry()
			var runs sync.WaitGroup
			runs.Add(1)
			startDone := make(chan runner.Result, 1)
			untrack := registry.Track(runID, "implementation", runRunner)
			go func() {
				defer runs.Done()
				defer untrack()
				result, startErr := runRunner.Start(context.Background(), runner.StartInput{
					RunID:   runID,
					Machine: machine,
					Gaggle:  "example",
					Trigger: journal.Trigger{Kind: journal.TriggerManual},
				})
				if startErr != nil {
					t.Errorf("Start: %v", startErr)
				}
				startDone <- result
			}()

			stagePID, childPID := waitForStagePIDs(t, pidsPath)
			var force <-chan struct{}
			if tt.force != nil {
				force = tt.force()
			}
			var stdout bytes.Buffer
			result := drainDaemonRuns(&runs, func() {}, registry, tt.timeout, force, &stdout)
			if !result.forced || result.terminated != 1 {
				t.Fatalf("drain result = %+v, want one forced run", result)
			}
			if start := <-startDone; start.Phase != journal.PhaseRunning {
				t.Fatalf("hard-stopped run phase = %s, want running", start.Phase)
			}
			waitForProcessGroupGone(t, stagePID)
			if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("child process %d still exists after hard shutdown: %v", childPID, err)
			}

			resumed, err := runRunner.Resume(context.Background(), runner.ResumeInput{
				RunID:   runID,
				Machine: machine,
			})
			if err != nil || resumed.Phase != journal.PhaseCompleted {
				t.Fatalf("Resume = %+v, %v, want completed", resumed, err)
			}
		})
	}
}

func waitForStagePIDs(t *testing.T, path string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) != 2 {
				t.Fatalf("stage pid marker = %q, want shell and child pid", data)
			}
			stagePID, stageErr := strconv.Atoi(fields[0])
			childPID, childErr := strconv.Atoi(fields[1])
			if stageErr != nil || childErr != nil {
				t.Fatalf("parse stage pid marker %q: %v, %v", data, stageErr, childErr)
			}
			return stagePID, childPID
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for stage process tree")
	return 0, 0
}

func waitForProcessGroupGone(t *testing.T, processGroupID int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-processGroupID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process group %d still exists after hard shutdown", processGroupID)
}
