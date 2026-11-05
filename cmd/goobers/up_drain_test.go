package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type boundaryDrainDeterministic struct {
	mu      sync.Mutex
	calls   []string
	started chan struct{}
	release chan struct{}
}

func (d *boundaryDrainDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.mu.Lock()
	d.calls = append(d.calls, env.TaskID)
	first := len(d.calls) == 1
	d.mu.Unlock()
	if first {
		close(d.started)
		<-d.release
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (d *boundaryDrainDeterministic) called() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func TestLocalDaemonDrainDefaultRemainsUnbounded(t *testing.T) {
	if localDaemonDrainTimeoutDefault != 0 {
		t.Fatalf("local daemon drain default = %s, want 0 (unbounded)", localDaemonDrainTimeoutDefault)
	}
}

func TestDaemonDrainStopsAtStageBoundary(t *testing.T) {
	machine, err := workflow.Compile(workflow.Definition{
		Name: "boundary-drain", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example",
			Start:  "first",
			Tasks: []apiv1.Task{
				{Name: "first", Type: apiv1.TaskDeterministic, Goal: "finish during drain", Run: &apiv1.DeterministicRun{Command: []string{"first"}, Workspace: apiv1.WorkspaceScratch}, Next: "second"},
				{Name: "second", Type: apiv1.TaskDeterministic, Goal: "resume after restart", Run: &apiv1.DeterministicRun{Command: []string{"second"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TerminalComplete},
			},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	executor := &boundaryDrainDeterministic{started: make(chan struct{}), release: make(chan struct{})}
	runRunner, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return executor, nil
		},
		Worktrees:  manager,
		RunsDir:    filepath.Join(root, "runs"),
		ScratchDir: filepath.Join(root, "scratch"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	registry := newDaemonRunnerRegistry()
	var runs sync.WaitGroup
	runs.Add(1)
	startDone := make(chan runner.Result, 1)
	untrack := registry.Track("boundary-drain-run", "boundary-drain", runRunner)
	go func() {
		defer runs.Done()
		defer untrack()
		result, startErr := runRunner.Start(ctx, runner.StartInput{
			RunID:   "boundary-drain-run",
			Machine: machine,
			Gaggle:  "example",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		})
		if startErr != nil {
			t.Errorf("Start: %v", startErr)
		}
		startDone <- result
	}()

	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first stage did not start")
	}
	cancel()
	drained := make(chan daemonDrainResult, 1)
	go func() {
		drained <- drainDaemonRuns(&runs, func() {}, registry, 5*time.Second, nil, io.Discard, nil)
	}()
	select {
	case result := <-drained:
		t.Fatalf("drain returned before the active stage reached its boundary: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	close(executor.release)

	if result := <-drained; result.forced {
		t.Fatalf("boundary drain forced a hard stop: %+v", result)
	}
	start := <-startDone
	if start.Phase != journal.PhaseRunning || start.FinalState != "second" {
		t.Fatalf("Start = %+v, want a running checkpoint before second", start)
	}
	if got := executor.called(); !slices.Equal(got, []string{"boundary-drain-run:first"}) {
		t.Fatalf("stages before restart = %v, want only first", got)
	}

	resumed, err := runRunner.Resume(context.Background(), runner.ResumeInput{
		RunID:   "boundary-drain-run",
		Machine: machine,
	})
	if err != nil || resumed.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume = %+v, %v, want completed", resumed, err)
	}
	if got := executor.called(); !slices.Equal(got, []string{"boundary-drain-run:first", "boundary-drain-run:second"}) {
		t.Fatalf("stages after restart = %v, want first then second", got)
	}
}

func TestDrainDaemonRunsWaitsForLateSchedulerRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var runs sync.WaitGroup
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			drainDaemonRuns(&runs, func() {
				// An admitted dispatch may not enter Start (and register its
				// run) until after shutdown has begun joining the scheduler.
				runs.Add(1)
				go func() {
					<-release
					runs.Done()
				}()
			}, newDaemonRunnerRegistry(), 0, nil, io.Discard, nil)
			close(done)
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Error("drain returned before the late-registered run finished")
		default:
		}
		close(release)
		<-done
	})
}

func TestDrainDaemonRunsReportsProgressUntilCleanCompletion(t *testing.T) {
	oldInterval := drainProgressInterval
	drainProgressInterval = 5 * time.Millisecond
	t.Cleanup(func() { drainProgressInterval = oldInterval })

	registry := newDaemonRunnerRegistry()
	untrack := registry.Track("run-123", "implementation", &runner.Runner{})
	defer untrack()
	release := make(chan struct{})
	time.AfterFunc(20*time.Millisecond, func() {
		untrack()
		close(release)
	})

	var stdout bytes.Buffer
	result := drainDaemonRuns(&sync.WaitGroup{}, func() { <-release }, registry, 0, nil, &stdout, nil)
	if result.forced {
		t.Fatal("clean drain reported forced")
	}
	output := stdout.String()
	if !strings.Contains(output, "implementation/run-123") ||
		!strings.Contains(output, "still draining") ||
		!strings.Contains(output, "send SIGINT/SIGTERM again") {
		t.Fatalf("drain output = %q", output)
	}
}

func TestDrainDaemonRunsTimeoutAndRepeatedSignalShareHardPath(t *testing.T) {
	for _, tt := range []struct {
		name    string
		timeout time.Duration
		force   func() <-chan struct{}
		want    string
	}{
		{name: "timeout", timeout: 5 * time.Millisecond, want: "drain timeout"},
		{name: "repeated signal", force: func() <-chan struct{} {
			ch := make(chan struct{})
			time.AfterFunc(5*time.Millisecond, func() { close(ch) })
			return ch
		}, want: "repeated shutdown signal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			registry := newDaemonRunnerRegistry()
			untrack := registry.Track("run-456", "review", &runner.Runner{})
			defer untrack()
			release := make(chan struct{})
			time.AfterFunc(15*time.Millisecond, func() { close(release) })

			var force <-chan struct{}
			if tt.force != nil {
				force = tt.force()
			}
			var stdout bytes.Buffer
			result := drainDaemonRuns(&sync.WaitGroup{}, func() { <-release }, registry, tt.timeout, force, &stdout, nil)
			if !result.forced || result.terminated != 1 {
				t.Fatalf("result = %+v, want one forced run", result)
			}
			if output := stdout.String(); !strings.Contains(output, tt.want) ||
				!strings.Contains(output, "terminating 1 run(s) mid-stage") ||
				!strings.Contains(output, "resume from their last checkpoints") {
				t.Fatalf("hard-shutdown output = %q", output)
			}
		})
	}
}
