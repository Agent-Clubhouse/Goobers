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

// fakeVerifier records the capability sets it was asked to verify and returns a
// canned result, standing in for internal/toolchain's real host probing (#735).
type fakeVerifier struct {
	err   error
	calls [][]string
}

func (f *fakeVerifier) Verify(_ context.Context, required []string) error {
	f.calls = append(f.calls, required)
	return f.err
}

func newPreflightRunner(t *testing.T, v ToolchainVerifier, detConstructed *bool, byTask map[string]stubTaskResult) *Runner {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			if detConstructed != nil {
				*detConstructed = true
			}
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Worktrees:         wtMgr,
		RunsDir:           filepath.Join(instanceRoot, "runs"),
		ToolchainVerifier: v,
		RepoCloneURL:      func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func preflightStart(t *testing.T, r *Runner, runID string, required []string) (Result, error) {
	t.Helper()
	return r.Start(context.Background(), StartInput{
		RunID:                runID,
		Machine:              taskReservedNextFixtureMachine(t, workflow.TerminalComplete),
		Gaggle:               "acme-web",
		Trigger:              journal.Trigger{Kind: journal.TriggerManual},
		RepoRef:              apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		RequiredCapabilities: required,
	})
}

// TestStartFailsClosedOnToolchainPreflight: a declared toolchain the host does
// not satisfy fails the run closed at a "runtime-preflight" terminal, carrying
// the verifier's diagnostic, before any stage executor is even constructed.
func TestStartFailsClosedOnToolchainPreflight(t *testing.T) {
	v := &fakeVerifier{err: errors.New("runtime preflight failed: dotnet@8 not satisfied on host")}
	var detConstructed bool
	r := newPreflightRunner(t, v, &detConstructed, nil)

	res, err := preflightStart(t, r, "run-preflight-fail", []string{"dotnet@8"})
	if err == nil {
		t.Fatal("expected Start to surface the preflight failure")
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if res.FailureStage != toolchainPreflightState {
		t.Fatalf("FailureStage = %q, want %q", res.FailureStage, toolchainPreflightState)
	}
	if !strings.Contains(res.FailureMessage, "dotnet@8") {
		t.Fatalf("FailureMessage lost the diagnostic: %q", res.FailureMessage)
	}
	if len(v.calls) != 1 || len(v.calls[0]) != 1 || v.calls[0][0] != "dotnet@8" {
		t.Fatalf("verifier calls = %v, want exactly one with [dotnet@8]", v.calls)
	}
	if detConstructed {
		t.Error("a stage executor was constructed despite the preflight failing closed")
	}
}

// TestStartSkipsPreflightWhenNoRequirement: a run declaring no capability never
// invokes the verifier and behaves exactly as before (no behavior change).
func TestStartSkipsPreflightWhenNoRequirement(t *testing.T) {
	v := &fakeVerifier{err: errors.New("must not be called")}
	r := newPreflightRunner(t, v, nil, map[string]stubTaskResult{
		"run-preflight-none:implement": {status: apiv1.ResultSuccess, summary: "done"},
	})

	res, err := preflightStart(t, r, "run-preflight-none", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if len(v.calls) != 0 {
		t.Fatalf("verifier called %d time(s) for a run with no requirement, want 0", len(v.calls))
	}
}

// TestStartProceedsWhenPreflightPasses: a satisfied requirement runs the
// verifier once (with the declared caps) and the run proceeds normally.
func TestStartProceedsWhenPreflightPasses(t *testing.T) {
	v := &fakeVerifier{}
	r := newPreflightRunner(t, v, nil, map[string]stubTaskResult{
		"run-preflight-pass:implement": {status: apiv1.ResultSuccess, summary: "done"},
	})

	res, err := preflightStart(t, r, "run-preflight-pass", []string{"go@1.26", "os=linux"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if len(v.calls) != 1 || len(v.calls[0]) != 2 {
		t.Fatalf("verifier calls = %v, want exactly one with the two declared caps", v.calls)
	}
}
