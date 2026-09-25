package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/worktree"
)

// TestForcePushWithLeaseClassifiesADOPolicyProtectedRejection is ADO-N26's
// acceptance for the remediation/rebase force-push path: forcePushWithLease
// -WithAuth (shared by pr-remediation's clean-rebase force-push and
// rebase-pr's own) must classify a real ADO TF402455 rejection the same way
// gitPushBranch does — a policyProtectedPushError, never a bare lease/race
// error a caller might otherwise route into an auth retry.
func TestForcePushWithLeaseClassifiesADOPolicyProtectedRejection(t *testing.T) {
	const prBranch = "goobers/impl/run-n26-force"
	origin, headSHA, _ := initPRBranchOrigin(t, prBranch)

	hookScript := "#!/bin/sh\n" +
		"echo '! [remote rejected] " + prBranch + " -> " + prBranch + " (TF402455: Pushes to this branch are not permitted; you must use a pull request to update this branch.)' 1>&2\n" +
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
		RepoURL: origin, RunID: "run-n26-force", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-n26-force",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	if _, err := checkoutExistingBranchWithAuth(t.Context(), wt.Path, prBranch, tokenGitAuthEnvironment("test-token")); err != nil {
		t.Fatalf("checkoutExistingBranch: %v", err)
	}

	// A force-push with nothing changed relative to the remote is a no-op
	// git never contacts the server for ("Everything up-to-date"), which
	// would never invoke the pre-receive hook at all — so this needs a real
	// local commit ahead of the captured lease value for the push to be
	// attempted.
	if err := os.WriteFile(filepath.Join(wt.Path, "rebased.txt"), []byte("rebased locally\n"), 0o644); err != nil {
		t.Fatalf("write rebased file: %v", err)
	}
	runGitT(t, wt.Path, "add", "rebased.txt")
	runGitT(t, wt.Path, "commit", "-m", "locally rebased commit")

	err = forcePushWithLeaseWithAuth(t.Context(), wt.Path, prBranch, headSHA, tokenGitAuthEnvironment("test-token"))
	if err == nil {
		t.Fatal("forcePushWithLeaseWithAuth: err = nil, want a policy-protected push error from the pre-receive hook's TF402455 rejection")
	}
	var policyErr *policyProtectedPushError
	if !errors.As(err, &policyErr) {
		t.Fatalf("forcePushWithLeaseWithAuth err = %v, want it to be (or wrap) a policyProtectedPushError", err)
	}
}
