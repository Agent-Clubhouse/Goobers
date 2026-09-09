package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGitHubQueueIntentPrecedesAdmission(t *testing.T) {
	for _, scenario := range []string{"admitted", "storage-failure", "already-queued"} {
		t.Run(scenario, func(t *testing.T) {
			r := &intentTestRecorder{}
			failure := errors.New("intent unavailable")
			if scenario == "storage-failure" {
				r.err = failure
			}
			var mutations atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					Query string `json:"query"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if strings.Contains(body.Query, "mutation") {
					mutations.Add(1)
					if !r.ready.Load() {
						t.Error("enqueue preceded durable intent")
					}
					writeJSON(t, w, map[string]any{"data": map[string]any{"enqueuePullRequest": map[string]any{"mergeQueueEntry": map[string]any{"id": "entry-9", "enqueuedAt": "2026-09-01T12:00:00Z", "state": "QUEUED", "position": 1}}}})
					return
				}
				pr := map[string]any{"id": "PR_node", "merged": false}
				if scenario == "already-queued" {
					pr["mergeQueueEntry"] = map[string]any{"state": "QUEUED", "position": 1}
				}
				writeJSON(t, w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pr}}})
			}))
			defer server.Close()
			p := NewGitHubProvider("fixture", WithMutationRecorder(r), func(p *GitHubProvider) { p.BaseURL = server.URL })
			result, err := p.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", ExpectedHeadSHA: "head"})
			ref, recorded := r.last()
			if scenario != "admitted" {
				if mutations.Load() != 0 || recorded || result.Merged || result.QueueEntryID != "" {
					t.Fatalf("unexpected mutation or ownership: %+v %+v calls=%d", result, ref, mutations.Load())
				}
				if scenario == "storage-failure" && !errors.Is(err, failure) {
					t.Fatalf("lost persistence error: %v", err)
				}
				if scenario == "already-queued" && (err != nil || r.intent.ID != "") {
					t.Fatalf("observation created intent: %+v %v", r.intent, err)
				}
				return
			}
			if err != nil || result.Merged || mutations.Load() != 1 || !recorded || ref.MergeConfirmation != nil || ref.QueueAdmission == nil {
				t.Fatalf("bad admission: %+v %+v %v", result, ref, err)
			}
			if r.intent.Operation != "enqueue" || len(r.intent.ID) != 32 || ref.QueueAdmission.IntentID != r.intent.ID || ref.QueueAdmission.EntryID != result.QueueEntryID || r.intent.ExpectedHeadSHA != "head" {
				t.Fatalf("intent/receipt mismatch: %+v %+v", r.intent, ref.QueueAdmission)
			}
		})
	}
}
