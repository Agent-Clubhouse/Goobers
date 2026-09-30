package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// push-remediated writes no text of its own: it reads back the sticky
// remediation-state comment remediation-checkpoint stored earlier in the run,
// takes the recorded pre-remediation head SHA from it as the force-with-lease
// expectation, force-pushes, and clears goobers:needs-remediation. In a
// daemon run that comment carries the attribution footer, so these cases seed
// it stamped, as an attributed write stores it, and replay the stage: both
// runs must recover the lease from the attributed comment and publish (run 2
// is an up-to-date push), and neither may write a comment.

const replayADOPullRequestLabelClear = "DELETE /acme/project/_apis/git/repositories/web/pullrequests/{n}/labels/{label}"

// replayRemediationStateBody is the sticky remediation-state comment
// remediation-checkpoint records for headSHA, stored with the attribution
// footer its provider write adds.
func replayRemediationStateBody(t *testing.T, headSHA, baseSHA string) string {
	t.Helper()
	return stampOwnFixtureBody(renderRemediationComment(remediationState{
		Cycles: 1, LastDiffDigest: "sha256:prior", HeadSHA: headSHA, BaseSHA: baseSHA,
	}), "comment")
}

// replayPushRemediatedRun runs push-remediated and additionally requires it
// to have published: the PR branch on origin must sit at the worktree's
// reworked head. A stage that could not read its lease back fails here even
// if it reported success.
func replayPushRemediatedRun(root, wtPath, remoteTip string) func(t *testing.T) (int, string, string) {
	return func(t *testing.T) (int, string, string) {
		t.Helper()
		code, stdout, stderr := runArgs(t, "push-remediated", root)
		if code != 0 {
			return code, stdout, stderr
		}
		local := strings.TrimSpace(runGitOutputT(t, wtPath, "rev-parse", "HEAD"))
		pushed, _, _ := strings.Cut(strings.TrimSpace(runGitOutputT(t, wtPath, "ls-remote", "origin", "refs/heads/"+remediationPRBranch)), "\t")
		if pushed != local || pushed == remoteTip {
			return 1, stdout, stderr + "\nremote " + remediationPRBranch + " = " + pushed + ", want the reworked head " + local
		}
		result := readCheckpointResult(t, filepath.Join(wtPath, pushRemediatedResultName))
		if result["published"] != "true" || result["localHead"] != local {
			return 1, stdout, stderr + "\npush result does not record the publish: " + strings.TrimSpace(stdout)
		}
		return code, stdout, stderr
	}
}

// ownedRemediationState returns the bodies that carry a remediation-state
// payload.
func ownedRemediationState(bodies []string) map[string][]string {
	var owned []string
	for _, body := range bodies {
		if _, ok := parseRemediationStateComment(providers.StripAttribution(body)); ok {
			owned = append(owned, body)
		}
	}
	return map[string][]string{"remediation state": owned}
}

func replayPushRemediatedGitHub(t *testing.T) replayFixture {
	root, st, wtPath, remoteTip := pushRemediatedFixture(t, false)
	st.mu.Lock()
	st.comments = []string{replayRemediationStateBody(t, st.headSHA, st.baseSHA)}
	st.mu.Unlock()
	counter := &providerWriteCounter{}
	inner := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	server := recordServerWrites(t, inner.Config.Handler, counter)
	previous := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = previous })
	return replayFixture{
		run:    replayPushRemediatedRun(root, wtPath, remoteTip),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			st.mu.Lock()
			defer st.mu.Unlock()
			for _, label := range st.labels {
				if label == needsRemediationLabel {
					t.Errorf("labels = %v, want %s cleared", st.labels, needsRemediationLabel)
				}
			}
			return ownedRemediationState(st.comments)
		},
	}
}

func replayPushRemediatedADO(t *testing.T) replayFixture {
	root, st, wtPath, remoteTip := pushRemediatedADOFixture(t, false)
	setDeliveredADOStageCredentials(t)
	st.mu.Lock()
	st.threadComments = []string{replayRemediationStateBody(t, st.headSHA, st.baseSHA)}
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: st.owner, Project: st.project, Name: st.name}
	st.mu.Unlock()
	counter := &providerWriteCounter{}
	installADOStageProvider(t, repo, recordServerWrites(t, st.start(t).Config.Handler, counter))
	return replayFixture{
		run:    replayPushRemediatedRun(root, wtPath, remoteTip),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			st.mu.Lock()
			defer st.mu.Unlock()
			if len(st.deletedLabels) != 1 || st.deletedLabels[0] != needsRemediationLabel || st.workItemHit {
				t.Errorf("deletedLabels = %v (workItemHit=%v), want %s cleared once through the PR-label DELETE",
					st.deletedLabels, st.workItemHit, needsRemediationLabel)
			}
			return ownedRemediationState(st.threadComments)
		},
	}
}
