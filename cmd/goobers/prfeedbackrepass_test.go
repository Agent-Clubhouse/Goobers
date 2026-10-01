package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// finishStage records a stage.finished event with outputs exactly as the
// executor does after a stage's result file is read.
func (r *revisionRun) finishStage(stage string, outputs map[string]any) {
	r.t.Helper()
	if err := r.run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: stage, Attempt: 1, Status: string(apiv1.ResultSuccess), Outputs: outputs,
	}); err != nil {
		r.t.Fatalf("append %s: %v", stage, err)
	}
}

func TestLatestFeedbackRepass(t *testing.T) {
	finished := func(stage string, outputs map[string]any) journal.Event {
		return journal.Event{Type: journal.EventStageFinished, Stage: stage, Outputs: outputs}
	}
	for _, tc := range []struct {
		name          string
		events        []journal.Event
		wantFound     bool
		wantRef       string
		wantReGather  bool
		wantReasonKey string
	}{
		{name: "first pass has no stale record", events: []journal.Event{
			finished("gather-review-threads", nil), finished("implement", nil),
		}},
		{name: "pre-publication stale then re-gather", events: []journal.Event{
			finished("guard-before-push", map[string]any{"staleInput": "new_feedback", "localHead": "aaa"}),
			finished("gather-review-threads", nil),
		}, wantFound: true, wantRef: "aaa", wantReGather: true, wantReasonKey: "new_feedback"},
		{name: "post-publication stale uses the published head", events: []journal.Event{
			finished("resolve-review-threads", map[string]any{"staleInput": "changed_feedback", "publishedHeadSha": "bbb"}),
			finished("gather-review-threads", nil),
		}, wantFound: true, wantRef: "bbb", wantReGather: true, wantReasonKey: "changed_feedback"},
		{name: "stale without a re-gather yet", events: []journal.Event{
			finished("gather-review-threads", nil),
			finished("guard-before-push", map[string]any{"staleInput": "new_feedback", "localHead": "aaa"}),
		}, wantFound: true, wantRef: "aaa", wantReasonKey: "new_feedback"},
		{name: "a current verdict is not stale", events: []journal.Event{
			finished("guard-before-push", map[string]any{"staleInput": ""}),
			finished("gather-review-threads", nil),
		}},
		{name: "the newest stale verdict wins", events: []journal.Event{
			finished("guard-before-push", map[string]any{"staleInput": "new_feedback", "localHead": "aaa"}),
			finished("gather-review-threads", nil),
			finished("guard-before-push", map[string]any{"staleInput": "changed_feedback", "localHead": "ccc"}),
		}, wantFound: true, wantRef: "ccc", wantReasonKey: "changed_feedback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := latestFeedbackRepass(tc.events)
			if got.found != tc.wantFound || got.reference != tc.wantRef || got.reGathered != tc.wantReGather || got.reason != tc.wantReasonKey {
				t.Fatalf("latestFeedbackRepass = %+v, want found=%v ref=%q reGathered=%v reason=%q",
					got, tc.wantFound, tc.wantRef, tc.wantReGather, tc.wantReasonKey)
			}
		})
	}
}

// chdirToCommittedRepo makes the test's working directory a git checkout with
// one commit, standing in for the PR worktree a repo-workspace stage runs in,
// and returns its head.
func chdirToCommittedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q")
	runGitT(t, dir, "config", "user.name", "goobers")
	runGitT(t, dir, "config", "user.email", "goobers@example.com")
	commitWorkspaceChange(t, dir, "remediated.txt", "addressed the finding\n")
	t.Chdir(dir)
	return strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))
}

func commitWorkspaceChange(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	runGitT(t, dir, "add", "-A")
	runGitT(t, dir, "commit", "-q", "-m", "remediation")
}

func classifyRepass(t *testing.T, w feedbackWorld) map[string]any {
	t.Helper()
	resultFile := filepath.Join(t.TempDir(), "feedback-repass.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	code, stdout, stderr := runArgs(t, "pr-claim", "--classify-feedback-repass", w.root)
	if code != 0 {
		t.Fatalf("pr-claim --classify-feedback-repass: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	return readJSONResult(t, resultFile)
}

// TestThanksCommentMidRunIsAcknowledgedAcrossProviders is the casual human
// comment mid-run (#6126) end to end on GitHub and Azure DevOps: a "thanks"
// posted after the snapshot is rejected as stale input before publication and
// re-gathered; the agent's repass needs no change; the repass is classified
// as a no-op against the head that had already passed review and local CI,
// and the guard then finds the fresh snapshot current — so the run can
// publish instead of re-reviewing an unchanged head into an UNCHANGED_REPASS
// park. A repass that did change the branch takes the normal review path.
func TestThanksCommentMidRunIsAcknowledgedAcrossProviders(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		for _, agentChanges := range []bool{false, true} {
			name := string(kind) + "/no change needed"
			if agentChanges {
				name = string(kind) + "/agent changed the branch"
			}
			t.Run(name, func(t *testing.T) {
				w := newFeedbackWorld(t, kind)
				run := newRevisionRun(t, w.root, w.runID)
				run.selectHead("77", revisionSelectedSHA)
				reviewed := chdirToCommittedRepo(t)
				gatherIntoJournal(t, w, run)
				run.finishStage("gather-review-threads", nil)

				if first := classifyRepass(t, w); first[feedbackNoopOutput] != "false" {
					t.Fatalf("first pass classified as %v, want the normal review path", first)
				}

				w.addComment("Thanks!")
				stale := verifyFeedback(t, w)
				if stale.StaleInput != staleReasonNew || stale.LocalHead != reviewed {
					t.Fatalf("guard-before-push = %+v, want new_feedback beside the reviewed head %s", stale, reviewed)
				}
				run.finishStage("guard-before-push", map[string]any{
					staleInputOutput: stale.StaleInput, staleLocalHeadOutput: stale.LocalHead,
				})
				gatherIntoJournal(t, w, run)
				run.finishStage("gather-review-threads", nil)
				if agentChanges {
					wd, err := os.Getwd()
					if err != nil {
						t.Fatal(err)
					}
					commitWorkspaceChange(t, wd, "remediated.txt", "addressed the finding and the comment\n")
				}

				got := classifyRepass(t, w)
				want := strconv.FormatBool(!agentChanges)
				if got[feedbackNoopOutput] != want || got[feedbackReferenceHeadOutput] != reviewed || got[feedbackRepassCauseOutput] != staleReasonNew {
					t.Fatalf("classification = %v, want feedbackNoop=%s against the reviewed head %s", got, want, reviewed)
				}
				if _, ok := got[staleInputOutput]; ok {
					t.Fatalf("classification = %v, must never itself read as a stale verdict", got)
				}
				if current := verifyFeedback(t, w); current.StaleInput != "" {
					t.Fatalf("re-verified feedback = %+v, want the fresh snapshot current", current)
				}
			})
		}
	}
}

// TestVerifyFeedbackRecordsNoLocalHeadOutsideARepository: in a scratch
// workspace the guard has no branch to record, so a later classification has
// no reference and keeps the normal review path rather than guessing.
func TestVerifyFeedbackRecordsNoLocalHeadOutsideARepository(t *testing.T) {
	w := newFeedbackWorld(t, providers.ProviderGitHub)
	run := newRevisionRun(t, w.root, w.runID)
	run.selectHead("77", revisionSelectedSHA)
	t.Chdir(t.TempDir())
	gatherIntoJournal(t, w, run)
	w.addComment("Thanks!")
	if stale := verifyFeedback(t, w); stale.StaleInput != staleReasonNew || stale.LocalHead != "" {
		t.Fatalf("guard-before-push = %+v, want new_feedback with no local head", stale)
	}
}

// TestPushRemediatedAcknowledgesOwnPublishedHeadAcrossProviders: after this
// run published, a stale-feedback repass whose agent pass needed no change
// returns to push-remediated with the branch still at the run's own
// published head. That is acknowledged, not failed as "nothing to publish".
// The protection for a FIRST pass that produced nothing is unchanged.
func TestPushRemediatedAcknowledgesOwnPublishedHeadAcrossProviders(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			root, wtPath, selected, runID, setHead := ownPublicationFixture(t, kind)
			run := newRevisionRun(t, root, runID)
			run.selectHead("77", selected)

			code, stdout, stderr := runArgs(t, "push-remediated", root)
			if code != 0 {
				t.Fatalf("first publication: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			first := readJSONResult(t, filepath.Join(wtPath, pushRemediatedResultName))
			published, _ := first[pushRemediatedLocalHeadOutput].(string)
			if first[pushRemediatedPublishedOutput] != "true" || published == "" {
				t.Fatalf("first publication = %v, want published", first)
			}
			run.publish(published)
			setHead(published)

			code, stdout, stderr = runArgs(t, "push-remediated", root)
			if code != 0 {
				t.Fatalf("no-change repass: code = %d, stdout = %q, stderr = %q — a no-change feedback repass must not fail as nothing to publish", code, stdout, stderr)
			}
			again := readJSONResult(t, filepath.Join(wtPath, pushRemediatedResultName))
			if again[pushRemediatedPublishedOutput] != "true" || again[pushRemediatedLocalHeadOutput] != published ||
				!strings.Contains(stdout, "feedback acknowledged, no change needed") {
				t.Fatalf("no-change repass = %v (stdout %q), want the published head acknowledged", again, stdout)
			}
			remote := strings.TrimSpace(runGitOutputT(t, wtPath, "ls-remote", "origin", "refs/heads/"+remediationPRBranch))
			if sha, _, _ := strings.Cut(remote, "\t"); sha != published {
				t.Fatalf("remote branch = %q, want it left at the published head %s", sha, published)
			}
		})
	}
}

// TestPushRemediatedStillRefusesAnUnchangedFirstPass pins the protection the
// acknowledgement must not weaken: without an own publication in the
// journal, a branch still at its pre-remediation head is refused.
func TestPushRemediatedStillRefusesAnUnchangedFirstPass(t *testing.T) {
	root, wtPath, selected, runID, _ := ownPublicationFixture(t, providers.ProviderGitHub)
	newRevisionRun(t, root, runID).selectHead("77", selected)
	runGitT(t, wtPath, "reset", "-q", "--hard", selected)
	code, _, stderr := runArgs(t, "push-remediated", root)
	if code != 1 || !strings.Contains(stderr, "nothing to publish") {
		t.Fatalf("code = %d, stderr = %q, want the unchanged first pass refused", code, stderr)
	}
}

// ownPublicationFixture is a push-remediated fixture whose setHead moves only
// the provider's reported head (the remote branch moves by the real push).
func ownPublicationFixture(t *testing.T, kind providers.ProviderKind) (root, wtPath, selected, runID string, setHead func(string)) {
	t.Helper()
	if kind == providers.ProviderGitHub {
		var st *remediationCheckpointServerState
		root, st, wtPath, selected = pushRemediatedFixture(t, true)
		return root, wtPath, selected, "run-392-push", func(head string) {
			st.mu.Lock()
			defer st.mu.Unlock()
			st.headSHA = head
		}
	}
	var st *adoRemediationServerState
	root, st, wtPath, selected = pushRemediatedADOFixture(t, true)
	setDeliveredADOStageCredentials(t)
	return root, wtPath, selected, "run-392-ado", func(head string) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.headSHA = head
	}
}
