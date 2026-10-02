// Package pushrejection classifies a rejected git push from git's own
// combined output: a ref race worth a fetch-rebase-retry, a refusal no retry
// can clear (an ADO branch policy, a GitHub credential not allowed to change
// workflows), or neither. goobers push-branch and rebase-pr's force-push share
// it so both stop on the same terminal refusals instead of spending a retry
// budget or misreporting a credential problem.
package pushrejection

import (
	"errors"
	"fmt"
	"strings"
)

// IsRace classifies a push failure as a ref race worth a
// fetch-rebase-retry, from git's own stable rejection phrasing. Everything
// else (auth failures, unreachable remotes, missing refs) is not retryable
// at this layer.
func IsRace(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"failed to push some refs",
		"fetch first",
		"non-fast-forward",
		"cannot lock ref",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// Classify wraps a failed push's error with git's combined output and, when
// that output names a refusal no retry can clear, types it so callers stop
// instead of treating it as a ref race or a credential problem. Shared by
// push-branch's push and rebase-pr's force-push.
func Classify(branch, output string, err error) error {
	wrapped := fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	if IsADOPolicyProtected(output) {
		return &PolicyProtectedError{Branch: branch, Err: wrapped}
	}
	if IsGitHubWorkflowPermission(output) {
		return &WorkflowPermissionError{Branch: branch, Err: wrapped}
	}
	return wrapped
}

// IsTerminal reports whether err is a typed push refusal that retrying — as
// a ref race or with a fresh credential — can never clear.
func IsTerminal(err error) bool {
	var policyErr *PolicyProtectedError
	var workflowErr *WorkflowPermissionError
	return errors.As(err, &policyErr) || errors.As(err, &workflowErr)
}

// githubWorkflowPermissionPrefix and githubWorkflowPermissionVerb bracket
// GitHub's refusal of a push that creates or updates a file under
// .github/workflows/ by a credential not allowed to change workflows. The
// credential named between them varies — "a GitHub App" (missing the
// installation's `workflows` permission, as on #5502), "a Personal Access
// Token" or "an OAuth App" (missing the `workflow` scope):
//
//	! [remote rejected] <ref> -> <ref> (refusing to allow a GitHub App to
//	create or update workflow `.github/workflows/ci.yml` without `workflows`
//	permission)
//
// The parenthetical can reach a log truncated ("workflow `.github/wo..."),
// so only these two fragments are matched (lower-cased).
const (
	githubWorkflowPermissionPrefix = "refusing to allow "
	githubWorkflowPermissionVerb   = " to create or update workflow"
)

// IsGitHubWorkflowPermission reports whether output — git's combined
// stdout+stderr from a rejected push — is GitHub's refusal of a
// workflow-file change by a credential lacking permission to change
// workflows (#5502). Like TF402455 it carries git's generic "failed to push
// some refs" trailer, so it is checked ahead of IsRace.
func IsGitHubWorkflowPermission(output string) bool {
	for _, line := range strings.Split(strings.ToLower(output), "\n") {
		i := strings.Index(line, githubWorkflowPermissionPrefix)
		if i >= 0 && strings.Contains(line[i:], githubWorkflowPermissionVerb) {
			return true
		}
	}
	return false
}

// WorkflowPermissionError reports that GitHub refused a push because the
// diff touches .github/workflows/ and the pushing credential (normally the
// GitHub App installation) is not allowed to change workflows. The diff is
// fine and the branch did not race: no retry can succeed until the
// credential is granted that permission, so its message leads with the
// remedy rather than the raw git rejection alone. The provider command's
// error classification maps it to a distinct non-retryable code.
type WorkflowPermissionError struct {
	Branch string
	Err    error
}

func (e *WorkflowPermissionError) Error() string {
	return fmt.Sprintf("push of branch %q was refused because it creates or updates a file under .github/workflows/ and the pushing credential lacks permission to change workflows; grant the GitHub App installation the `workflows` (Workflows: read and write) permission (or a personal/OAuth token the `workflow` scope), or push this change manually (retrying cannot succeed): %v", e.Branch, e.Err)
}

func (e *WorkflowPermissionError) Unwrap() error { return e.Err }

// IsADOPolicyProtected reports whether output — git's combined
// stdout+stderr from a rejected push — carries the markers ADO's Git provider
// attaches to a push refused by an enabled branch policy: TF402455 in the
// human-readable "remote rejected" line, and
// GitRefUpdateRejectedByPolicyException in the underlying exception name.
// Neither ever appears in a GitHub or Gitea rejection, so this never fires
// for those remotes.
//
// Design §5 ADO-N26 (F8): any enabled blocking policy makes the ref
// PR-only — a direct push (or force-push) to it is refused outright, not
// merely delayed by a race, so this is checked ahead of IsRace rather than
// folded into it.
func IsADOPolicyProtected(output string) bool {
	return strings.Contains(output, "TF402455") ||
		strings.Contains(output, "GitRefUpdateRejectedByPolicyException")
}

// PolicyProtectedError reports that a push (or force-push) was refused
// because the target branch is protected by an enabled ADO branch policy —
// never a credential problem. Its message names the policy, not the
// credential, so a caller does not misdiagnose it as an auth failure; the
// provider command's error classification maps it to a distinct
// non-retryable code and never sends it through an auth retry.
type PolicyProtectedError struct {
	Branch string
	Err    error
}

func (e *PolicyProtectedError) Error() string {
	return fmt.Sprintf("branch %q is protected by an ADO branch policy and cannot be pushed to directly (land the change through a pull request instead): %v", e.Branch, e.Err)
}

func (e *PolicyProtectedError) Unwrap() error { return e.Err }
