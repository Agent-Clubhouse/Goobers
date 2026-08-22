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
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := fmt.Sprintf(
				`if [ -f %q ]; then exit 0; fi; printf '%%s ' "$$" > %q; %q -test.run=^TestUpDrainEscapedSessionProcess$ -- %q >/dev/null 2>&1 & wait`,
				pidsPath, pidsPath, executable, pidsPath,
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
			result := drainDaemonRuns(&runs, func() {}, registry, tt.timeout, force, &stdout, nil)
			if !result.forced || result.terminated != 1 {
				t.Fatalf("drain result = %+v, want one forced run", result)
			}
			if start := <-startDone; start.Phase != journal.PhaseRunning {
				t.Fatalf("hard-stopped run phase = %s, want running", start.Phase)
			}
			waitForProcessGroupGone(t, stagePID)
			// The escaped child called Setsid (TestUpDrainEscapedSessionProcess),
			// detaching into its own session/process group, so its exit is not
			// synchronized with stagePID's process-group teardown above: the
			// kernel can take a little longer to reap it, especially under the
			// heavier scheduling contention of a loaded CI shard. Poll with the
			// same bounded retry as waitForProcessGroupGone rather than a single
			// snapshot check.
			waitForProcessGone(t, childPID)

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

func TestUpDrainEscapedSessionProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(os.Args[len(os.Args)-1], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
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
				time.Sleep(10 * time.Millisecond)
				continue
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

// waitForProcessGroupGone polls until processGroupID's process group is
// gone. kill(-pgid, 0) alone is zombie-blind: it keeps succeeding for a
// group whose leader has exited but not been reaped, since a zombie still
// occupies its process-table slot. The tree this test kills is a session
// leader (its pgid equals its own pid, see proc_unix.go's configure), so on
// linux a "Z" state for that pid is read as the group being gone too (#3395)
// — in a container whose pid 1 does not reap orphans, this is the difference
// between the group reading as gone and hanging forever after the kill path
// actually succeeded. isZombie is a no-op on non-linux unix.
func waitForProcessGroupGone(t *testing.T, processGroupID int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-processGroupID, 0)
		if errors.Is(err, syscall.ESRCH) || isZombie(processGroupID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process group %d still exists after hard shutdown", processGroupID)
}

// waitForProcessGone polls until pid is gone. See waitForProcessGroupGone:
// kill(pid, 0) succeeds for a zombie, so on linux a "Z" /proc/<pid>/stat
// state also counts as gone (#3395).
func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) || isZombie(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists after hard shutdown", pid)
}
