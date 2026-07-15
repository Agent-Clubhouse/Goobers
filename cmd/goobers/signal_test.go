package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// signalTriggeredWorkflowYAML declares a type=signal trigger instead of
// deterministicWorkflowYAML's backlog-item one, so `goobers signal` has
// something to dispatch.
const signalTriggeredWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: signal
      signal: "deploy"
  start: local-ci
  tasks:
    - name: local-ci
      type: deterministic
      goal: run a no-op local command
      run:
        command: ["true"]
`

func initSignalDemo(t *testing.T) string {
	t.Helper()
	root := initDemo(t)

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(signalTriggeredWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "config", "gaggles", "example", "goobers")); err != nil {
		t.Fatal(err)
	}

	fixtureRepo := newDaemonFixtureRepo(t)
	prev := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }
	t.Cleanup(func() { repoCloneURL = prev })

	return root
}

// TestSignalDispatchesSubscribedWorkflow is #342's CLI-level acceptance:
// `goobers signal <name>` fires every workflow with a matching type=signal
// trigger — TriggerSignal was declared in the schema but compiled and
// dispatched nowhere before this.
func TestSignalDispatchesSubscribedWorkflow(t *testing.T) {
	root := initSignalDemo(t)
	l := instance.NewLayout(root)

	code, stdout, stderr := runArgs(t, "signal", "deploy", root)
	if code != 0 {
		t.Fatalf("signal: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "created run") {
		t.Fatalf("stdout = %q, want a mention of the created run", stdout)
	}

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var sawSignalFire, sawRunStarted bool
	for _, ev := range events {
		if ev.Workflow != "default-implement" {
			continue
		}
		if ev.Type == journal.EventTriggerFired && ev.Reason == "signal" {
			sawSignalFire = true
		}
		if ev.Type == journal.EventRunStarted {
			sawRunStarted = true
		}
	}
	if !sawSignalFire {
		t.Fatalf("expected a trigger.fired(reason=signal) event in the instance journal: %+v", events)
	}
	if !sawRunStarted {
		t.Fatalf("expected a run.started event in the instance journal: %+v", events)
	}
}

// TestSignalUnmatchedNameStillExitsZero proves a signal matching no
// subscribed workflow is a legitimate no-op broadcast, not a usage error —
// distinct from `goobers run <workflow>`'s unknown-workflow exit 1.
func TestSignalUnmatchedNameStillExitsZero(t *testing.T) {
	root := initSignalDemo(t)

	code, stdout, stderr := runArgs(t, "signal", "no-such-signal", root)
	if code != 0 {
		t.Fatalf("signal: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no subscribed workflow") {
		t.Fatalf("stdout = %q, want a mention of no subscribed workflow", stdout)
	}
}

// TestSignalFailsFastWhenLockHeld mirrors TestRunFailsFastWhenLockHeld: a
// live `goobers up` daemon holding the instance lock must make `goobers
// signal` fail fast rather than silently racing scheduler/journal state.
func TestSignalFailsFastWhenLockHeld(t *testing.T) {
	root := initSignalDemo(t)
	l := instance.NewLayout(root)

	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	code, _, stderr := runArgs(t, "signal", "deploy", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "already holds the lock") {
		t.Fatalf("stderr = %q", stderr)
	}
}
