package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/pushrejection"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/worktree"
)

// githubWorkflowPermissionStderr is GitHub's real rejection of a push by a
// GitHub App installation that updates a workflow file without the
// `workflows` permission, as reported on #5502.
const githubWorkflowPermissionStderr = "To https://github.com/example-org/example-repo\n" +
	" ! [remote rejected] goobers/implementation/c50b43fe -> goobers/implementation/c50b43fe (refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission)\n" +
	"error: failed to push some refs to 'https://github.com/example-org/example-repo'\n"

// TestClassifyProviderError_WorkflowPermissionPush proves
// classifyProviderError maps a pushrejection.WorkflowPermissionError to its own
// non-retryable provider code, never auth-failed — even wrapped.
func TestClassifyProviderError_WorkflowPermissionPush(t *testing.T) {
	base := &pushrejection.WorkflowPermissionError{Branch: "b", Err: errors.New(githubWorkflowPermissionStderr)}
	for name, err := range map[string]error{
		"direct":  base,
		"wrapped": fmt.Errorf("force-push rebased PR #1 branch %q: %w", "b", base),
	} {
		t.Run(name, func(t *testing.T) {
			code, retryable, extra := classifyProviderError(err)
			if code != errorCodeWorkflowPermissionDenied {
				t.Fatalf("code = %q, want %q", code, errorCodeWorkflowPermissionDenied)
			}
			if class := telemetry.ClassifyError(code); class != telemetry.ErrorClassProvider {
				t.Fatalf("telemetry class of %q = %q, want %q", code, class, telemetry.ErrorClassProvider)
			}
			if retryable {
				t.Fatal("retryable = true, want false")
			}
			if extra != nil {
				t.Fatalf("extra = %v, want nil", extra)
			}
		})
	}
}

// TestPushBranchWithRetrySingleAttemptOnWorkflowPermissionPush is #5502's
// acceptance case against a real bare origin whose pre-receive hook rejects
// every push with GitHub's workflow-permission text. The rejection carries
// pushrejection.IsRace's "failed to push some refs" marker, so before the fix
// push-branch warned "rejected as a ref race" and tried to rebase onto a
// remote branch that was never created. Asserts exactly one push, no
// ref-race warning, and the typed error.
func TestPushBranchWithRetrySingleAttemptOnWorkflowPermissionPush(t *testing.T) {
	const branch = "goobers/implementation/run-5502"
	origin := initBareOrigin(t)

	counterFile := filepath.Join(t.TempDir(), "attempts")
	hookScript := "#!/bin/sh\n" +
		"echo x >> \"" + counterFile + "\"\n" +
		"echo 'refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission' 1>&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(origin, "hooks", "pre-receive"), []byte(hookScript), 0o755); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-5502", BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	if err := os.WriteFile(filepath.Join(wt.Path, "change.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}
	runGitT(t, wt.Path, "add", "change.txt")
	runGitT(t, wt.Path, "commit", "-m", "implement")

	var stderr bytes.Buffer
	pushErr := pushBranchWithRetry(wt.Path, branch, nil, &stderr)
	var workflowErr *pushrejection.WorkflowPermissionError
	if !errors.As(pushErr, &workflowErr) {
		t.Fatalf("pushBranchWithRetry err = %v, want it to be (or wrap) a pushrejection.WorkflowPermissionError", pushErr)
	}
	attempts, readErr := os.ReadFile(counterFile)
	if readErr != nil {
		t.Fatalf("read attempts counter: %v", readErr)
	}
	if got := strings.Count(string(attempts), "x"); got != 1 {
		t.Fatalf("pre-receive hook invoked %d times, want exactly 1", got)
	}
	if strings.Contains(stderr.String(), "ref race") || strings.Contains(stderr.String(), "rebase onto remote") {
		t.Fatalf("stderr = %q, want no ref-race retry or rebase warning", stderr.String())
	}
}

// TestForcePushWithLeaseClassifiesWorkflowPermissionRejection is #5502's
// acceptance for rebase-pr / pr-remediation's force-push: the same GitHub
// refusal surfaces as a pushrejection.WorkflowPermissionError, never a lease race.
func TestForcePushWithLeaseClassifiesWorkflowPermissionRejection(t *testing.T) {
	const prBranch = "goobers/impl/run-5502-force"
	origin, headSHA, _ := initPRBranchOrigin(t, prBranch)

	hookScript := "#!/bin/sh\n" +
		"echo 'refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission' 1>&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(origin, "hooks", "pre-receive"), []byte(hookScript), 0o755); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-5502-force", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-5502-force",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	if _, err := checkoutExistingBranchWithAuth(t.Context(), wt.Path, prBranch, tokenGitAuthEnvironment("test-token")); err != nil {
		t.Fatalf("checkoutExistingBranch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "rebased.txt"), []byte("rebased locally\n"), 0o644); err != nil {
		t.Fatalf("write rebased file: %v", err)
	}
	runGitT(t, wt.Path, "add", "rebased.txt")
	runGitT(t, wt.Path, "commit", "-m", "locally rebased commit")

	err = forcePushWithLeaseWithAuth(t.Context(), wt.Path, prBranch, headSHA, tokenGitAuthEnvironment("test-token"))
	var workflowErr *pushrejection.WorkflowPermissionError
	if !errors.As(err, &workflowErr) {
		t.Fatalf("forcePushWithLeaseWithAuth err = %v, want it to be (or wrap) a pushrejection.WorkflowPermissionError", err)
	}
}
