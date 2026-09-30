package main

import (
	"testing"

	"github.com/goobers/goobers/providers"
)

// replayRecordMergeRefusalGitHub replays record-merge-refusal (#950): the
// stage reads its sticky demotion comment back out of the PR thread, bumps the
// refusal counter at an unchanged head and edits that comment in place. Under
// daemon attribution the stored comment carries the footer after the payload,
// so run 2 must still find it, accumulate to attempt 2 and edit it rather than
// start a second demotion trail at attempt 1.
func replayRecordMergeRefusalGitHub(t *testing.T) replayFixture {
	const (
		number  = 77
		headSHA = "sha-stuck"
	)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(number, "goobers/implementation/stuck", "main", headSHA, "base1", false, nil, nil)
	server.addIssue(number, "stuck lander")
	// A reviewer's comment on the pull request is not Goobers' and must be
	// left alone.
	server.addCommentAs(number, "reviewer", "Why does this keep failing to merge?")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-replay-refusal")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "77")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", headSHA)
	t.Setenv("GOOBERS_INPUT_REASON", "base moved: the base advance touches files this PR also changes")
	counter := recordGitHubWrites(t, server)
	t.Chdir(t.TempDir())
	return replayFixture{
		run:    replayStageRun("record-merge-refusal", root),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			owned := ownedGitHubComments(t, server, number, func(body string) bool {
				_, ok := parseMergeDemotionComment(body)
				return ok
			})
			// Both runs are refusals at the same head, so the one sticky record
			// must have accumulated to attempt 2: a record stuck at attempt 1
			// means run 2 did not read run 1's attributed comment back.
			if len(owned) == 1 {
				state, _ := parseMergeDemotionComment(providers.StripAttribution(owned[0]))
				if state.Attempts != 2 || state.HeadSHA != headSHA || state.Demoted {
					t.Errorf("demotion record = %+v, want attempt 2 at %s, not demoted", state, headSHA)
				}
			}
			return map[string][]string{"merge-demotion record": owned}
		},
	}
}
