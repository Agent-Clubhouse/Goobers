package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/worktree"
)

// githubWorkflowPermissionStderr is GitHub's real rejection of a push by a
// GitHub App installation that updates a workflow file without the
// `workflows` permission, as reported on #5502.
const githubWorkflowPermissionStderr = "To https://github.com/example-org/example-repo\n" +
	" ! [remote rejected] goobers/implementation/c50b43fe -> goobers/implementation/c50b43fe (refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission)\n" +
	"error: failed to push some refs to 'https://github.com/example-org/example-repo'\n"

// githubWorkflowPermissionTruncatedStderr is the same rejection as it first
// reached a run log on #5502, with the parenthetical truncated mid-path.
const githubWorkflowPermissionTruncatedStderr = " ! [remote rejected] goobers/implementation/aae49600 -> goobers/implementation/aae49600 (refusing to allow a GitHub App to create or update workflow `.github/wo...`)\n" +
	"error: failed to push some refs to 'https://github.com/example-org/example-repo'\n"

// TestClassifyRejectedPush is #5502's classifier table: GitHub's
// workflow-permission refusal (full or truncated) is typed as a
// workflowPermissionPushError, ADO's TF402455 keeps its own type, and plain
// races and auth failures stay untyped.
func TestClassifyRejectedPush(t *testing.T) {
	cases := []struct {
		name         string
		output       string
		wantWorkflow bool
		wantPolicy   bool
		wantRace     bool
	}{
		{"real GitHub workflows-permission rejection", githubWorkflowPermissionStderr, true, false, false},
		{"truncated GitHub workflows-permission rejection", githubWorkflowPermissionTruncatedStderr, true, false, false},
		{"personal access token missing workflow scope", " ! [remote rejected] b -> b (refusing to allow a Personal Access Token to create or update workflow `.github/workflows/ci.yml` without `workflow` scope)\nerror: failed to push some refs", true, false, false},
		{"OAuth App missing workflow scope", " ! [remote rejected] b -> b (refusing to allow an OAuth App to create or update workflow `.github/workflows/ci.yml` without `workflow` scope)\nerror: failed to push some refs", true, false, false},
		{"fragments on separate lines do not match", "remote: refusing to allow a force push\nhint: to create or update workflow files, see docs\nerror: failed to push some refs", false, false, true},
		{"real ADO TF402455 rejection", adoTF402455Stderr, false, true, false},
		{"plain ref race", "! [rejected] main -> main (fetch first)\nerror: failed to push some refs", false, false, true},
		{"non-fast-forward race", "! [rejected] main -> main (non-fast-forward)\nerror: failed to push some refs", false, false, true},
		{"auth failure", "remote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/example-org/example-repo'", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGitHubWorkflowPermissionPush(tc.output); got != tc.wantWorkflow {
				t.Fatalf("isGitHubWorkflowPermissionPush = %v, want %v", got, tc.wantWorkflow)
			}
			err := classifyRejectedPush("b", tc.output, errors.New("exit status 1"))
			var workflowErr *workflowPermissionPushError
			if got := errors.As(err, &workflowErr); got != tc.wantWorkflow {
				t.Fatalf("classifyRejectedPush is workflowPermissionPushError = %v, want %v (err = %v)", got, tc.wantWorkflow, err)
			}
			var policyErr *policyProtectedPushError
			if got := errors.As(err, &policyErr); got != tc.wantPolicy {
				t.Fatalf("classifyRejectedPush is policyProtectedPushError = %v, want %v (err = %v)", got, tc.wantPolicy, err)
			}
			if got := isTerminalPushRejection(err); got != (tc.wantWorkflow || tc.wantPolicy) {
				t.Fatalf("isTerminalPushRejection = %v, want %v", got, tc.wantWorkflow || tc.wantPolicy)
			}
			// A race is only retried when it is not a terminal refusal.
			if got := isPushRaceError(err) && !isTerminalPushRejection(err); got != tc.wantRace {
				t.Fatalf("retried as a ref race = %v, want %v", got, tc.wantRace)
			}
			if !strings.Contains(err.Error(), strings.TrimSpace(tc.output)) {
				t.Fatalf("err = %q, want it to carry git's output verbatim", err)
			}
		})
	}
}

// TestWorkflowPermissionPushErrorMessageIsActionable proves the surfaced
// message names the cause and the remedy, not just git's rejection.
func TestWorkflowPermissionPushErrorMessageIsActionable(t *testing.T) {
	err := classifyRejectedPush("feature", githubWorkflowPermissionStderr, errors.New("exit status 1"))
	msg := err.Error()
	for _, want := range []string{"`workflows`", "`workflow` scope", ".github/workflows/", "grant the GitHub App installation", "push this change manually", "retrying cannot succeed", "refusing to allow a GitHub App"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

// TestClassifyProviderError_WorkflowPermissionPush proves
// classifyProviderError maps a workflowPermissionPushError to its own
// non-retryable provider code, never auth-failed — even wrapped.
func TestClassifyProviderError_WorkflowPermissionPush(t *testing.T) {
	base := &workflowPermissionPushError{branch: "b", err: errors.New(githubWorkflowPermissionStderr)}
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
// isPushRaceError's "failed to push some refs" marker, so before the fix
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
	var workflowErr *workflowPermissionPushError
	if !errors.As(pushErr, &workflowErr) {
		t.Fatalf("pushBranchWithRetry err = %v, want it to be (or wrap) a workflowPermissionPushError", pushErr)
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
// refusal surfaces as a workflowPermissionPushError, never a lease race.
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
	var workflowErr *workflowPermissionPushError
	if !errors.As(err, &workflowErr) {
		t.Fatalf("forcePushWithLeaseWithAuth err = %v, want it to be (or wrap) a workflowPermissionPushError", err)
	}
}
