package main

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/journal"
)

// replaySelectSourceGitHub replays select-source's decomposition source
// selection on GitHub. Two escalated runs are eligible sources: the older one
// names parent 502, which publish-batch has already decomposed (its
// attributed published-batch marker is on the issue), so select-source must
// skip it and select parent 501. On 501 it claims the ledger and mirrors the
// claim through ClaimWorkItem, whose breadcrumb it then reads back to elect
// the winner. A retry of the same run must recognise both Goobers texts
// through their attribution footers: skip 502 again, and re-claim 501
// without a second breadcrumb.
func replaySelectSourceGitHub(t *testing.T) replayFixture {
	const (
		selectRun  = "decomposition-run-1"
		trustLabel = "acme:maintainer-approved"
	)
	root := initDemo(t)
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-decomposed",
		startedAt:      time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "502",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "already decomposed"),
	})
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-open",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "501",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "too large to implement as one PR"),
	})

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(501, "A very large issue", trustLabel)
	server.addIssue(502, "An already decomposed issue", trustLabel)
	// addComment stores it under Goobers' login with the attribution footer,
	// as publish-batch's attributed write leaves it.
	server.addComment(502, decomposition.PublishedBatchMarkerPrefix+
		" v1 parent=502 digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 children=601,602")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", selectRun)
	decompositionInstanceEnv(t, root)
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", trustLabel)
	counter := recordGitHubWrites(t, server)
	return replayFixture{
		run: func(t *testing.T) (int, string, string) {
			t.Chdir(t.TempDir())
			code, stdout, stderr := runArgs(t, "select-source", root)
			if code == 0 && !strings.Contains(stdout, "selected parent 501 ") {
				return 1, stdout, "parent 501 was not selected (502 carries an attributed published-batch marker): " + stderr
			}
			return code, stdout, stderr
		},
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			claims := func(number int) []string {
				return ownedGitHubComments(t, server, number, func(body string) bool {
					return claimRunIDOf(body) == selectRun
				})
			}
			if stray := claims(502); len(stray) != 0 {
				t.Errorf("decomposed parent 502 carries claim breadcrumbs: %q", stray)
			}
			return map[string][]string{
				"claim 501": claims(501),
				"batch marker 502": ownedGitHubComments(t, server, 502, func(body string) bool {
					return strings.HasPrefix(body, decomposition.PublishedBatchMarkerPrefix)
				}),
			}
		},
	}
}
