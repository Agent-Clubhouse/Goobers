package main

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/goobers/goobers/internal/telemetry"
)

// buildFailedHandler exempts an infra fault from the failure-streak circuit
// breaker, and its own comment names git among them (#3361/#3364). The
// exemption is keyed on telemetry.ClassifyError(code).InfraFault(), and until
// #4106 the only producer of an infra-git code was provisionFailureCode — a
// stage's OWN git subprocess failed with classifyProviderError's fallback,
// provider_error, which is not an infra fault.
//
// MEASURED live 2026-09-01: three pr-remediation runs hit #4103 in
// gather-pr-context, each terminating
//
//	run_failed  provider_error: checkout PR #3894's branch "...":
//	            exit status 128: fatal: detected dubious ownership in
//	            repository at '/workspace'
//
// and the third parked #3894 goobers:needs-human, which
// filterRemediationPullRequests treats as a permanent exclusion from the
// remediation lane. #3908 was parked identically. Both are app/goobersbot PRs
// with 22/22 green checks.
func TestStageGitFailureClassifiesAsInfraNotProvider(t *testing.T) {
	t.Run("the live failure is an infra fault, so the breaker skips it", func(t *testing.T) {
		cmd := workspaceGitAuthCommand("/workspace", "tok", "fetch", "https://example.test/r", "refs/heads/b")
		cmd.Path = "/nonexistent/git"
		_, gitErr := workspaceGitCombinedOutput(cmd)
		if gitErr == nil {
			t.Fatal("expected the git subprocess to fail")
		}
		// Exactly how gather-pr-context wraps it before failProviderStage.
		wrapped := fmt.Errorf("checkout PR #3894's branch %q: %w", "goobernetes/implementation/47622a80", gitErr)

		code, retryable, extra := classifyProviderError(wrapped)
		if code != telemetry.ErrCodeInfraGit {
			t.Fatalf("code = %q, want %q — provider_error is what parked #3894", code, telemetry.ErrCodeInfraGit)
		}
		if retryable {
			t.Fatal("retryable = true; a dubious-ownership fetch does not get better on its own")
		}
		if len(extra) != 0 {
			t.Fatalf("extra = %v, want none", extra)
		}
		if class := telemetry.ClassifyError(code); !class.InfraFault() {
			t.Fatalf("class = %q, InfraFault() = false — the #3361 exemption would not fire", class)
		}
	})

	t.Run("a provider refusal is still a provider error", func(t *testing.T) {
		code, _, _ := classifyProviderError(errors.New("merge PR #3894: base branch was modified"))
		if code != errorCodeProvider {
			t.Fatalf("code = %q, want %q", code, errorCodeProvider)
		}
	})

	t.Run("a subprocess we did not build stays unclassified", func(t *testing.T) {
		bare := exec.Command("/nonexistent/go", "test", "./...")
		err := bare.Run()
		if err == nil {
			t.Fatal("expected the subprocess to fail")
		}
		code, _, _ := classifyProviderError(fmt.Errorf("local-ci: %w", err))
		if code == telemetry.ErrCodeInfraGit {
			t.Fatal("a failing build/test subprocess must not be excused as infra git — that is real evidence about the item")
		}
	})

	t.Run("only the workspace helpers produce the tag", func(t *testing.T) {
		raw := exec.Command("/nonexistent/git", "status")
		if err := raw.Run(); err == nil {
			t.Fatal("expected failure")
		} else {
			var tagged *gitCommandError
			if errors.As(err, &tagged) {
				t.Fatal("a bare exec.Command is tagged; the marker no longer means 'git, run by us, against a checkout'")
			}
		}
	})

	t.Run("a successful command is not tagged", func(t *testing.T) {
		if err := gitFailure(nil); err != nil {
			t.Fatalf("gitFailure(nil) = %v, want nil", err)
		}
	})
}

// A push failing is normally the forge answering, not the workspace breaking:
// GH006 protected-branch and merge-queue rejections both arrive as a plain
// `exit status 1` from `git push`. Those must keep the retryable provider
// codes classifyProviderError already gives them.
func TestRejectedPushStaysAProviderVerdict(t *testing.T) {
	push := workspaceGitAuthCommand("/workspace", "tok", "push", "--force-with-lease", "https://example.test/r", "b:b")
	push.Path = "/nonexistent/git"
	_, err := workspaceGitCombinedOutput(push)
	if err == nil {
		t.Fatal("expected the push to fail")
	}
	var tagged *gitCommandError
	if errors.As(err, &tagged) {
		t.Fatal("a rejected push is tagged infra; GH006 would stop being retryable")
	}

	queued := fmt.Errorf("force-push branch: exit status 1: remote: error: GH006: Protected branch update failed\nremote: - A pull request for this branch has been added to a merge queue.")
	code, retryable, _ := classifyProviderError(queued)
	if code != errorCodeBranchMergeQueued || !retryable {
		t.Fatalf("code = %q retryable = %v, want %q and retryable", code, retryable, errorCodeBranchMergeQueued)
	}
}
