package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type intentTestRecorder struct {
	recordingRecorder
	intent LandingIntent
	err    error
	ready  atomic.Bool
}

func (r *intentTestRecorder) RecordLandingIntent(_ context.Context, _ ProviderKind, intent LandingIntent) error {
	r.intent = intent
	if r.err == nil {
		r.ready.Store(true)
	}
	return r.err
}

func TestGitHubLandingIntentPrecedesMutationAndFailurePreventsHTTP(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted", true: "persistence failure"}[fail], func(t *testing.T) {
			r := &intentTestRecorder{}
			failure := errors.New("intent storage unavailable")
			if fail {
				r.err = failure
			}
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if !r.ready.Load() {
					t.Error("merge request preceded durable intent")
				}
				writeJSON(t, w, map[string]any{"merged": true, "sha": "merged-sha"})
			}))
			defer server.Close()
			p := NewGitHubProvider("fixture", WithMutationRecorder(r), func(p *GitHubProvider) { p.BaseURL = server.URL })
			result, err := p.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", ExpectedHeadSHA: "expected-head"})
			if fail {
				if !errors.Is(err, failure) || result.Merged || calls.Load() != 0 {
					t.Fatalf("failed intent did not prevent merge: %+v %v calls=%d", result, err, calls.Load())
				}
				return
			}
			if err != nil || !result.Merged || calls.Load() != 1 {
				t.Fatalf("merge=%+v err=%v calls=%d", result, err, calls.Load())
			}
			ref, ok := r.last()
			if !ok || ref.MergeConfirmation == nil || len(r.intent.ID) != 32 || ref.MergeConfirmation.IntentID != r.intent.ID || r.intent.ExpectedHeadSHA != "expected-head" || r.intent.RepositoryAPIURL != server.URL+"/repos/acme/app" {
				t.Fatalf("intent/confirmation mismatch: %+v %+v", r.intent, ref)
			}
		})
	}
}
