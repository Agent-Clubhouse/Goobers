package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubMergeReceiptFieldsWithAndWithoutLandingIntent(t *testing.T) {
	for _, withIntent := range []bool{false, true} {
		t.Run(fmt.Sprintf("intent=%t", withIntent), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{"merged": true, "sha": "merge-sha", "message": "merged"})
			}))
			defer server.Close()

			var recorder MutationRecorder
			var recorded *recordingRecorder
			if withIntent {
				intentRecorder := &intentTestRecorder{}
				recorder = intentRecorder
				recorded = &intentRecorder.recordingRecorder
			} else {
				recorded = &recordingRecorder{}
				recorder = recorded
			}
			provider := NewGitHubProvider("token", WithMutationRecorder(recorder), func(p *GitHubProvider) { p.BaseURL = server.URL })
			result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
				Repository: RepositoryRef{Owner: "Acme", Name: "App"}, PullID: "not-numeric", ExpectedHeadSHA: "head",
			})
			if err != nil {
				t.Fatalf("MergePullRequest returned error: %v", err)
			}
			ref, ok := recorded.last()
			if !ok || ref.Provider != ProviderGitHub || ref.Ref != "Acme/App#not-numeric" || ref.Operation != "merge" {
				t.Fatalf("receipt identity = %+v, recorded=%v", ref, ok)
			}
			if result.Number != 0 || !result.Merged || result.MergeSHA != "merge-sha" || result.Message != "merged" {
				t.Fatalf("result = %+v", result)
			}
			if ref.MergeConfirmation == nil || ref.MergeConfirmation.RepositoryAPIURL != server.URL+"/repos/acme/app" || ref.MergeConfirmation.PullID != "not-numeric" || ref.MergeConfirmation.MergeSHA != "merge-sha" {
				t.Fatalf("confirmation = %+v", ref.MergeConfirmation)
			}
			if got := ref.Fields["state"].After; got != digestString("merged") {
				t.Fatalf("state digest = %q, want %q", got, digestString("merged"))
			}
			if withIntent != (ref.MergeConfirmation.IntentID != "") {
				t.Fatalf("confirmation intent ID = %q, withIntent=%t", ref.MergeConfirmation.IntentID, withIntent)
			}
		})
	}
}

func TestGitHubMergeDoesNotRecordReceiptWhenNotMerged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"merged": false, "message": "not merged"})
	}))
	defer server.Close()

	recorder := &recordingRecorder{}
	provider := NewGitHubProvider("token", WithMutationRecorder(recorder), func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: RepositoryRef{Owner: "Acme", Name: "App"}, PullID: "9",
	})
	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	if result.Merged {
		t.Fatalf("result = %+v, want Merged=false", result)
	}
	if ref, recorded := recorder.last(); recorded {
		t.Fatalf("receipt = %+v, want no recorded receipt", ref)
	}
}

type orderedMergeReceiptRecorder struct {
	intentTestRecorder
	mergeCompleted bool
}

func (r *orderedMergeReceiptRecorder) RecordLandingReceipt(ctx context.Context, ref ExternalRef) error {
	if !r.mergeCompleted {
		return fmt.Errorf("receipt recorded before direct merge completed")
	}
	r.RecordExternalRef(ctx, ref)
	return nil
}

func TestGiteaMergeReceiptFollowsDirectMerge(t *testing.T) {
	recorder := &orderedMergeReceiptRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			recorder.mergeCompleted = true
			w.WriteHeader(http.StatusOK)
			return
		}
		writeJSON(t, w, map[string]any{"number": 0, "merge_commit_sha": "merge-sha"})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token", WithGiteaMutationRecorder(recorder))
	result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: RepositoryRef{Owner: "Acme", Name: "App"}, PullID: "not-numeric", ExpectedHeadSHA: "head",
	})
	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	ref, ok := recorder.last()
	if result.Number != 0 || !result.Merged || result.MergeSHA != "merge-sha" || !ok {
		t.Fatalf("result=%+v receipt=%+v recorded=%v", result, ref, ok)
	}
	if ref.Provider != ProviderGitea || ref.Ref != "Acme/App#not-numeric" || ref.Operation != "merge" || ref.MergeConfirmation == nil || ref.MergeConfirmation.IntentID != recorder.intent.ID {
		t.Fatalf("receipt = %+v, intent = %+v", ref, recorder.intent)
	}
}
