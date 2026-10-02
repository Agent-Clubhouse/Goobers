package pushrejection

import (
	"errors"
	"strings"
	"testing"
)

// adoTF402455Stderr is real ADO stderr for a push refused by an enabled
// branch policy: the client-side "remote rejected" line names TF402455, and
// the remote's own exception name (relayed verbatim by ADO's git server) is
// GitRefUpdateRejectedByPolicyException.
const adoTF402455Stderr = "To https://dev.azure.com/example-org/example-project/_git/example-repo\n" +
	"! [remote rejected] main -> main (TF402455: Pushes to this branch are not permitted; you must use a pull request to update this branch.)\n" +
	"error: failed to push some refs to 'https://dev.azure.com/example-org/example-project/_git/example-repo'\n"

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

// TestIsADOPolicyProtected is ADO-N26's classifier table: only ADO's own
// policy-rejection markers match, and a plain ref race or an auth failure —
// including one that also happens to print git's generic "failed to push
// some refs" trailer — do not.
func TestIsADOPolicyProtected(t *testing.T) {
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
			if got := IsADOPolicyProtected(tc.output); got != tc.want {
				t.Fatalf("IsADOPolicyProtected(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// TestIsRaceAloneCannotTellAPolicyRejectionFromARace documents WHY
// push-branch's retry loop must check IsADOPolicyProtected ahead of IsRace:
// TF402455's own "failed to push some refs" trailer is exactly the marker
// IsRace keys on, so read in isolation the two classifiers disagree on the
// same real ADO rejection text.
func TestIsRaceAloneCannotTellAPolicyRejectionFromARace(t *testing.T) {
	err := errors.New(adoTF402455Stderr)
	if !IsRace(err) {
		t.Fatal("IsRace = false, want true — precondition: TF402455's trailer text must still look race-shaped in isolation, which is exactly why the ordering in push-branch's retry loop matters")
	}
	if !IsADOPolicyProtected(err.Error()) {
		t.Fatal("IsADOPolicyProtected = false, want true")
	}
}

// TestClassify is #5502's classifier table: GitHub's workflow-permission
// refusal (full or truncated) is typed as a WorkflowPermissionError, ADO's
// TF402455 keeps its own type, and plain races and auth failures stay
// untyped.
func TestClassify(t *testing.T) {
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
			if got := IsGitHubWorkflowPermission(tc.output); got != tc.wantWorkflow {
				t.Fatalf("IsGitHubWorkflowPermission = %v, want %v", got, tc.wantWorkflow)
			}
			err := Classify("b", tc.output, errors.New("exit status 1"))
			var workflowErr *WorkflowPermissionError
			if got := errors.As(err, &workflowErr); got != tc.wantWorkflow {
				t.Fatalf("Classify is WorkflowPermissionError = %v, want %v (err = %v)", got, tc.wantWorkflow, err)
			}
			var policyErr *PolicyProtectedError
			if got := errors.As(err, &policyErr); got != tc.wantPolicy {
				t.Fatalf("Classify is PolicyProtectedError = %v, want %v (err = %v)", got, tc.wantPolicy, err)
			}
			if got := IsTerminal(err); got != (tc.wantWorkflow || tc.wantPolicy) {
				t.Fatalf("IsTerminal = %v, want %v", got, tc.wantWorkflow || tc.wantPolicy)
			}
			// A race is only retried when it is not a terminal refusal.
			if got := IsRace(err) && !IsTerminal(err); got != tc.wantRace {
				t.Fatalf("retried as a ref race = %v, want %v", got, tc.wantRace)
			}
			if !strings.Contains(err.Error(), strings.TrimSpace(tc.output)) {
				t.Fatalf("err = %q, want it to carry git's output verbatim", err)
			}
		})
	}
}

// TestWorkflowPermissionErrorMessageIsActionable proves the surfaced message
// names the cause and the remedy, not just git's rejection.
func TestWorkflowPermissionErrorMessageIsActionable(t *testing.T) {
	err := Classify("feature", githubWorkflowPermissionStderr, errors.New("exit status 1"))
	msg := err.Error()
	for _, want := range []string{"`workflows`", "`workflow` scope", ".github/workflows/", "grant the GitHub App installation", "push this change manually", "retrying cannot succeed", "refusing to allow a GitHub App"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}
