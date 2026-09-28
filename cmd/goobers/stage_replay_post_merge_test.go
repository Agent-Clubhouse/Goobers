package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// post-merge's replay case. A merge-review retry re-runs post-merge whenever
// the reconcile ledger holds no completed entry for the merged pull request,
// which is every run that did not time out in merge-pr first. The stage then
// reads back three things it wrote on the earlier run, each stored with the
// attribution footer:
//
//   - the "Merged in pull request #N." close-out on each issue the pull
//     request closes (closeReferencedIssue, deduped by first line);
//   - the cost summary on the merged pull request
//     (reconcileIssueCommentCostSummary, deduped by postMergeCostSummaryMarker);
//   - the remediation handoff on each displaced sibling
//     (persistPostMergeRemediationHandoff, found by its marker and edited in
//     place).
const (
	replayPostMergeNumber  = 20
	replayPostMergeIssue   = 42
	replayPostMergeSibling = 21
)

// isPostMergeCloseOut reports whether body is post-merge's close-out comment
// for the replayed pull request.
func isPostMergeCloseOut(body string) bool {
	return strings.HasPrefix(body, "Merged in pull request #20.")
}

func isPostMergeCostSummary(body string) bool {
	return strings.Contains(body, postMergeCostSummaryMarker)
}

func isPostMergeHandoffFor(merged int) func(string) bool {
	return func(body string) bool {
		handoff, ok := parsePostMergeRemediationHandoff(body)
		return ok && handoff.DisplacingPullNumber == merged
	}
}

// replayPostMergeGitHub merges PR #20 ("Fixes #42") with cost publication on
// and one conflicted sibling (#21) the merge displaces, behind a verbatim
// fake: every write is stored with the footer the stage stamps.
func replayPostMergeGitHub(t *testing.T) replayFixture {
	st := newPostMergeServerState(replayPostMergeNumber, "main", "Implements the thing.\n\nFixes #42",
		[]string{"shared/pkg.go"}, []int{replayPostMergeSibling})
	st.setConflicted(replayPostMergeSibling)
	// An earlier run's cost receipt on the merged pull request, so the
	// stage has a summary to publish and to dedupe on the retry.
	st.prComments = []string{costComment(t, "goobers", "implementation", "cost-run", 20, 8_000_000_000).Body}
	inner := newPostMergeServer(t, "your-org", "your-repo", st)
	counter := &providerWriteCounter{}
	server := recordServerWrites(t, inner.Config.Handler, counter)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})
	// Cost publication gates on the originating gaggle; the replay publishes
	// under the instance default, as TestCostPublicationGitHubImmediateAndDelayed
	// does.
	t.Setenv(executor.GaggleEnvVar, "")
	setTestInstanceCostPublication(t, root, true)
	return replayFixture{
		run:    replayStageRun("post-merge", root),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			st.mu.Lock()
			defer st.mu.Unlock()
			mine := func(bodies []string, match func(string) bool) []string {
				var owned []string
				for _, body := range bodies {
					if match(providers.StripAttribution(body)) {
						owned = append(owned, body)
					}
				}
				return owned
			}
			return map[string][]string{
				"close-out":           mine(st.issueComments[replayPostMergeIssue], isPostMergeCloseOut),
				"cost summary":        mine(st.prComments, isPostMergeCostSummary),
				"remediation handoff": mine(st.issueComments[replayPostMergeSibling], isPostMergeHandoffFor(replayPostMergeNumber)),
			}
		},
	}
}
