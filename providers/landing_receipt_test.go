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

type receiptFailureRecorder struct {
	intentTestRecorder
	failure error
	receipt ExternalRef
}

func (r *receiptFailureRecorder) RecordLandingReceipt(_ context.Context, ref ExternalRef) error {
	r.receipt = ref
	return r.failure
}

func TestGitHubReceiptFailurePreservesSuccessfulMergeResult(t *testing.T) {
	failure := errors.New("PRIVATE_STORAGE_DIAGNOSTIC")
	recorder := &receiptFailureRecorder{failure: failure}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if !recorder.ready.Load() {
			t.Error("merge preceded intent")
		}
		writeJSON(t, w, map[string]any{"merged": true, "sha": "merged-sha"})
	}))
	defer server.Close()
	p := NewGitHubProvider("fixture", WithMutationRecorder(recorder), func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := p.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", ExpectedHeadSHA: "head"})
	var receiptErr *LandingReceiptError
	if !errors.As(err, &receiptErr) || !errors.Is(err, failure) || !result.Merged || result.MergeSHA != "merged-sha" || calls.Load() != 1 {
		t.Fatalf("receipt failure lost successful outcome: %+v %v calls=%d", result, err, calls.Load())
	}
	if strings.Contains(err.Error(), failure.Error()) {
		t.Fatal("storage detail leaked into public error")
	}
	if recorder.receipt.MergeConfirmation == nil || recorder.receipt.MergeConfirmation.IntentID != recorder.intent.ID {
		t.Fatal("failed receipt lost intent correlation")
	}
}

func TestOtherLandingReceiptFailuresPreserveAcknowledgedOutcome(t *testing.T) {
	for _, scenario := range []string{"ado-merge", "gitea-merge", "ado-enqueue", "github-enqueue"} {
		t.Run(scenario, func(t *testing.T) {
			failure := errors.New("PRIVATE_STORAGE_DIAGNOSTIC")
			r := &receiptFailureRecorder{failure: failure}
			var mutations atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mutating := req.Method != http.MethodGet
				if scenario == "github-enqueue" {
					var body struct{ Query string }
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					mutating = strings.Contains(body.Query, "mutation")
				}
				if mutating {
					mutations.Add(1)
					if !r.ready.Load() {
						t.Error("mutation preceded intent")
					}
				}
				switch scenario {
				case "github-enqueue":
					if mutating {
						writeJSON(t, w, map[string]any{"data": map[string]any{"enqueuePullRequest": map[string]any{"mergeQueueEntry": map[string]any{"id": "entry-42", "enqueuedAt": "2026-09-01T12:00:00Z", "state": "QUEUED"}}}})
					} else {
						writeJSON(t, w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"id": "PR_node", "merged": false}}}})
					}
				case "gitea-merge":
					if !mutating {
						writeJSON(t, w, map[string]any{"number": 42, "merge_commit_sha": "merged"})
					}
				default:
					out := map[string]any{"pullRequestId": 42, "status": "active", "mergeStatus": "succeeded", "createdBy": map[string]string{"id": "creator"}, "lastMergeSourceCommit": map[string]string{"commitId": "head"}}
					if mutating && scenario == "ado-merge" {
						out["status"] = "completed"
						out["lastMergeCommit"] = map[string]string{"commitId": "merged"}
					} else if mutating {
						out["autoCompleteSetBy"] = map[string]string{"id": "creator"}
					}
					writeJSON(t, w, out)
				}
			}))
			defer server.Close()
			var merge MergePullRequestResult
			var enqueue EnqueuePullRequestResult
			var err error
			repo := RepositoryRef{Owner: "acme", Name: "app"}
			switch scenario {
			case "github-enqueue":
				p := NewGitHubProvider("fixture", WithMutationRecorder(r), func(p *GitHubProvider) { p.BaseURL = server.URL })
				enqueue, err = p.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{Repository: repo, PullID: "42", ExpectedHeadSHA: "head"})
			case "gitea-merge":
				p := NewGiteaProvider(server.URL, "fixture", WithGiteaMutationRecorder(r))
				merge, err = p.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: repo, PullID: "42", ExpectedHeadSHA: "head"})
			default:
				p := NewADOProvider("org", "project", "fixture", func(p *ADOProvider) { p.BaseURL = server.URL })
				p.SetMutationRecorder(r)
				if scenario == "ado-merge" {
					merge, err = p.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head"})
				} else {
					enqueue, err = p.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head"})
				}
			}
			var receiptErr *LandingReceiptError
			if !errors.As(err, &receiptErr) || !errors.Is(err, failure) || mutations.Load() != 1 {
				t.Fatalf("lost receipt failure: merge=%+v enqueue=%+v err=%v mutations=%d", merge, enqueue, err, mutations.Load())
			}
			if strings.Contains(err.Error(), failure.Error()) {
				t.Fatal("private storage diagnostic leaked")
			}
			if strings.HasSuffix(scenario, "merge") && !strings.HasSuffix(scenario, "enqueue") {
				if !merge.Merged || merge.MergeSHA != "merged" || r.receipt.MergeConfirmation == nil || r.receipt.MergeConfirmation.IntentID != r.intent.ID {
					t.Fatalf("merge evidence lost: %+v %+v", merge, r.receipt)
				}
			} else if enqueue.Number != 42 || enqueue.Merged || r.receipt.MergeConfirmation != nil {
				t.Fatalf("queue acknowledgement lost or promoted: %+v %+v", enqueue, r.receipt)
			}
			if scenario == "github-enqueue" && (enqueue.QueueEntryID != "entry-42" || r.receipt.QueueAdmission == nil || r.receipt.QueueAdmission.IntentID != r.intent.ID) {
				t.Fatalf("queue identity lost: %+v %+v", enqueue, r.receipt)
			}
			if scenario == "ado-enqueue" && (r.receipt.LandingIntent == nil || r.receipt.LandingIntent.ID != r.intent.ID || r.receipt.QueueAdmission != nil) {
				t.Fatalf("ADO acknowledgement lost or invented queue entry: %+v", r.receipt)
			}
		})
	}
}
