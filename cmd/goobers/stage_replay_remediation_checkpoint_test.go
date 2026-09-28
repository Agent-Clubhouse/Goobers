package main

import (
	"testing"

	"github.com/goobers/goobers/providers"
)

// replayRemediationCheckpointReviewerComment is a comment a person left on the
// pull request. It carries no remediation-state payload, is not Goobers', and
// must be left exactly as it was.
const replayRemediationCheckpointReviewerComment = "Please keep the empty-input guard, thanks."

// replayRemediationCheckpointGitHub replays remediation-checkpoint's sticky
// remediation-state comment on GitHub. Run 1 records cycle 1 as a new comment,
// stored with the attribution footer; run 2 must find that attributed comment
// by its embedded payload and edit it in place rather than posting another.
//
// The diff does not change between the runs, but both run under the same run
// ID at the same pushed head, so run 2 reads run 1's state back as this
// attempt's own write rather than a previous cycle: it re-records cycle 1
// and does not park the PR as a same-diff stall (#6008). The stall across
// distinct attempts is TestRemediationCheckpointEscalatesOnSameDiff's.
func replayRemediationCheckpointGitHub(t *testing.T) replayFixture {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels:         []string{needsRemediationLabel},
		comments:       []string{replayRemediationCheckpointReviewerComment},
		commentAuthors: []string{"human-reviewer"},
	}
	inner := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	counter := &providerWriteCounter{}
	server := recordServerWrites(t, inner.Config.Handler, counter)
	root := remediationCheckpointEnv(t, server.URL, false)
	return replayFixture{
		run:    replayStageRun("remediation-checkpoint", root),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			st.mu.Lock()
			defer st.mu.Unlock()
			var owned []string
			reviewerIntact := false
			for _, comment := range st.comments {
				if comment == replayRemediationCheckpointReviewerComment {
					reviewerIntact = true
					continue
				}
				if _, ok := parseRemediationStateComment(providers.StripAttribution(comment)); ok {
					owned = append(owned, comment)
				}
			}
			if !reviewerIntact {
				t.Errorf("reviewer comment was changed or removed: %q", st.comments)
			}
			return map[string][]string{"remediation state": owned}
		},
	}
}
