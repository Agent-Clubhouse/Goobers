package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/testgit"
)

// commitPostconditionRepo creates a git checkout on a branch with one
// committed file, the shape of a writable stage's run-branch worktree.
func commitPostconditionRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	postconditionTestGit(t, dir, "init", "-q", "-b", "run-branch")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	postconditionTestGit(t, dir, "add", "main.go")
	postconditionTestGit(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func postconditionTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)
	out, err := testgit.Command(full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// editWithoutCommit is the #5182 reproduction: the agent edits a tracked file
// and adds a new one, reports success, and exits without committing.
func editWithoutCommit(_ context.Context, req RunRequest) error {
	if err := os.WriteFile(filepath.Join(req.Workspace, "main.go"), []byte("package main\n\nfunc fixed() {}\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(req.Workspace, "fixed_test.go"), []byte("package main\n"), 0o644); err != nil {
		return err
	}
	return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "fixed it"})
}

func newPostconditionExecutor(t *testing.T, adapter Adapter, rec *fakeRecorder) *Executor {
	t.Helper()
	exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return exec
}

func modifyRepositoryEnvelope(workspace string) apiv1.InvocationEnvelope {
	env := testEnvelope(workspace)
	env.PolicyActions = []string{modifyRepositoryPolicyAction}
	return env
}

func TestExecutorSendsUncommittedSuccessBackAsRetryableFailure(t *testing.T) {
	workspace := commitPostconditionRepo(t)
	rec := &fakeRecorder{}
	exec := newPostconditionExecutor(t, &FakeAdapter{Act: editWithoutCommit}, rec)

	result, err := exec.Invoke(context.Background(), modifyRepositoryEnvelope(workspace))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("Status = %q, want failure for a success that left its work uncommitted", result.Status)
	}
	if result.Error == nil || result.Error.Code != ErrorCodeUncommittedChanges || !result.Error.Retryable {
		t.Fatalf("Error = %+v, want retryable %s", result.Error, ErrorCodeUncommittedChanges)
	}
	if !strings.Contains(result.Error.Message, "main.go") || !strings.Contains(result.Error.Message, "fixed_test.go") {
		t.Fatalf("Error.Message = %q, want the uncommitted paths named", result.Error.Message)
	}
	var patch *recordedArtifact
	for i := range rec.artifacts {
		if rec.artifacts[i].name == "implement"+uncommittedDiffArtifactSuffix {
			patch = &rec.artifacts[i]
		}
	}
	if patch == nil {
		t.Fatalf("artifacts = %+v, want the uncommitted diff recorded for recovery", rec.artifacts)
	}
	for _, want := range []string{"func fixed()", "fixed_test.go"} {
		if !strings.Contains(string(patch.data), want) {
			t.Fatalf("uncommitted diff missing %q:\n%s", want, patch.data)
		}
	}
	if strings.Contains(string(patch.data), ".goobers") {
		t.Fatalf("uncommitted diff includes harness-owned scratch:\n%s", patch.data)
	}
	if !resultCarriesArtifact(result, patch.name) {
		t.Fatalf("result artifacts = %+v, want the recovery diff attached", result.Artifacts)
	}
	// Never committed on the agent's behalf, and the agent's index untouched.
	if log := postconditionTestGit(t, workspace, "rev-list", "--count", "HEAD"); strings.TrimSpace(log) != "1" {
		t.Fatalf("commit count = %s, want the harness to create no commit", log)
	}
	if staged := postconditionTestGit(t, workspace, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "" {
		t.Fatalf("staged paths = %q, want the real index untouched", staged)
	}
}

func resultCarriesArtifact(result apiv1.ResultEnvelope, name string) bool {
	for _, artifact := range result.Artifacts {
		if strings.HasSuffix(artifact.Path, name) || artifact.Path == name {
			return true
		}
	}
	return false
}

func TestExecutorAcceptsCommittedSuccess(t *testing.T) {
	workspace := commitPostconditionRepo(t)
	adapter := &FakeAdapter{Act: func(ctx context.Context, req RunRequest) error {
		if err := editWithoutCommit(ctx, req); err != nil {
			return err
		}
		postconditionTestGit(t, req.Workspace, "add", "-A", "--", ".", ":(exclude).goobers")
		postconditionTestGit(t, req.Workspace, "commit", "-q", "-m", "fix")
		return nil
	}}
	result, err := newPostconditionExecutor(t, adapter, &fakeRecorder{}).Invoke(context.Background(), modifyRepositoryEnvelope(workspace))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("Status = %q (%+v), want success for committed work", result.Status, result.Error)
	}
}

func TestExecutorCommitPostconditionScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    func(string) apiv1.InvocationEnvelope
		act    func(context.Context, RunRequest) error
		status apiv1.ResultStatus
	}{
		{
			name:   "stage without modify-repository is not checked",
			env:    func(ws string) apiv1.InvocationEnvelope { return testEnvelope(ws) },
			act:    editWithoutCommit,
			status: apiv1.ResultSuccess,
		},
		{
			name: "no-work verdict is not converted",
			env:  modifyRepositoryEnvelope,
			act: func(_ context.Context, req RunRequest) error {
				if err := os.WriteFile(filepath.Join(req.Workspace, "scratch.txt"), []byte("x"), 0o644); err != nil {
					return err
				}
				return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultNoWork})
			},
			status: apiv1.ResultNoWork,
		},
		{
			// An empty success stays a success here; the reviewer gate's
			// empty-diff guard keeps failing it closed downstream.
			name: "clean tree is left to the empty-diff guard",
			env:  modifyRepositoryEnvelope,
			act: func(_ context.Context, req RunRequest) error {
				return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			},
			status: apiv1.ResultSuccess,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := commitPostconditionRepo(t)
			result, err := newPostconditionExecutor(t, &FakeAdapter{Act: tc.act}, &fakeRecorder{}).Invoke(context.Background(), tc.env(workspace))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if result.Status != tc.status {
				t.Fatalf("Status = %q (%+v), want %q", result.Status, result.Error, tc.status)
			}
		})
	}
}

func TestExecutorCommitPostconditionSkipsNonGitWorkspace(t *testing.T) {
	result, err := newPostconditionExecutor(t, &FakeAdapter{Act: editWithoutCommit}, &fakeRecorder{}).
		Invoke(context.Background(), modifyRepositoryEnvelope(t.TempDir()))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("Status = %q, want a scratch workspace left unchecked", result.Status)
	}
}

// repairingAdapter models a subprocess adapter's bounded repair turn: it
// validates its completion, and on a repairable error runs a second turn
// with the repair prompt before validating again.
type repairingAdapter struct {
	FakeAdapter
	commitOnRepair bool
	repairPrompts  []string
}

func (a *repairingAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	if err := editWithoutCommit(ctx, req); err != nil {
		return Outcome{}, err
	}
	payload, err := readCompletion(req.Workspace, req.CompletionPath)
	err = validateCompletion(req, payload, err)
	if repairableCompletionError(err) {
		a.repairPrompts = append(a.repairPrompts, renderCompletionRepairPrompt(req, err))
		if a.commitOnRepair {
			out, gitErr := testgit.Command("-C", req.Workspace, "-c", "user.name=t", "-c", "user.email=t@example.com",
				"commit", "-q", "-am", "commit my own work").CombinedOutput()
			if gitErr != nil {
				return Outcome{}, errors.New(string(out))
			}
		}
		payload, err = readCompletion(req.Workspace, req.CompletionPath)
		err = validateCompletion(req, payload, err)
	}
	return Outcome{Payload: payload}, err
}

func TestExecutorRepairTurnLetsTheAgentCommitItsOwnWork(t *testing.T) {
	workspace := commitPostconditionRepo(t)
	adapter := &repairingAdapter{commitOnRepair: true}
	result, err := newPostconditionExecutor(t, adapter, &fakeRecorder{}).Invoke(context.Background(), modifyRepositoryEnvelope(workspace))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("Status = %q (%+v), want success once the repair turn committed", result.Status, result.Error)
	}
	if len(adapter.repairPrompts) != 1 {
		t.Fatalf("repair turns = %d, want exactly one", len(adapter.repairPrompts))
	}
	prompt := adapter.repairPrompts[0]
	for _, want := range []string{ErrorCodeUncommittedChanges, "git commit", "main.go"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("repair prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "schema validation") {
		t.Fatalf("repair prompt misdescribes the problem as a schema failure:\n%s", prompt)
	}
}

func TestExecutorUncommittedAfterRepairIsAFailureResultNotAnAdapterFault(t *testing.T) {
	workspace := commitPostconditionRepo(t)
	result, err := newPostconditionExecutor(t, &repairingAdapter{}, &fakeRecorder{}).Invoke(context.Background(), modifyRepositoryEnvelope(workspace))
	if err != nil {
		t.Fatalf("Invoke: %v, want a typed failure result", err)
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != ErrorCodeUncommittedChanges {
		t.Fatalf("result = %q %+v, want %s failure", result.Status, result.Error, ErrorCodeUncommittedChanges)
	}
}

// uncommittedOnceValidator fails the commit postcondition on its first call
// and passes afterwards, standing in for an agent that commits on repair.
func uncommittedOnceValidator() func([]byte) error {
	calls := 0
	return func([]byte) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("%w; uncommitted paths:\n M main.go", ErrUncommittedChanges)
		}
		return nil
	}
}

func claudeUncommittedRequest(workspace string, validate func([]byte) error) RunRequest {
	return RunRequest{
		Mode: ModeInvoke, Envelope: testEnvelope(workspace), Workspace: workspace,
		CompletionPath: DefaultResultPath, Timeout: time.Minute, ValidateCompletion: validate,
	}
}

func writeSuccessCompletion(req ProcessRequest) error {
	return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
}

// The real adapter's repair loop: an uncommitted success gets a commit
// prompt in the same session, and its payload is never treated as invalid.
func TestClaudeAdapterRepairsUncommittedSuccessInSameSession(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &claudeSequenceRunner{
		results: []ProcessResult{{Transcript: []byte(claudeResultStream)}, {Transcript: []byte(claudeResultStream)}},
		acts:    []func(ProcessRequest) error{writeSuccessCompletion, nil},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}
	out, err := adapter.Run(context.Background(), claudeUncommittedRequest(workspace, uncommittedOnceValidator()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.reqs) != 2 {
		t.Fatalf("process calls = %d, want one repair turn", len(runner.reqs))
	}
	if prompt := string(runner.reqs[1].Stdin); !strings.Contains(prompt, "git commit") {
		t.Fatalf("repair prompt = %q, want a commit instruction", prompt)
	}
	if len(out.InvalidCompletionPayload) != 0 {
		t.Fatalf("a schema-valid completion was captured as invalid: %s", out.InvalidCompletionPayload)
	}
}

// A repair turn that fails leaves the original completion and its
// postcondition error standing, so the Executor still reports
// UNCOMMITTED_CHANGES instead of an adapter fault.
func TestClaudeAdapterFailedUncommittedRepairKeepsCompletion(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &claudeSequenceRunner{
		results: []ProcessResult{{Transcript: []byte(claudeResultStream)}, {Transcript: []byte(claudeResultStream)}},
		acts: []func(ProcessRequest) error{writeSuccessCompletion, func(ProcessRequest) error {
			return errors.New("repair session crashed")
		}},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}
	alwaysUncommitted := func([]byte) error { return ErrUncommittedChanges }
	out, err := adapter.Run(context.Background(), claudeUncommittedRequest(workspace, alwaysUncommitted))
	if !errors.Is(err, ErrUncommittedChanges) || errors.Is(err, ErrInvalidCompletion) {
		t.Fatalf("Run error = %v, want only the postcondition error", err)
	}
	if len(out.Payload) == 0 {
		t.Fatal("the original completion was dropped")
	}
}

func TestSettleStripsOnlyThePostconditionError(t *testing.T) {
	c := &commitPostcondition{}
	other := errors.New("read goobers-io receipts")
	joined := errors.Join(fmt.Errorf("%w; paths", ErrUncommittedChanges), other)
	if _, err := c.settle(Outcome{}, joined); !errors.Is(err, other) || errors.Is(err, ErrUncommittedChanges) {
		t.Fatalf("settle(joined) = %v, want only the other failure kept", err)
	}
	if _, err := c.settle(Outcome{}, ErrUncommittedChanges); err != nil {
		t.Fatalf("settle(postcondition only) = %v, want nil", err)
	}
	var unarmed *commitPostcondition
	if _, err := unarmed.settle(Outcome{}, ErrUncommittedChanges); !errors.Is(err, ErrUncommittedChanges) {
		t.Fatalf("an unarmed postcondition must pass errors through, got %v", err)
	}
}

func TestResponseRepairPromptNamesUncommittedChanges(t *testing.T) {
	prompt := renderResponseCompletionRepairPrompt(RunRequest{Mode: ModeInvoke}, ErrUncommittedChanges)
	if !strings.Contains(prompt, "git commit") || strings.Contains(prompt, "schema validation") {
		t.Fatalf("response repair prompt = %q", prompt)
	}
}
