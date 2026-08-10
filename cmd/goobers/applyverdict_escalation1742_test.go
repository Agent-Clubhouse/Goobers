package main

import (
	"context"
	"os"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestApplyVerdictDoesNotRearmEscalatedRemediation(t *testing.T) {
	tests := []struct {
		name            string
		labels          []string
		escalationState *remediationState
		wantRemediation bool
	}{
		{
			name:   "does not reapply removed remediation label",
			labels: []string{remediationEscalatedLabel},
		},
		{
			name:   "cleans conflicting remediation label",
			labels: []string{remediationEscalatedLabel, needsRemediationLabel},
		},
		{
			name:   "self-healed escalation resumes remediation",
			labels: []string{remediationEscalatedLabel},
			escalationState: &remediationState{
				Escalated:        true,
				EscalatedHeadSHA: "head-before-new-commits",
				EscalatedBaseSHA: "escalated-base",
			},
			wantRemediation: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const (
				prNumber = 1742
				runID    = "merge-review-escalated"
				headSHA  = "escalated-head"
				baseSHA  = "escalated-base"
			)

			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(prNumber, "Escalated PR")
			server.addOpenPR(
				prNumber,
				"goobers/implementation/escalated",
				"main",
				headSHA,
				baseSHA,
				false,
				tt.labels,
				[]fakePRFile{{
					path:      "cmd/goobers/applyverdict.go",
					status:    "modified",
					additions: 3,
				}},
			)
			server.mu.Lock()
			server.issues[prNumber].labels = append([]string(nil), tt.labels...)
			server.mu.Unlock()
			if tt.escalationState != nil {
				comment, err := remediationStateComment(*tt.escalationState)
				if err != nil {
					t.Fatalf("remediationStateComment: %v", err)
				}
				server.addComment(prNumber, comment)
			}
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
			t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
			t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "1742")
			t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", headSHA)
			t.Setenv("GOOBERS_INPUT_SELECTEDBASESHA", baseSHA)
			seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
				Decision:  apiv1.VerdictNeedsChanges,
				Summary:   "the defect remains",
				Rationale: "automated remediation already exhausted its budget",
				HeadSHA:   headSHA,
				BaseSHA:   baseSHA,
				Findings: []apiv1.Finding{{
					Severity: apiv1.SeverityError,
					Class:    apiv1.FindingSubstantive,
					Message:  "still needs a human decision",
				}},
			})

			t.Chdir(t.TempDir())
			code, stdout, stderr := runArgs(t, "apply-verdict", root)
			if code != 0 {
				t.Fatalf("apply-verdict: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if tt.wantRemediation {
				if strings.Contains(stdout, "without re-applying "+needsRemediationLabel) {
					t.Fatalf("stdout=%q, self-healed escalation must not suppress remediation", stdout)
				}
			} else if !strings.Contains(stdout, "without re-applying "+needsRemediationLabel) {
				t.Fatalf("stdout=%q, want suppressed remediation label message", stdout)
			}
			data, err := os.ReadFile("verdict-result.json")
			if err != nil {
				t.Fatalf("read verdict result: %v", err)
			}
			if !strings.Contains(string(data), `"decision":"needs-changes"`) {
				t.Fatalf("verdict result=%q, want published needs-changes decision", data)
			}

			server.mu.Lock()
			issue := server.issues[prNumber]
			reviews := append([]fakeReview(nil), server.prs[prNumber].reviews...)
			server.mu.Unlock()
			if !hasAnyLabel(issue.labels, []string{remediationEscalatedLabel}) {
				t.Fatalf("labels=%v, want %s retained", issue.labels, remediationEscalatedLabel)
			}
			if got := hasAnyLabel(issue.labels, []string{needsRemediationLabel}); got != tt.wantRemediation {
				t.Fatalf("labels=%v, has %s=%t, want %t", issue.labels, needsRemediationLabel, got, tt.wantRemediation)
			}
			if len(reviews) != 1 || reviews[0].state != "CHANGES_REQUESTED" {
				t.Fatalf("reviews=%+v, want the fresh needs-changes review published", reviews)
			}
			if len(issue.comments) == 0 || !strings.Contains(issue.comments[len(issue.comments)-1], "the defect remains") {
				t.Fatalf("comments=%v, want the fresh verdict status published", issue.comments)
			}
		})
	}
}

func TestApplyVerdictFailReplacesNeedsRemediationWithEscalation(t *testing.T) {
	const (
		prNumber = 2760
		runID    = "merge-review-fail"
		headSHA  = "failed-head"
		baseSHA  = "failed-base"
	)

	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "Failed PR")
	server.addOpenPR(
		prNumber,
		"goobers/implementation/failed",
		"main",
		headSHA,
		baseSHA,
		false,
		[]string{needsRemediationLabel},
		[]fakePRFile{{path: "cmd/goobers/applyverdict.go", status: "modified", additions: 3}},
	)
	server.mu.Lock()
	server.issues[prNumber].labels = []string{needsRemediationLabel}
	server.mu.Unlock()

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "2760")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", headSHA)
	t.Setenv("GOOBERS_INPUT_SELECTEDBASESHA", baseSHA)
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision:  apiv1.VerdictFail,
		Summary:   "human intervention required",
		Rationale: "the approach cannot be remediated automatically",
		HeadSHA:   headSHA,
		BaseSHA:   baseSHA,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Message:  "requires a human decision",
		}},
	})

	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("apply-verdict: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !issueHasLabel(server, prNumber, remediationEscalatedLabel) {
		t.Fatalf("labels do not contain %s", remediationEscalatedLabel)
	}
	if issueHasLabel(server, prNumber, needsRemediationLabel) {
		t.Fatalf("labels still contain %s after fail escalation", needsRemediationLabel)
	}
}

func TestApplyVerdictRepeatFailRefreshesEscalationBaseSnapshot(t *testing.T) {
	const (
		prNumber   = 2378
		runID      = "merge-review-repeat-fail"
		headSHA    = "unchanged-head"
		pinnedBase = "base-at-pr-cut"
		oldBaseTip = "base-at-original-escalation"
		liveBase   = "base-after-sibling-merge"
	)

	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "Repeat-failing escalated PR")
	server.addOpenPR(
		prNumber,
		"goobers/implementation/repeat-fail",
		"main",
		headSHA,
		pinnedBase,
		false,
		[]string{remediationEscalatedLabel},
		[]fakePRFile{{path: "cmd/goobers/applyverdict.go", status: "modified", additions: 3}},
	)
	server.mu.Lock()
	server.issues[prNumber].labels = []string{remediationEscalatedLabel}
	server.mu.Unlock()
	stateComment, err := remediationStateComment(remediationState{
		Cycles:           4,
		Escalated:        true,
		EscalatedReason:  "the defect still needs human intervention",
		EscalatedHeadSHA: headSHA,
		EscalatedBaseSHA: oldBaseTip,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	server.addComment(prNumber, stateComment)
	server.setBranchTip("main", liveBase)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "2378")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", headSHA)
	t.Setenv("GOOBERS_INPUT_SELECTEDBASESHA", pinnedBase)
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision:  apiv1.VerdictFail,
		Summary:   "the defect remains",
		Rationale: "repeat review reached the same rejection",
		HeadSHA:   headSHA,
		BaseSHA:   pinnedBase,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Message:  "still needs a human decision",
		}},
	})

	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("apply-verdict: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	server.mu.Lock()
	comments := append([]string(nil), server.issues[prNumber].comments...)
	server.mu.Unlock()
	var refreshed remediationState
	var found bool
	for _, comment := range comments {
		if state, ok := parseRemediationStateComment(comment); ok {
			refreshed, found = state, true
		}
	}
	if !found {
		t.Fatalf("comments=%v, want remediation state", comments)
	}
	if refreshed.EscalatedHeadSHA != headSHA || refreshed.EscalatedBaseSHA != liveBase {
		t.Fatalf("refreshed escalation snapshot = (%q, %q), want (%q, %q)",
			refreshed.EscalatedHeadSHA, refreshed.EscalatedBaseSHA, headSHA, liveBase)
	}

	blocked, err := escalationStillBlocks(
		context.Background(),
		server.newGitHubProvider("token"),
		providers.RepositoryRef{Owner: "your-org", Name: "your-repo"},
		providers.PullRequestSummary{
			Number: prNumber, Base: "main", HeadSHA: headSHA, BaseSHA: pinnedBase,
			Labels: []string{remediationEscalatedLabel},
		},
	)
	if err != nil {
		t.Fatalf("escalationStillBlocks: %v", err)
	}
	if !blocked {
		t.Fatal("blocked = false, want true after repeat fail consumes the base-advance self-heal")
	}
}
