package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/worktree"
)

// adoTF402455Stderr is real ADO stderr for a push refused by an enabled
// branch policy: the client-side "remote rejected" line names TF402455, and
// the remote's own exception name (relayed verbatim by ADO's git server) is
// GitRefUpdateRejectedByPolicyException.
const adoTF402455Stderr = "To https://dev.azure.com/example-org/example-project/_git/example-repo\n" +
	"! [remote rejected] main -> main (TF402455: Pushes to this branch are not permitted; you must use a pull request to update this branch.)\n" +
	"error: failed to push some refs to 'https://dev.azure.com/example-org/example-project/_git/example-repo'\n"

// TestIsADOPolicyProtectedPush is ADO-N26's classifier table: only ADO's own
// policy-rejection markers match, and a plain ref race or an auth failure —
// including one that also happens to print git's generic "failed to push
// some refs" trailer — do not.
func TestIsADOPolicyProtectedPush(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"real ADO TF402455 rejection", adoTF402455Stderr, true},
		{"GitRefUpdateRejectedByPolicyException exception text alone", "remote: GitRefUpdateRejectedByPolicyException: the push was rejected by policy.", true},
		{"plain ref race", "! [rejected] main -> main (fetch first)\nerror: failed to push some refs", false},
		{"non-fast-forward race", "! [rejected] main -> main (non-fast-forward)\nerror: failed to push some refs", false},
		{"auth failure", "remote: Invalid username or password.\nfatal: Authentication failed for 'https://dev.azure.com/example-org/example-project/_git/example-repo'", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isADOPolicyProtectedPush(tc.output); got != tc.want {
				t.Fatalf("isADOPolicyProtectedPush(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// TestIsPushRaceErrorAloneCannotTellAPolicyRejectionFromARace documents WHY
// pushBranchWithRetry must check isADOPolicyProtectedPush ahead of
// isPushRaceError: TF402455's own "failed to push some refs" trailer is
// exactly the marker isPushRaceError keys on, so read in isolation the two
// classifiers disagree on the same real ADO rejection text.
func TestIsPushRaceErrorAloneCannotTellAPolicyRejectionFromARace(t *testing.T) {
	err := errors.New(adoTF402455Stderr)
	if !isPushRaceError(err) {
		t.Fatal("isPushRaceError = false, want true — precondition: TF402455's trailer text must still look race-shaped in isolation, which is exactly why the ordering in pushBranchWithRetry matters")
	}
	if !isADOPolicyProtectedPush(err.Error()) {
		t.Fatal("isADOPolicyProtectedPush = false, want true")
	}
}

// TestClassifyProviderError_BranchPolicyProtectedPush proves
// classifyProviderError maps a policyProtectedPushError to its own
// non-retryable code, never the auth-failed code — even wrapped, and even
// though the underlying message contains no HTTP status classifyProviderError
// could otherwise key on.
func TestClassifyProviderError_BranchPolicyProtectedPush(t *testing.T) {
	base := &policyProtectedPushError{branch: "main", err: errors.New(adoTF402455Stderr)}
	cases := []struct {
		name string
		err  error
	}{
		{"direct", base},
		{"wrapped", fmt.Errorf("push branch %q: %w", "main", base)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, retryable, extra := classifyProviderError(tc.err)
			if code != errorCodeBranchPolicyProtected {
				t.Fatalf("code = %q, want %q", code, errorCodeBranchPolicyProtected)
			}
			if code == errorCodeAuthFailed {
				t.Fatal("code classified as auth failure, want the distinct branch_policy_protected code")
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

// TestPushBranchWithRetrySingleAttemptOnPolicyProtectedPush is the
// pushBranchWithRetry acceptance case: a real bare origin whose pre-receive
// hook rejects every push with ADO's TF402455 text. Because that rejection
// also contains isPushRaceError's own "failed to push some refs" marker, a
// regression that checked isPushRaceError first (or omitted the policy check
// entirely) would fetch, rebase, and push again — driving the hook a second
// time. The hook counts its own invocations, so this asserts exactly one.
func TestPushBranchWithRetrySingleAttemptOnPolicyProtectedPush(t *testing.T) {
	origin := initBareOrigin(t)

	counterFile := filepath.Join(t.TempDir(), "attempts")
	hookScript := "#!/bin/sh\n" +
		"echo x >> \"" + counterFile + "\"\n" +
		"echo '! [remote rejected] main -> main (TF402455: Pushes to this branch are not permitted; you must use a pull request to update this branch.)' 1>&2\n" +
		"echo 'error: failed to push some refs' 1>&2\n" +
		"exit 1\n"
	hookPath := filepath.Join(origin, "hooks", "pre-receive")
	if err := os.WriteFile(hookPath, []byte(hookScript), 0o755); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin,
		RunID:   "run-n26",
		BaseRef: "main",
		Branch:  "goobers/implementation/run-n26",
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
	pushErr := pushBranchWithRetry(wt.Path, "goobers/implementation/run-n26", nil, &stderr)
	if pushErr == nil {
		t.Fatal("pushBranchWithRetry: err = nil, want a policy-protected push error")
	}
	var policyErr *policyProtectedPushError
	if !errors.As(pushErr, &policyErr) {
		t.Fatalf("pushBranchWithRetry err = %v, want it to be (or wrap) a policyProtectedPushError", pushErr)
	}

	attempts, readErr := os.ReadFile(counterFile)
	if readErr != nil {
		t.Fatalf("read attempts counter: %v", readErr)
	}
	if got := strings.Count(string(attempts), "x"); got != 1 {
		t.Fatalf("pre-receive hook invoked %d times, want exactly 1 (no fetch-rebase-retry on a policy-protected push)", got)
	}
	if strings.Contains(stderr.String(), "rejected as a ref race") {
		t.Fatalf("stderr = %q, want no ref-race retry warning for a policy-protected push", stderr.String())
	}
}
