package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

const multiGaggleWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: deploy
spec:
  gaggle: %s
  triggers:
    - type: signal
      signal: release
  readiness:
    maxConcurrentRuns: %d
    maxRunsPerHour: %d
  start: finish
  tasks:
    - name: finish
      type: deterministic
      goal: Complete a daemon dispatch.
      run:
        command: [%q, "-test.run=^$"]
        workspace: scratch
`

type daemonGateStarter struct {
	next    localscheduler.Starter
	release <-chan struct{}

	mu     sync.Mutex
	starts []localscheduler.StartRequest
}

func (s *daemonGateStarter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.mu.Lock()
	s.starts = append(s.starts, req)
	s.mu.Unlock()
	<-s.release
	return s.next.Start(ctx, req)
}

func (s *daemonGateStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.starts)
}

func installSecondDaemonGaggle(t *testing.T, root string) {
	t.Helper()
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config.RunConditions = instance.RunConditions{MaxParallelRuns: 10}
	if err := instance.WriteConfig(layout.ConfigFile(), config); err != nil {
		t.Fatal(err)
	}

	configDir := filepath.Join(root, "config")
	manifestPath := filepath.Join(configDir, "manifest.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	updatedManifest := strings.Replace(string(manifest), "    - example\n", "    - example\n    - beta\n", 1)
	if updatedManifest == string(manifest) {
		t.Fatal("starter manifest did not contain example gaggle")
	}
	if err := os.WriteFile(manifestPath, []byte(updatedManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	exampleGagglePath := filepath.Join(configDir, "gaggles", "example", "gaggle.yaml")
	exampleGaggle, err := os.ReadFile(exampleGagglePath)
	if err != nil {
		t.Fatal(err)
	}
	betaGaggle := strings.ReplaceAll(string(exampleGaggle), "example", "beta")
	betaDir := filepath.Join(configDir, "gaggles", "beta")
	if err := os.MkdirAll(filepath.Join(betaDir, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(betaDir, "gaggle.yaml"), []byte(betaGaggle), 0o644); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	workflows := []struct {
		gaggle        string
		maxConcurrent int
		maxPerHour    int
	}{
		{gaggle: "example", maxConcurrent: 1, maxPerHour: 1},
		{gaggle: "beta", maxConcurrent: 2, maxPerHour: 3},
	}
	for _, workflow := range workflows {
		path := filepath.Join(configDir, "gaggles", workflow.gaggle, "workflows", "default-implement.yaml")
		body := []byte(strings.TrimSpace(
			fmt.Sprintf(multiGaggleWorkflowYAML, workflow.gaggle, workflow.maxConcurrent, workflow.maxPerHour, testBinary),
		) + "\n")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunSelectsGaggleForDuplicateWorkflow(t *testing.T) {
	root := initDeterministicDemo(t)
	installSecondDaemonGaggle(t, root)

	code, stdout, stderr := runArgs(t, "run", "--gaggle", "beta", "deploy", root)
	if code != 0 {
		t.Fatalf("run: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "workflow=deploy gaggle=beta") {
		t.Fatalf("stdout = %q, want beta gaggle", stdout)
	}
	betaRuns, err := os.ReadDir(instance.NewLayout(root).ForGaggle("beta").RunsDir())
	if err != nil {
		t.Fatal(err)
	}
	exampleRuns, err := os.ReadDir(instance.NewLayout(root).ForGaggle("example").RunsDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(betaRuns) != 1 || len(exampleRuns) != 0 {
		t.Fatalf("run counts: beta=%d example=%d, want beta=1 example=0", len(betaRuns), len(exampleRuns))
	}
}

func TestDaemonDispatchesAndDrainsAllManifestGaggles(t *testing.T) {
	root := initDeterministicDemo(t)
	installSecondDaemonGaggle(t, root)
	layout := instance.NewLayout(root)

	var tracked sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), layout, &tracked)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Shutdown(context.Background())

	if len(setup.Runners) != 2 || len(setup.Entries) != 2 {
		t.Fatalf("daemon setup = %d runners and %d workflows, want 2 of each", len(setup.Runners), len(setup.Entries))
	}

	release := make(chan struct{})
	starters := make(map[string]*daemonGateStarter, len(setup.Entries))
	for i := range setup.Entries {
		entry := &setup.Entries[i]
		gated := &daemonGateStarter{next: entry.Starter, release: release}
		entry.Starter = gated
		starters[entry.Gaggle] = gated
	}

	scheduler := newDaemonScheduler(setup)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	if got := scheduler.Signal(context.Background(), "release", now); len(got) != 2 {
		t.Fatalf("first signal dispatched %d runs, want one per gaggle", len(got))
	}
	waitForStarterCount(t, starters["example"], 1)
	waitForStarterCount(t, starters["beta"], 1)

	if got := scheduler.Signal(context.Background(), "release", now.Add(time.Minute)); len(got) != 1 {
		t.Fatalf("second signal dispatched %d runs, want only beta's second concurrency slot", len(got))
	}
	waitForStarterCount(t, starters["beta"], 2)
	if got := starters["example"].count(); got != 1 {
		t.Fatalf("example starts = %d, want maxConcurrentRuns=1", got)
	}

	drained := make(chan bool, 1)
	go func() {
		done := make(chan struct{})
		go func() {
			scheduler.Wait()
			close(done)
		}()
		select {
		case <-done:
			drained <- true
		case <-time.After(10 * time.Second):
			drained <- false
		}
	}()
	select {
	case <-drained:
		t.Fatal("daemon drain completed while runs in both gaggles were still in flight")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if ok := <-drained; !ok {
		t.Fatal("daemon did not drain in-flight runs from every gaggle")
	}

	if got := scheduler.Signal(context.Background(), "release", now.Add(2*time.Minute)); len(got) != 1 {
		t.Fatalf("third signal dispatched %d runs, want only beta within its hourly budget", len(got))
	}
	scheduler.Wait()
	if got := starters["example"].count(); got != 1 {
		t.Fatalf("example starts = %d, want maxRunsPerHour=1", got)
	}
	if got := starters["beta"].count(); got != 3 {
		t.Fatalf("beta starts = %d, want maxRunsPerHour=3", got)
	}

	assertGaggleRunJournals(t, layout.ForGaggle("example"), "example", 1)
	assertGaggleRunJournals(t, layout.ForGaggle("beta"), "beta", 3)
}

func waitForStarterCount(t *testing.T, starter *daemonGateStarter, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if starter.count() >= want {
			return
		}
		time.Sleep(time.Millisecond) // Polling interval for the test starter's synchronized call count.
	}
	t.Fatalf("starter calls = %d, want at least %d", starter.count(), want)
}

func assertGaggleRunJournals(t *testing.T, layout instance.Layout, gaggle string, want int) {
	t.Helper()
	entries, err := os.ReadDir(layout.RunsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Fatalf("%s run journals = %d, want %d", gaggle, len(entries), want)
	}
	for _, entry := range entries {
		reader, err := journal.OpenRead(filepath.Join(layout.RunsDir(), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		identity, err := reader.Identity()
		if err != nil {
			t.Fatal(err)
		}
		if identity.Gaggle != gaggle {
			t.Fatalf("run %s gaggle = %q, want %q", identity.RunID, identity.Gaggle, gaggle)
		}
	}
}
