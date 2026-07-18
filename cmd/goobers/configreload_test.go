package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestUpReloadsValidConfigAndRejectsInvalidEdit(t *testing.T) {
	previousReloadInterval := configReloadInterval
	previousDelegationInterval := delegationSweepInterval
	configReloadInterval = 20 * time.Millisecond
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		configReloadInterval = previousReloadInterval
		delegationSweepInterval = previousDelegationInterval
	})

	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	workflowPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runUpContext(ctx, []string{"--quiet", root}, started, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-daemonDone:
			if code != 0 {
				t.Errorf("daemon exit code = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-started.started:
	case code := <-daemonDone:
		t.Fatalf("daemon exited before startup with code %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	reloadedWorkflow := strings.Replace(deterministicWorkflowYAML, "name: default-implement", "name: reloaded-implement", 1)
	if err := os.WriteFile(workflowPath, []byte(reloadedWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded := waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 1)
	oldDigest, oldOK := reloaded.Runner["oldDigest"].(string)
	newDigest, newOK := reloaded.Runner["newDigest"].(string)
	if !oldOK || !newOK || oldDigest == "" || newDigest == "" || oldDigest == newDigest {
		t.Fatalf("config.reloaded digests = %+v, want distinct old/new digests", reloaded.Runner)
	}

	code, stdout, stderr := runArgs(t, "run", "reloaded-implement", root)
	if code != 0 {
		t.Fatalf("run reloaded workflow: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	if err := os.WriteFile(workflowPath, []byte("kind: Workflow\nmetadata: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rejected := waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloadRejected, 1)
	if rejected.Error == nil || rejected.Error.Code != "config_reload_rejected" || rejected.Error.Message == "" {
		t.Fatalf("config.reload.rejected error = %+v", rejected.Error)
	}

	code, stdout, stderr = runArgs(t, "run", "reloaded-implement", root)
	if code != 0 {
		t.Fatalf("last-known-good workflow unavailable after rejected edit: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestBuildSchedulerSetupRejectsConfigChangedDuringStartup(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	workflowPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")

	previousLoader := loadConfigDirectory
	loadConfigDirectory = func(dir string) (*instance.ConfigSet, *validate.Report, error) {
		set, report, err := instance.LoadConfigDir(dir)
		if err != nil {
			return set, report, err
		}
		changed := strings.Replace(deterministicWorkflowYAML, "name: default-implement", "name: changed-during-startup", 1)
		if err := os.WriteFile(workflowPath, []byte(changed), 0o644); err != nil {
			return nil, report, err
		}
		return set, report, nil
	}
	t.Cleanup(func() { loadConfigDirectory = previousLoader })

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), layout, &wg)
	if setup != nil {
		setup.Shutdown(context.Background())
	}
	if err == nil || !strings.Contains(err.Error(), "config directory changed during daemon setup") {
		t.Fatalf("buildSchedulerSetup error = %v, want changed-during-setup refusal", err)
	}
}

func waitForConfigEvent(t *testing.T, schedulerDir string, eventType journal.EventType, count int) journal.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err := journal.ReadInstanceLog(schedulerDir)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, event := range events {
			if event.Type != eventType {
				continue
			}
			seen++
			if seen == count {
				return event
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s event %d", eventType, count)
	return journal.Event{}
}
