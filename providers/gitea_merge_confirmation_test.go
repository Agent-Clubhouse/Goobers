package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestGiteaMergeConfirmationRequiresDirectSuccessResponse(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent, http.StatusMethodNotAllowed} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(status)
					return
				}
				// The optional commit-SHA lookup failing cannot erase the
				// merge endpoint's successful direct-mutation receipt.
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			recorder := &recordingRecorder{}
			provider := NewGiteaProvider(server.URL, "token", WithGiteaMutationRecorder(recorder))
			result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9"})
			ref, recorded := recorder.last()
			if status != http.StatusOK {
				if err == nil || result.Merged || recorded {
					t.Fatalf("non-merge status gained confirmation: result=%+v err=%v ref=%+v recorded=%v", result, err, ref, recorded)
				}
				return
			}
			if err != nil || !result.Merged || !recorded || ref.MergeConfirmation == nil || ref.MergeConfirmation.RepositoryAPIURL != server.URL+"/api/v1/repos/acme/app" || ref.MergeConfirmation.PullID != "9" {
				t.Fatalf("successful direct merge lost confirmation: result=%+v err=%v ref=%+v", result, err, ref)
			}
		})
	}
}
