package main

import (
	"os"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
