package runner

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// localCIFixtureMachine compiles a single-task machine whose deterministic
// task is named "local-ci" (the well-known stage name #1380's preflight keys
// on) and runs command.
func localCIFixtureMachine(t *testing.T, command []string) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    localCIStageName,
		Tasks: []apiv1.Task{
			{
				Name: localCIStageName, Type: apiv1.TaskDeterministic, Goal: "run ci",
				Run:  &apiv1.DeterministicRun{Command: command},
				Next: workflow.TerminalComplete,
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "local-ci-fixture", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile local-ci fixture machine: %v", err)
	}
	return m
}

func newCIPreflightRunner(t *testing.T, lookPath func(string) (string, error), byTask map[string]stubTaskResult) *Runner {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      filepath.Join(instanceRoot, "runs"),
		LookPathFunc: lookPath,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func ciPreflightStart(t *testing.T, r *Runner, runID string, machine *workflow.Machine) (Result, error) {
	t.Helper()
	return r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
}

// TestStartFailsClosedOnCICommandPreflight: a local-ci stage whose configured
// ciCommand executable cannot be resolved on PATH fails the run closed at a
// "ci-command-preflight" terminal — before the stage executor runs at all —
// naming the missing executable, instead of looping through repeated stage
// attempts/remediation cycles an environment gap can never fix (#1380).
func TestStartFailsClosedOnCICommandPreflight(t *testing.T) {
	var lookedUp []string
	lookPath := func(name string) (string, error) {
		lookedUp = append(lookedUp, name)
		return "", errors.New("exec: \"make\": executable file not found in $PATH")
	}
	r := newCIPreflightRunner(t, lookPath, nil)

	res, err := ciPreflightStart(t, r, "run-ci-preflight-fail", localCIFixtureMachine(t, []string{"make", "ci"}))
	if err == nil {
		t.Fatal("expected Start to surface the preflight failure")
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if res.FailureStage != ciCommandPreflightState {
		t.Fatalf("FailureStage = %q, want %q", res.FailureStage, ciCommandPreflightState)
	}
	if !strings.Contains(res.FailureMessage, "make") {
		t.Fatalf("FailureMessage lost the executable name: %q", res.FailureMessage)
	}
	if len(lookedUp) != 1 || lookedUp[0] != "make" {
		t.Fatalf("lookPath calls = %v, want exactly one with \"make\"", lookedUp)
	}
}

// TestStartProceedsWhenCICommandResolves: a resolvable ciCommand executable
// runs the preflight once and the run proceeds normally.
func TestStartProceedsWhenCICommandResolves(t *testing.T) {
	lookPath := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	r := newCIPreflightRunner(t, lookPath, map[string]stubTaskResult{
		"run-ci-preflight-pass:local-ci": {status: apiv1.ResultSuccess, summary: "done"},
	})

	res, err := ciPreflightStart(t, r, "run-ci-preflight-pass", localCIFixtureMachine(t, []string{"make", "ci"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
}

// TestStartSkipsCICommandPreflightWhenNoLocalCIStage: a workflow with no
// local-ci stage has nothing to preflight and never calls lookPath — no
// behavior change for a workflow that doesn't run a local CI-equivalent.
func TestStartSkipsCICommandPreflightWhenNoLocalCIStage(t *testing.T) {
	lookPath := func(string) (string, error) {
		t.Fatal("lookPath must not be called for a workflow with no local-ci stage")
		return "", nil
	}
	r := newCIPreflightRunner(t, lookPath, map[string]stubTaskResult{
		"run-ci-preflight-none:implement": {status: apiv1.ResultSuccess, summary: "done"},
	})

	res, err := ciPreflightStart(t, r, "run-ci-preflight-none", taskReservedNextFixtureMachine(t, workflow.TerminalComplete))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
}

// TestCICommandExecutable pins the pure lookup helper in isolation: it names
// the local-ci stage's Command[0], or "" when there's no such stage or it
// carries no command.
func TestCICommandExecutable(t *testing.T) {
	if got := ciCommandExecutable(localCIFixtureMachine(t, []string{"go", "test", "./..."})); got != "go" {
		t.Errorf("ciCommandExecutable = %q, want %q", got, "go")
	}
	if got := ciCommandExecutable(taskReservedNextFixtureMachine(t, workflow.TerminalComplete)); got != "" {
		t.Errorf("ciCommandExecutable with no local-ci stage = %q, want empty", got)
	}
}

// TestCheckCICommandPreflight pins the pure preflight check: a path-separator
// name is tried directly (matching exec.LookPath's own documented behavior,
// and exactly what exec.Command does when the stage actually runs), a
// resolvable name passes, and an unresolvable name fails with a message
// naming both the executable and the underlying lookup error.
func TestCheckCICommandPreflight(t *testing.T) {
	t.Run("no local-ci stage is a no-op", func(t *testing.T) {
		lookPath := func(string) (string, error) {
			t.Fatal("lookPath must not be called")
			return "", nil
		}
		if err := checkCICommandPreflight(lookPath, taskReservedNextFixtureMachine(t, workflow.TerminalComplete)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("resolvable executable passes", func(t *testing.T) {
		lookPath := func(name string) (string, error) { return "/usr/bin/" + name, nil }
		if err := checkCICommandPreflight(lookPath, localCIFixtureMachine(t, []string{"go", "test", "./..."})); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unresolvable executable fails naming it", func(t *testing.T) {
		lookPath := func(string) (string, error) {
			return "", errors.New("executable file not found in $PATH")
		}
		err := checkCICommandPreflight(lookPath, localCIFixtureMachine(t, []string{"make", "ci"}))
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "make") || !strings.Contains(err.Error(), "not found in $PATH") {
			t.Errorf("error = %q, want it to name the executable and the underlying lookup failure", err.Error())
		}
	})

	t.Run("a relative-path command is tried directly, not skipped", func(t *testing.T) {
		var lookedUp string
		lookPath := func(name string) (string, error) {
			lookedUp = name
			return "", errors.New("no such file or directory")
		}
		err := checkCICommandPreflight(lookPath, localCIFixtureMachine(t, []string{"./scripts/ci.sh"}))
		if err == nil {
			t.Fatal("expected an error")
		}
		if lookedUp != "./scripts/ci.sh" {
			t.Errorf("lookPath called with %q, want the relative path passed through unchanged", lookedUp)
		}
	})
}
