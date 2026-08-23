package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func TestMergeReviewEligibilityPolicy(t *testing.T) {
	pr := providers.PullRequestSummary{
		Labels:             []string{"goobers:team-a"},
		Assignees:          []string{"alice"},
		RequestedReviewers: []string{"review-bot"},
	}
	tests := []struct {
		name            string
		label           string
		respectAssignee bool
		selfIdentity    string
		wantEligible    bool
		wantDescription string
	}{
		{"legacy", "", false, "", true, "legacy"},
		{"opted in", "goobers:team-a", false, "", true, "label:goobers:team-a"},
		{"missing opt in", "goobers:team-b", false, "", false, ""},
		{"assigned", "", true, "ALICE", true, "assignee-or-reviewer:ALICE"},
		{"requested reviewer", "", true, "REVIEW-BOT", true, "assignee-or-reviewer:REVIEW-BOT"},
		{"unrelated identity", "", true, "mallory", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eligibleByMergeReviewPolicy(pr, tt.label, tt.respectAssignee, tt.selfIdentity); got != tt.wantEligible {
				t.Fatalf("eligibleByMergeReviewPolicy() = %t, want %t", got, tt.wantEligible)
			}
			if tt.wantDescription != "" && mergeReviewEligibilityDescription(tt.label, tt.respectAssignee, tt.selfIdentity) != tt.wantDescription {
				t.Fatalf("eligibility description = %q, want %q", mergeReviewEligibilityDescription(tt.label, tt.respectAssignee, tt.selfIdentity), tt.wantDescription)
			}
		})
	}
}

// setDaemonIdentity rewrites root's instance.yaml to configure a pat-kind
// DaemonIdentity — enough for daemonIdentityAuthorLogin to consult it, since
// its PAT branch never reads the token ref's own value, only the provider
// already built from whichever credential the stage resolved (here, the
// fake GitHub server via providerCmdEnv).
func setDaemonIdentity(t *testing.T, root string) {
	t.Helper()
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatalf("load instance config: %v", err)
	}
	cfg.DaemonIdentity = &instance.DaemonIdentityConfig{
		Kind:  instance.GitHubAuthPAT,
		Token: &instance.TokenRef{Env: "DAEMON_PAT_UNUSED"},
	}
	if err := instance.WriteConfig(layoutFor(root).ConfigFile(), cfg); err != nil {
		t.Fatalf("write instance config: %v", err)
	}
}

// TestPRSelectAttributionUsesDaemonIdentityLogin is #1780's PR-attribution
// acceptance: with a DaemonIdentity configured, pr-select selects a PR by
// comparing its author login against the daemon identity's own resolved
// login — not the branch-prefix heuristic — so a PR whose head does not
// carry the managed prefix is still recognized as ours when its author
// matches, and a same-shaped PR from someone else is not.
func TestPRSelectAttributionUsesDaemonIdentityLogin(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.authenticatedLogin = "goobers-daemon"
	setDaemonIdentity(t, root)

	const ownNumber = 20
	const otherNumber = 21
	server.addIssue(ownNumber, "Our PR")
	server.addIssue(otherNumber, "Someone else's PR")
	// Neither PR's head carries the managed goobers/implementation/ prefix —
	// under the old heuristic alone, authorScope=goobers would exclude both.
	server.addOpenPR(ownNumber, "feature/unrelated-naming", "main", "sha20head", "shamainbase", false, nil, nil)
	server.addOpenPR(otherNumber, "feature/also-unrelated", "main", "sha21head", "shamainbase", false, nil, nil)
	server.setPRIdentities(ownNumber, "goobers-daemon", nil, nil)
	server.setPRIdentities(otherNumber, "someone-else", nil, nil)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "selected PR #20") {
		t.Fatalf("stdout = %q, want PR #20 selected by daemon-identity login match", stdout)
	}
}

// TestPRSelectAttributionFallsBackToHeadPrefixWithoutDaemonIdentity proves
// the DaemonIdentity-aware check is purely additive: with no DaemonIdentity
// configured (the default), a PR whose author happens to equal the
// resolved-live-login is still excluded unless its head carries the managed
// prefix — byte-identical to pr-select's behavior before #1780.
func TestPRSelectAttributionFallsBackToHeadPrefixWithoutDaemonIdentity(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.authenticatedLogin = "goobers-daemon"
	// No setDaemonIdentity call: DaemonIdentity stays nil.

	const number = 30
	server.addIssue(number, "Not on the managed branch prefix")
	server.addOpenPR(number, "feature/unrelated-naming", "main", "sha30head", "shamainbase", false, nil, nil)
	server.setPRIdentities(number, "goobers-daemon", nil, nil)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") && !strings.Contains(stdout, "no eligible") {
		t.Fatalf("stdout = %q, want no-work (branch prefix still required, login match alone must not select it)", stdout)
	}
}

// TestGatherSiblingContextAdvisoryModeConsistentWithDaemonIdentity drives the
// real pr-select -> gather-sibling-context chain (mirroring
// TestPRSelectAndSiblingContextShareProductionListSnapshot's pattern) with a
// DaemonIdentity configured, authorScope=any (the only scope where
// advisoryMode's own value — not just eligibility — actually depends on the
// ownership check: advisoryMode is authorScope==any && !isOwnPullRequest,
// which is unconditionally false whenever authorScope is the default
// "goobers", regardless of which ownership logic runs), and a PR whose head
// does not carry the managed prefix but whose author matches the resolved
// login. Both stages must agree it is non-advisory (isOwnPullRequest gives
// the same answer in both places); before #1780's fix to gather-sibling-
// context's own consistency check, this combination would have failed the
// stage with an advisoryMode mismatch, since only pr-select's decision had
// been made DaemonIdentity-aware.
func TestGatherSiblingContextAdvisoryModeConsistentWithDaemonIdentity(t *testing.T) {
	const selected = 40
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.authenticatedLogin = "goobers-daemon"
	setDaemonIdentity(t, root)

	server.addIssue(selected, "Selected PR, off the managed branch prefix")
	server.addOpenPR(selected, "feature/unrelated-naming", "main", "head-40", "base-40", false, nil, nil)
	server.setPRIdentities(selected, "goobers-daemon", nil, nil)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_AUTHORSCOPE", authorScopeAny)

	workDir := t.TempDir()
	t.Chdir(workDir)
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "selected-pr.json"))
	if err != nil {
		t.Fatalf("read selected-pr.json: %v", err)
	}
	var selectedResult map[string]string
	if err := json.Unmarshal(data, &selectedResult); err != nil {
		t.Fatalf("unmarshal selected-pr.json: %v", err)
	}
	if selectedResult["advisoryMode"] != "false" {
		t.Fatalf("pr-select advisoryMode = %q, want \"false\" (login-matched daemon PR is not advisory)", selectedResult["advisoryMode"])
	}

	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", strconv.Itoa(selected))
	t.Setenv("GOOBERS_INPUT_ADVISORYMODE", selectedResult["advisoryMode"])
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q — want its own advisoryMode consistency check to agree with pr-select's daemon-identity-aware decision", code, stdout, stderr)
	}
}
