package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

func TestApplyVerdictAuthoritativeHeadPin(t *testing.T) {
	tests := []struct {
		echo  string
		moved bool
	}{
		{echo: "match"},
		{echo: "match", moved: true},
		{echo: "mismatch"},
		{echo: "mismatch", moved: true},
		{echo: "absent"},
		{echo: "absent", moved: true},
	}

	for _, tt := range tests {
		state := "unchanged"
		if tt.moved {
			state = "moved"
		}
		t.Run(tt.echo+"/"+state, func(t *testing.T) {
			const (
				prNumber = 10
				pinHead  = "reviewed-head"
				pinBase  = "reviewed-base"
				runID    = "review-run"
			)
			currentHead := pinHead
			if tt.moved {
				currentHead = "current-head"
			}

			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(prNumber, "Selected PR")
			server.addOpenPR(prNumber, "goobers/implementation/run-10", "main", currentHead, pinBase, false, nil, nil)
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
			t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
			t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")

			verdict := apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "ready"}
			switch tt.echo {
			case "match":
				verdict.HeadSHA, verdict.BaseSHA = pinHead, pinBase
			case "mismatch":
				verdict.HeadSHA, verdict.BaseSHA = "different-reviewed-head", pinBase
			}
			seedGateVerdictJournal(t, root, runID, verdict)
			t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", pinHead)
			t.Setenv("GOOBERS_INPUT_SELECTEDBASESHA", pinBase)

			t.Chdir(t.TempDir())
			code, stdout, stderr := runArgs(t, "apply-verdict", root)
			if code != 0 {
				t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			data, err := os.ReadFile("verdict-result.json")
			if err != nil {
				t.Fatalf("read verdict result: %v", err)
			}
			var result map[string]string
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatalf("unmarshal verdict result: %v", err)
			}

			wantApply := tt.echo != "mismatch" && !tt.moved
			server.mu.Lock()
			reviews := append([]fakeReview(nil), server.prs[prNumber].reviews...)
			comments := append([]string(nil), server.issues[prNumber].comments...)
			labels := append([]string(nil), server.issues[prNumber].labels...)
			server.mu.Unlock()
			if !wantApply {
				if result["decision"] != "moot" {
					t.Fatalf("result = %v, want a voided verdict", result)
				}
				wantReason := "PR head moved"
				if tt.echo == "mismatch" {
					wantReason = "reviewer echoed head SHA"
				}
				if !strings.Contains(result["reason"], wantReason) {
					t.Fatalf("journaled reason = %q, want it to contain %q", result["reason"], wantReason)
				}
				if len(reviews) != 0 || len(comments) != 0 || len(labels) != 0 {
					t.Fatalf("voided verdict mutated PR: reviews=%v comments=%v labels=%v", reviews, comments, labels)
				}
				return
			}

			if result["decision"] != "pass" || result["reason"] != "" {
				t.Fatalf("result = %v, want applied pass without a void reason", result)
			}
			if len(reviews) != 1 || reviews[0].commitSHA != pinHead || len(comments) != 1 {
				t.Fatalf("applied verdict = reviews=%v comments=%v, want one review and comment pinned to %s", reviews, comments, pinHead)
			}
			posted, ok := parseVerdictComment(comments[0])
			if !ok || posted.HeadSHA != pinHead || posted.BaseSHA != pinBase {
				t.Fatalf("posted verdict = %+v, ok=%v, want authoritative pin (%s, %s)", posted, ok, pinHead, pinBase)
			}
		})
	}
}

func TestVerdictPinChecksAuthoritativeBase(t *testing.T) {
	const (
		head = "reviewed-head"
		base = "reviewed-base"
	)
	tests := []struct {
		name        string
		verdict     apiv1.Verdict
		currentBase string
		wantReason  string
	}{
		{
			name:        "matching echo and unchanged base apply",
			verdict:     apiv1.Verdict{BaseSHA: base},
			currentBase: base,
		},
		{
			name:        "mismatching echo voids distinctly",
			verdict:     apiv1.Verdict{BaseSHA: "different-reviewed-base"},
			currentBase: base,
			wantReason:  "reviewer echoed base SHA",
		},
		{
			name:        "absent echo still detects moved base",
			currentBase: "current-base",
			wantReason:  "PR base moved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := verdictPinVoidReason(tt.verdict, head, base, head, tt.currentBase)
			if tt.wantReason == "" && reason != "" {
				t.Fatalf("verdictPinVoidReason = %q, want an applicable pin", reason)
			}
			if tt.wantReason != "" && !strings.Contains(reason, tt.wantReason) {
				t.Fatalf("verdictPinVoidReason = %q, want it to contain %q", reason, tt.wantReason)
			}
		})
	}
}

func TestApplyVerdictReadsCurrentPRPastTickSnapshot(t *testing.T) {
	tests := []struct {
		name       string
		move       func(*fakeGitHubServer)
		wantReason string
	}{
		{
			name: "head moved",
			move: func(server *fakeGitHubServer) {
				server.setPRHead(10, "current-head", nil)
			},
			wantReason: "PR head moved",
		},
		{
			name: "base moved",
			move: func(server *fakeGitHubServer) {
				server.setPRBase(10, "current-base")
			},
			wantReason: "PR base moved",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const (
				pinHead = "reviewed-head"
				pinBase = "reviewed-base"
				runID   = "review-snapshot-race"
			)
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(10, "Selected PR")
			server.addOpenPR(10, "goobers/implementation/run-10", "main", pinHead, pinBase, false, nil, nil)
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
			t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
			t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
			t.Setenv(providersnapshot.EnvVar, "tick-before-review")
			seedGateVerdictJournal(t, root, runID, apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "ready"})
			t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", pinHead)
			t.Setenv("GOOBERS_INPUT_SELECTEDBASESHA", pinBase)

			repo, err := providerRepo(root)
			if err != nil {
				t.Fatalf("providerRepo: %v", err)
			}
			cached := newCachedGitHubProvider(root, "test-token")
			prs, err := cached.ListPullRequests(t.Context(), providers.ListPullRequestsRequest{
				Repository: repo, Base: "main", HeadPrefix: providerBranchNamespace(), SkipCheckState: true,
			})
			if err != nil || len(prs) != 1 {
				t.Fatalf("seed pull-request snapshot: prs=%v, err=%v", prs, err)
			}

			tt.move(server)
			t.Chdir(t.TempDir())
			code, stdout, stderr := runArgs(t, "apply-verdict", root)
			if code != 0 {
				t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}

			var result map[string]string
			raw, err := os.ReadFile("verdict-result.json")
			if err != nil {
				t.Fatalf("read verdict result: %v", err)
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatalf("unmarshal verdict result: %v", err)
			}
			if result["decision"] != "moot" || !strings.Contains(result["reason"], tt.wantReason) {
				t.Fatalf("result = %v, want a voided verdict containing %q", result, tt.wantReason)
			}
			if got := server.pullListRequestCount(); got != 1 {
				t.Fatalf("pull-list requests = %d, want the command to replay the seeded tick snapshot", got)
			}
			server.mu.Lock()
			reviews := len(server.prs[10].reviews)
			comments := len(server.issues[10].comments)
			labels := len(server.issues[10].labels)
			server.mu.Unlock()
			if reviews != 0 || comments != 0 || labels != 0 {
				t.Fatalf("voided verdict mutated PR: reviews=%d comments=%d labels=%d", reviews, comments, labels)
			}
		})
	}
}
