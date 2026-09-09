package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestADOMergeRejectsMismatchedResponseIdentity(t *testing.T) {
	recorder := &intentTestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			writeJSON(t, w, map[string]any{"pullRequestId": 42, "status": "active", "lastMergeSourceCommit": map[string]string{"commitId": "head"}})
			return
		}
		writeJSON(t, w, map[string]any{"pullRequestId": 43, "status": "completed", "mergeStatus": "succeeded", "lastMergeCommit": map[string]string{"commitId": "other-merge"}})
	}))
	defer server.Close()
	p := NewADOProvider("org", "project", "fixture", func(p *ADOProvider) { p.BaseURL = server.URL })
	p.SetMutationRecorder(recorder)
	result, err := p.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head"})
	if err == nil || result.Merged {
		t.Fatalf("mismatched merge accepted: %+v %v", result, err)
	}
	if ref, recorded := recorder.last(); recorded {
		t.Fatalf("mismatched merge receipt persisted: %+v", ref)
	}
	if recorder.intent.PullID != "42" {
		t.Fatalf("original intent lost: %+v", recorder.intent)
	}
}
