package providers

import (
	"context"
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
