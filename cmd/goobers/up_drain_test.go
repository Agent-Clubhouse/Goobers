package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/runner"
)

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
