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
      next: ready
  gates:
    - name: ready
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: ""
        fail: "@abort"
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
	if !strings.Contains(stderr, "stage local-ci started") || !strings.Contains(stderr, "stage local-ci finished") {
		t.Fatalf("signal stderr missing stage transitions: %q", stderr)
	}
	if !strings.Contains(stderr, "paused at gate ready") {
		t.Fatalf("signal stderr missing gate pause: %q", stderr)
	}
	if strings.Contains(stdout, "stage local-ci") {
		t.Fatalf("stage progress leaked to stdout: %q", stdout)
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

func TestSignalMonitorsAllRunsConcurrently(t *testing.T) {
	root := initSignalDemo(t)
	configPath := filepath.Join(root, "instance.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config = []byte(strings.Replace(string(config), "maxParallelRuns: 1", "maxParallelRuns: 2", 1))
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatal(err)
	}
	workflowsDir := filepath.Join(root, "config", "gaggles", "example", "workflows")
	slow := strings.ReplaceAll(signalTriggeredWorkflowYAML,
		"name: default-implement", "name: a-slow")
	slow = strings.ReplaceAll(slow,
		"local-ci", "slow")
	slow = strings.ReplaceAll(slow,
		`command: ["true"]`, `command: ["sh", "-c", "sleep 1"]`)
	fast := strings.ReplaceAll(signalTriggeredWorkflowYAML,
		"name: default-implement", "name: z-fast")
	fast = strings.ReplaceAll(fast,
		"local-ci", "fast")
	if err := os.WriteFile(filepath.Join(workflowsDir, "a-slow.yaml"), []byte(slow), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowsDir, "z-fast.yaml"), []byte(fast), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workflowsDir, "default-implement.yaml")); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "signal", "deploy", root)
	if code != 0 {
		t.Fatalf("signal: code = %d, stderr = %q", code, stderr)
	}
	fastStarted := strings.Index(stderr, "stage fast started")
	slowFinished := strings.Index(stderr, "stage slow finished")
	if fastStarted < 0 || slowFinished < 0 || fastStarted > slowFinished {
		t.Fatalf("later run was not monitored while the first run was active: %q", stderr)
	}

	var created, finished []string
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "created" && fields[1] == "run" {
			created = append(created, fields[2])
		}
		if len(fields) >= 2 && fields[0] == "finished:" {
			finished = append(finished, strings.TrimPrefix(fields[1], "run="))
		}
	}
	if len(created) != 2 || len(finished) != 2 ||
		created[0] != finished[0] || created[1] != finished[1] {
		t.Fatalf("stdout run order changed: created=%v finished=%v, stdout=%q", created, finished, stdout)
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
