package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
)

// replayRebasePRSiblingHandoffGitHub replays rebase-pr against a pull request
// that carries Goobers' merge-review verdict and a legacy (unversioned)
// sibling-overlap handoff post-merge left, both stored with the attribution
// footer a daemon write carries.
//
// Run 1 reads the handoff back, recognises it as a sibling overlap on the
// head it just fetched, and migrates it in place to the versioned form pinned
// to that head: one comment edit. Run 2 must read that migrated, attributed
// body back as an already-current handoff, so it edits nothing, and must
// still route the cycle as sibling-overlap alone (the verdict's finding is
// the sibling's ordering ask, not an independent substantive cause). A stage
// that compared the stored body exactly, or matched only the rendered text,
// would miss its own handoff on the retry: it would re-migrate, or drop the
// sibling-overlap cause and route a stale substantive finding instead.
func replayRebasePRSiblingHandoffGitHub(t *testing.T) replayFixture {
	const (
		runID      = "replay-rebase-pr"
		prNumber   = 67
		sibling    = 66
		prBranch   = "goobers/impl/run-replay-sibling-overlap"
		resultName = "rebase-result.json"
	)
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	targetHeadSHA := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "HEAD"))

	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "Selected PR", needsRemediationLabel)
	server.addComment(prNumber, renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Location: "PR #66",
			Message:  "PR #67 must reconcile its overlap with PR #66.",
		}},
	}))
	server.addComment(prNumber, `**Post-merge remediation handoff**

<!-- post-merge-remediation: {"displacingPullNumber":66,"reason":"file-overlap:shared.go","overlappingFiles":["shared.go"]} -->`)

	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubPRWrite)), runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "test-token")
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "test-token")
	for name, value := range map[string]string{
		"SELECTEDNUMBER":         "67",
		"HEAD":                   prBranch,
		"BASE":                   "main",
		"HASSUBSTANTIVEFINDINGS": "true",
		"HASFAILINGCI":           "false",
		"RESULTFILE":             resultName,
	} {
		t.Setenv("GOOBERS_INPUT_"+name, value)
	}
	counter := recordGitHubWrites(t, server)
	t.Chdir(wt.Path)

	isSiblingHandoff := func(body string) bool {
		handoff, ok := parsePostMergeRemediationHandoff(body)
		return ok && isSiblingOverlapHandoff(handoff) && handoff.DisplacingPullNumber == sibling
	}
	return replayFixture{
		run: func(t *testing.T) (int, string, string) {
			code, stdout, stderr := runArgs(t, "rebase-pr", root)
			if code != 0 {
				return code, stdout, stderr
			}
			result := readProviderStageResult(t, filepath.Join(wt.Path, resultName))
			if result["needsAgent"] != "true" || result["remediationCauses"] != "sibling-overlap" {
				return 1, stdout, "rebase-pr did not route its own attributed handoff as sibling-overlap alone: " +
					fmt.Sprintf("needsAgent=%v remediationCauses=%v\n", result["needsAgent"], result["remediationCauses"]) + stderr
			}
			remoteHead := strings.TrimSpace(runGitOutputT(
				t, filepath.Dir(origin), "--git-dir="+origin, "rev-parse", "refs/heads/"+prBranch,
			))
			if remoteHead != targetHeadSHA {
				return 1, stdout, "rebase-pr force-pushed " + remoteHead + " over " + targetHeadSHA +
					" while a sibling-overlap handoff was pending\n" + stderr
			}
			return 0, stdout, stderr
		},
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			owned := ownedGitHubComments(t, server, prNumber, isSiblingHandoff)
			for _, body := range owned {
				handoff, _ := parsePostMergeRemediationHandoff(body)
				if handoff.Version != postMergeRemediationHandoffVersion || handoff.TargetHeadSHA != targetHeadSHA {
					t.Errorf("sibling handoff = %+v, want version %d pinned to %s",
						handoff, postMergeRemediationHandoffVersion, targetHeadSHA)
				}
			}
			return map[string][]string{"sibling-overlap handoff": owned}
		},
	}
}
