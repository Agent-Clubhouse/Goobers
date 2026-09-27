package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

type fakeStackedLister struct {
	prs  []providers.PullRequestSummary
	err  error
	reqs []providers.ListPullRequestsRequest
}

func (f *fakeStackedLister) ListPullRequests(_ context.Context, req providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error) {
	f.reqs = append(f.reqs, req)
	return f.prs, f.err
}

// TestADOCompletionDeletesSourceBranchKeepsBranchWhenUnsure pins the ADO
// deleteSourceBranch decision: with the delete grant it asks for deletion
// only after confirming no open pull request targets the head branch, and
// keeps the branch (recording why) when it cannot confirm that.
func TestADOCompletionDeletesSourceBranchKeepsBranchWhenUnsure(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "example-project", Name: "example-repo"}
	for _, tc := range []struct {
		name       string
		isADO      bool
		grant      bool
		headBranch string
		lister     *fakeStackedLister
		want       bool
		wantStatus string
	}{
		{name: "not ado", isADO: false, grant: true, headBranch: "feature", lister: &fakeStackedLister{}},
		{name: "no grant", isADO: true, headBranch: "feature", lister: &fakeStackedLister{}},
		{name: "granted and unstacked", isADO: true, grant: true, headBranch: "feature", lister: &fakeStackedLister{}, want: true},
		{
			name: "stacked", isADO: true, grant: true, headBranch: "feature",
			lister:     &fakeStackedLister{prs: []providers.PullRequestSummary{{Number: 2, Head: "child", Base: "feature"}}},
			wantStatus: "skipped-stacked",
		},
		{name: "list fails", isADO: true, grant: true, headBranch: "feature", lister: &fakeStackedLister{err: errors.New("status 500")}, wantStatus: "failed"},
		{name: "no head branch", isADO: true, grant: true, lister: &fakeStackedLister{}, wantStatus: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := ""
			if tc.grant {
				token = "branch-delete-token"
			}
			t.Setenv(executor.CredentialEnvVar(string(capability.GitHubBranchDelete)), token)
			var skipped *mergeBranchCleanup
			got := adoCompletionDeletesSourceBranch(context.Background(), tc.lister, repo, tc.headBranch, tc.isADO, &skipped)
			if got != tc.want {
				t.Fatalf("deleteSourceBranch = %v, want %v", got, tc.want)
			}
			switch {
			case tc.wantStatus == "" && skipped != nil:
				t.Fatalf("skipped = %+v, want none", skipped)
			case tc.wantStatus != "" && (skipped == nil || skipped.Status != tc.wantStatus):
				t.Fatalf("skipped = %+v, want status %q", skipped, tc.wantStatus)
			case tc.wantStatus == "failed" && !strings.Contains(skipped.Error, "source branch kept"):
				t.Fatalf("skipped error = %q, want it to say the branch is kept", skipped.Error)
			}
			for _, req := range tc.lister.reqs {
				if req.Base != tc.headBranch || req.Repository != repo {
					t.Fatalf("list request = %+v, want pull requests based on %q in %v", req, tc.headBranch, repo)
				}
			}
		})
	}
}
