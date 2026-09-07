package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubMergeMutationRequiresPositiveMergeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   map[string]interface{}
		merged bool
	}{
		{name: "merged", body: map[string]interface{}{"merged": true, "sha": "merge-commit"}, merged: true},
		{name: "not merged", body: map[string]interface{}{"merged": false, "message": "not merged"}},
		{name: "missing evidence", body: map[string]interface{}{"message": "no merge result"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertMethod(t, r, http.MethodPut)
				writeJSON(t, w, tc.body)
			}))
			defer server.Close()
			recorder := &recordingRecorder{}
			provider := NewGitHubProvider("fixture-token", func(p *GitHubProvider) { p.BaseURL = server.URL }, WithMutationRecorder(recorder))
			result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9"})
			if err != nil || result.Merged != tc.merged {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			ref, recorded := recorder.last()
			if recorded != tc.merged {
				t.Fatalf("recorded a merge without positive evidence: result=%+v ref=%+v recorded=%v", result, ref, recorded)
			}
			if tc.merged && (ref.MergeConfirmation == nil || ref.MergeConfirmation.RepositoryAPIURL != server.URL+"/repos/acme/app" || ref.MergeConfirmation.PullID != "9" || ref.MergeConfirmation.MergeSHA != "merge-commit") {
				t.Fatalf("merge lost repository/PR/commit confirmation: %+v", ref)
			}
		})
	}
}
