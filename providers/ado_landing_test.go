package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func adoLandingRepo() RepositoryRef {
	return RepositoryRef{Name: "repo", Project: "project"}
}

// TestADOProviderMergePullRequestSucceedsImmediately proves the direct
// completion path (CONF-3 #2076, design doc §4: pr.merge) when ADO's
// completion job resolves synchronously with the PATCH response — no
// internal poll needed.
func TestADOProviderMergePullRequestSucceedsImmediately(t *testing.T) {
	var patched map[string]interface{}
	getCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCalls++
			writeJSON(t, w, map[string]interface{}{
				"pullRequestId": 42, "status": "active", "mergeStatus": "notSet",
				"lastMergeSourceCommit": map[string]string{"commitId": "head1"},
			})
		case http.MethodPatch:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if err := json.Unmarshal(body, &patched); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			writeJSON(t, w, map[string]interface{}{
				"pullRequestId": 42, "status": "completed", "mergeStatus": "succeeded",
				"lastMergeCommit": map[string]string{"commitId": "abc123"},
			})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	recorder := &recordingRecorder{}
	provider.SetMutationRecorder(recorder)
	result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head1", MergeMethod: MergeMethodSquash,
	})
	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	if !result.Merged || result.MergeSHA != "abc123" || result.Number != 42 {
		t.Fatalf("unexpected result: %#v", result)
	}
	ref, recorded := recorder.last()
	if !recorded || ref.MergeConfirmation == nil || ref.MergeConfirmation.RepositoryAPIURL != server.URL+"/org/project/_apis/git/repositories/repo" || ref.MergeConfirmation.PullID != "42" || ref.MergeConfirmation.MergeSHA != "abc123" {
		t.Fatalf("ADO landing confirmation = %+v, recorded=%v", ref, recorded)
	}
	if getCalls != 1 {
		t.Fatalf("GET calls = %d, want 1 (no poll needed when the PATCH response is already terminal)", getCalls)
	}
	opts, ok := patched["completionOptions"].(map[string]interface{})
	if !ok || opts["mergeStrategy"] != "squash" {
		t.Fatalf("completionOptions = %#v, want mergeStrategy=squash", patched["completionOptions"])
	}
	// ADO-N9: the completion PATCH pins the head server-side (live probe F6),
	// so a push between the fetch and the PATCH is refused with 409.
	pin, ok := patched["lastMergeSourceCommit"].(map[string]interface{})
	if !ok || pin["commitId"] != "head1" {
		t.Fatalf("lastMergeSourceCommit = %#v, want commitId=head1", patched["lastMergeSourceCommit"])
	}
	if _, present := patched["bypassPolicy"]; present {
		t.Fatalf("PATCH body must never set bypassPolicy: %#v", patched)
	}
}

// adoCompletionRefusalServer serves an active PR at head1 and answers the
// completion PATCH with status and body, counting PATCH calls.
func adoCompletionRefusalServer(t *testing.T, status int, body string, patchCalls *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			*patchCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		writeJSON(t, w, map[string]interface{}{
			"pullRequestId": 42, "status": "active", "mergeStatus": "succeeded",
			"lastMergeSourceCommit": map[string]string{"commitId": "head1"},
		})
	})
	return httptest.NewServer(mux)
}

// TestADOProviderMergePullRequestMapsStaleHeadConflict proves ADO-N9's 409
// mapping: a head that moves after the fetch is refused server-side with
// TF401192, which surfaces as the typed head-moved error without a retry.
func TestADOProviderMergePullRequestMapsStaleHeadConflict(t *testing.T) {
	patchCalls := 0
	server := adoCompletionRefusalServer(t, http.StatusConflict,
		`{"message":"TF401192: The pull request source has changed.","typeKey":"GitPullRequestStaleException"}`, &patchCalls)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head1", MergeMethod: MergeMethodSquash,
	})
	var moved PullRequestHeadMovedError
	if !errors.As(err, &moved) || moved.Expected != "head1" {
		t.Fatalf("err = %v, want PullRequestHeadMovedError{Expected: head1}", err)
	}
	if patchCalls != 1 {
		t.Fatalf("PATCH calls = %d, want 1 (never retried)", patchCalls)
	}
}

// TestADOProviderMergePullRequestMapsPolicyRejection proves ADO-N9's 403
// mapping: a completion refused because a branch policy is not met is a
// typed policy-not-met error, not an authentication failure, and is never
// retried.
func TestADOProviderMergePullRequestMapsPolicyRejection(t *testing.T) {
	patchCalls := 0
	server := adoCompletionRefusalServer(t, http.StatusForbidden,
		`{"message":"The pull request cannot be completed because required policies are not satisfied.","typeKey":"GitPullRequestUpdateRejectedByPolicyException"}`, &patchCalls)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head1", MergeMethod: MergeMethodSquash,
	})
	var policy PullRequestPolicyNotMetError
	if !errors.As(err, &policy) || policy.PullID != "42" {
		t.Fatalf("err = %v, want PullRequestPolicyNotMetError for PR 42", err)
	}
	if IsAuthenticationError(err) {
		t.Fatalf("IsAuthenticationError(%v) = true, want false for a policy refusal", err)
	}
	if IsTransientError(err) {
		t.Fatalf("IsTransientError(%v) = true, want false", err)
	}
	if patchCalls != 1 {
		t.Fatalf("PATCH calls = %d, want 1 (never retried)", patchCalls)
	}
}

// TestADOProviderMergePullRequestKeepsPlainForbiddenAsAuth pins the boundary
// of the 403 mapping: a 403 that is not a policy refusal remains an
// authentication failure.
func TestADOProviderMergePullRequestKeepsPlainForbiddenAsAuth(t *testing.T) {
	patchCalls := 0
	server := adoCompletionRefusalServer(t, http.StatusForbidden,
		`{"message":"TF401027: You need the Git 'PullRequestContribute' permission.","typeKey":"GitNeedsPermissionException"}`, &patchCalls)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head1", MergeMethod: MergeMethodSquash,
	})
	var policy PullRequestPolicyNotMetError
	if errors.As(err, &policy) || !IsAuthenticationError(err) {
		t.Fatalf("err = %v, want a plain authentication failure", err)
	}
}

// TestADOProviderMergePullRequestRejectsMovedHead pins the pre-PATCH head guard:
// when the fetched detail's source head no longer matches ExpectedHeadSHA, the
// merge is refused before any completion PATCH.
func TestADOProviderMergePullRequestRejectsMovedHead(t *testing.T) {
	patchCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCalls++
		}
		writeJSON(t, w, map[string]interface{}{
			"pullRequestId": 42, "status": "active", "mergeStatus": "notSet",
			"lastMergeSourceCommit": map[string]string{"commitId": "head-moved"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head1", MergeMethod: MergeMethodSquash,
	})
	var moved PullRequestHeadMovedError
	if !errors.As(err, &moved) || moved.Expected != "head1" || moved.Actual != "head-moved" {
		t.Fatalf("err = %v, want PullRequestHeadMovedError{head1 -> head-moved}", err)
	}
	if patchCalls != 0 {
		t.Fatalf("PATCH calls = %d, want 0 (refuse before completing)", patchCalls)
	}
}

// TestADOProviderMergePullRequestIsIdempotent proves §4's idempotency
// obligation: a second MergePullRequest call against an already-completed
// pull request observes the existing landing rather than re-PATCHing.
func TestADOProviderMergePullRequestIsIdempotent(t *testing.T) {
	patchCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCalls++
		}
		writeJSON(t, w, map[string]interface{}{
			"pullRequestId": 42, "status": "completed", "mergeStatus": "succeeded",
			"lastMergeCommit": map[string]string{"commitId": "already123"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	recorder := &recordingRecorder{}
	provider.SetMutationRecorder(recorder)
	result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: adoLandingRepo(), PullID: "42"})
	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	if !result.Merged || result.MergeSHA != "already123" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if patchCalls != 0 {
		t.Fatalf("PATCH called %d times, want 0 (idempotent observe, never re-mutate)", patchCalls)
	}
	if ref, recorded := recorder.last(); recorded {
		t.Fatalf("observed existing merge gained mutation provenance: %+v", ref)
	}
}

// TestADOProviderMergePullRequestReportsConflict proves a confirmed
// mergeStatus="conflicts" classifies as IsMergeConflictError (issue #1751
// parity) even though ADO reports it via a 200 response body field, not an
// HTTP status code like GitHub.
func TestADOProviderMergePullRequestReportsConflict(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, map[string]interface{}{"pullRequestId": 42, "status": "active", "mergeStatus": "notSet"})
			return
		}
		writeJSON(t, w, map[string]interface{}{"pullRequestId": 42, "status": "active", "mergeStatus": "conflicts"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: adoLandingRepo(), PullID: "42"})
	if err == nil {
		t.Fatal("expected an error for a conflicted merge")
	}
	if !IsMergeConflictError(err) {
		t.Fatalf("IsMergeConflictError(%v) = false, want true", err)
	}
}

// TestADOProviderMergePullRequestPollsUntilTerminal proves MergePullRequest
// waits out ADO's asynchronous completion job (CONF-3 #2076): unlike
// GitHub's synchronous merge endpoint, a PATCH response reporting
// mergeStatus="queued" is not terminal — the caller must keep polling.
func TestADOProviderMergePullRequestPollsUntilTerminal(t *testing.T) {
	getCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCalls++
			if getCalls == 1 {
				writeJSON(t, w, map[string]interface{}{"pullRequestId": 42, "status": "active", "mergeStatus": "notSet"})
				return
			}
			if getCalls == 2 {
				writeJSON(t, w, map[string]interface{}{"pullRequestId": 42, "status": "active", "mergeStatus": "queued"})
				return
			}
			writeJSON(t, w, map[string]interface{}{
				"pullRequestId": 42, "status": "completed", "mergeStatus": "succeeded",
				"lastMergeCommit": map[string]string{"commitId": "xyz789"},
			})
		case http.MethodPatch:
			writeJSON(t, w, map[string]interface{}{"pullRequestId": 42, "status": "active", "mergeStatus": "queued"})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	provider.sleep = func(context.Context, time.Duration) error { return nil }
	result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: adoLandingRepo(), PullID: "42"})
	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	if !result.Merged || result.MergeSHA != "xyz789" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if getCalls != 3 {
		t.Fatalf("GET calls = %d, want 3 (initial check + 2 polls to reach succeeded)", getCalls)
	}
}

// TestADOProviderMergePullRequestTimesOutWhenNeverTerminal proves the
// bounded poll gives up rather than hanging forever on a completion job
// that never resolves.
func TestADOProviderMergePullRequestTimesOutWhenNeverTerminal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"pullRequestId": 42, "status": "active", "mergeStatus": "queued"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	provider.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{Repository: adoLandingRepo(), PullID: "42"})
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
}

// TestADOProviderEnqueuePullRequestSetsAutoComplete proves the enqueue
// path (CONF-3 #2076, design doc §4: pr.landing.enqueue) PATCHes
// autoCompleteSetBy and completionOptions rather than attempting an
// immediate completion.
func TestADOProviderEnqueuePullRequestSetsAutoComplete(t *testing.T) {
	var patched map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{
				"pullRequestId": 42, "status": "active", "mergeStatus": "notSet",
				"createdBy": map[string]string{"id": "creator-1"},
			})
		case http.MethodPatch:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if err := json.Unmarshal(body, &patched); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			writeJSON(t, w, map[string]interface{}{
				"pullRequestId": 42, "status": "active", "mergeStatus": "notSet",
				"autoCompleteSetBy": map[string]string{"id": "creator-1"},
			})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{
		Repository: adoLandingRepo(), PullID: "42", ExpectedHeadSHA: "head1", MergeMethod: MergeMethodMerge,
	})
	if err != nil {
		t.Fatalf("EnqueuePullRequest returned error: %v", err)
	}
	if result.Merged {
		t.Fatalf("Merged = true, want false (enqueue never merges inline): %#v", result)
	}
	setBy, ok := patched["autoCompleteSetBy"].(map[string]interface{})
	if !ok || setBy["id"] != "creator-1" {
		t.Fatalf("autoCompleteSetBy = %#v, want id=creator-1", patched["autoCompleteSetBy"])
	}
	opts, ok := patched["completionOptions"].(map[string]interface{})
	if !ok || opts["mergeStrategy"] != "noFastForward" {
		t.Fatalf("completionOptions = %#v, want mergeStrategy=noFastForward", patched["completionOptions"])
	}
}

// TestADOProviderEnqueuePullRequestIsIdempotent proves §4's idempotency
// obligation for enqueue: a pull request that already has auto-complete
// armed is observed rather than re-PATCHed.
func TestADOProviderEnqueuePullRequestIsIdempotent(t *testing.T) {
	patchCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCalls++
		}
		writeJSON(t, w, map[string]interface{}{
			"pullRequestId": 42, "status": "active", "mergeStatus": "notSet",
			"autoCompleteSetBy": map[string]string{"id": "someone"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{Repository: adoLandingRepo(), PullID: "42"})
	if err != nil {
		t.Fatalf("EnqueuePullRequest returned error: %v", err)
	}
	if result.Merged {
		t.Fatalf("Merged = true, want false: %#v", result)
	}
	if patchCalls != 0 {
		t.Fatalf("PATCH called %d times, want 0 (idempotent observe)", patchCalls)
	}
}

// TestADOProviderEnqueuePullRequestAlreadyMergedIsNotAnError mirrors
// GitHub's identical edge case: a retried enqueue against a pull request
// the queue already landed reports Merged=true, not "enqueued".
func TestADOProviderEnqueuePullRequestAlreadyMergedIsNotAnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"pullRequestId": 42, "status": "completed", "mergeStatus": "succeeded",
			"lastMergeCommit": map[string]string{"commitId": "landed1"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{Repository: adoLandingRepo(), PullID: "42"})
	if err != nil {
		t.Fatalf("EnqueuePullRequest returned error: %v", err)
	}
	if !result.Merged || result.MergeSHA != "landed1" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

// TestADOProviderPollMergeQueueEntryStates covers every classification
// PollMergeQueueEntry reports (CONF-3 #2076, design doc §4: pr.landing.poll
// is the sole landed-oracle; eviction is first-class).
func TestADOProviderPollMergeQueueEntryStates(t *testing.T) {
	tests := []struct {
		name      string
		response  map[string]interface{}
		wantState MergeQueueEntryState
		wantSHA   string
	}{
		{
			name: "completed is merged",
			response: map[string]interface{}{
				"pullRequestId": 42, "status": "completed",
				"lastMergeCommit": map[string]string{"commitId": "merged1"},
			},
			wantState: MergeQueueEntryMerged, wantSHA: "merged1",
		},
		{
			name: "active with auto-complete armed is pending",
			response: map[string]interface{}{
				"pullRequestId": 42, "status": "active",
				"autoCompleteSetBy": map[string]string{"id": "someone"},
			},
			wantState: MergeQueueEntryPending,
		},
		{
			name: "active with auto-complete cleared is evicted (policy rejection)",
			response: map[string]interface{}{
				"pullRequestId": 42, "status": "active",
			},
			wantState: MergeQueueEntryEvicted,
		},
		{
			name: "abandoned is evicted",
			response: map[string]interface{}{
				"pullRequestId": 42, "status": "abandoned",
			},
			wantState: MergeQueueEntryEvicted,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, tc.response)
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			result, err := provider.PollMergeQueueEntry(context.Background(), PollMergeQueueEntryRequest{Repository: adoLandingRepo(), PullID: "42"})
			if err != nil {
				t.Fatalf("PollMergeQueueEntry returned error: %v", err)
			}
			if result.State != tc.wantState {
				t.Fatalf("State = %q, want %q", result.State, tc.wantState)
			}
			if result.MergeSHA != tc.wantSHA {
				t.Fatalf("MergeSHA = %q, want %q", result.MergeSHA, tc.wantSHA)
			}
		})
	}
}

// TestADOProviderDetectMergePolicy proves the branch-policy read (CONF-3
// #2076, design doc §4: pr.landing.detect-policy) maps an enabled,
// blocking policy scoped to the target branch to MergePolicyMergeQueue,
// and everything else to MergePolicyDirect.
func TestADOProviderDetectMergePolicy(t *testing.T) {
	tests := []struct {
		name    string
		configs []map[string]interface{}
		want    MergePolicy
	}{
		{name: "no policies is direct", configs: nil, want: MergePolicyDirect},
		{
			name: "blocking policy scoped to branch is merge_queue",
			configs: []map[string]interface{}{{
				"isEnabled": true, "isBlocking": true, "isDeleted": false,
				"settings": map[string]interface{}{"scope": []map[string]string{{"refName": "refs/heads/main"}}},
			}},
			want: MergePolicyMergeQueue,
		},
		{
			name: "non-blocking policy is direct",
			configs: []map[string]interface{}{{
				"isEnabled": true, "isBlocking": false, "isDeleted": false,
				"settings": map[string]interface{}{"scope": []map[string]string{{"refName": "refs/heads/main"}}},
			}},
			want: MergePolicyDirect,
		},
		{
			name: "blocking policy scoped to a different branch is direct",
			configs: []map[string]interface{}{{
				"isEnabled": true, "isBlocking": true, "isDeleted": false,
				"settings": map[string]interface{}{"scope": []map[string]string{{"refName": "refs/heads/release"}}},
			}},
			want: MergePolicyDirect,
		},
		{
			name: "deleted blocking policy is direct",
			configs: []map[string]interface{}{{
				"isEnabled": true, "isBlocking": true, "isDeleted": true,
				"settings": map[string]interface{}{"scope": []map[string]string{{"refName": "refs/heads/main"}}},
			}},
			want: MergePolicyDirect,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/policy/configurations", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, map[string]interface{}{"value": tc.configs})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			result, err := provider.DetectMergePolicy(context.Background(), RepoMergePolicyRequest{Repository: adoLandingRepo(), Branch: "main"})
			if err != nil {
				t.Fatalf("DetectMergePolicy returned error: %v", err)
			}
			if result.Policy != tc.want {
				t.Fatalf("Policy = %q, want %q", result.Policy, tc.want)
			}
		})
	}
}

// TestADOProviderCompareCommits proves the diffs/commits mapping (CONF-3
// #2076, design doc §4: pr.compare) — the merge-base comes directly from
// the response's commonCommit field, no second lookup.
func TestADOProviderCompareCommits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/diffs/commits", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"commonCommit": "base123",
			"changes": []map[string]interface{}{
				{"changeType": "add", "item": map[string]interface{}{"path": "/new.go"}},
				{"changeType": "edit", "item": map[string]interface{}{"path": "/existing.go"}},
				{"changeType": "edit", "item": map[string]interface{}{"path": "/dir", "isFolder": true}},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.CompareCommits(context.Background(), adoLandingRepo(), "base123", "head456")
	if err != nil {
		t.Fatalf("CompareCommits returned error: %v", err)
	}
	if result.MergeBaseSHA != "base123" {
		t.Fatalf("MergeBaseSHA = %q, want base123", result.MergeBaseSHA)
	}
	if len(result.Files) != 2 {
		t.Fatalf("Files = %#v, want 2 entries (folder excluded)", result.Files)
	}
	if result.Files[0].Path != "new.go" || result.Files[0].Status != "added" {
		t.Fatalf("Files[0] = %#v, want path=new.go status=added", result.Files[0])
	}
}

// TestADOProviderDeleteBranchWithExpectedSHA proves the lease-guarded path
// (CONF-3 #2076, design doc §4: branch.delete ≙ ref update to zeroed
// object id).
func TestADOProviderDeleteBranchWithExpectedSHA(t *testing.T) {
	var posted []map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeJSON(t, w, map[string]interface{}{"value": []map[string]string{{"name": "refs/heads/feature", "objectId": adoZeroObjectID}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{Repository: adoLandingRepo(), Name: "feature", ExpectedSHA: "sha1"})
	if err != nil {
		t.Fatalf("DeleteBranch returned error: %v", err)
	}
	if !result.Deleted {
		t.Fatalf("Deleted = false, want true")
	}
	if len(posted) != 1 || posted[0]["oldObjectId"] != "sha1" || posted[0]["newObjectId"] != adoZeroObjectID {
		t.Fatalf("posted body = %#v, want oldObjectId=sha1 newObjectId=%s", posted, adoZeroObjectID)
	}
}

// TestADOProviderDeleteBranchLooksUpSHAWhenUnconditional proves an
// unconditional delete (no ExpectedSHA) resolves the branch's live tip
// first — ADO's ref-update rejects an update whose oldObjectId doesn't
// match the ref's current value, so even an unconditional delete needs a
// real one.
func TestADOProviderDeleteBranchLooksUpSHAWhenUnconditional(t *testing.T) {
	var postedOldObjectID string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{"value": []map[string]string{{"name": "refs/heads/feature", "objectId": "live-sha"}}})
		case http.MethodPost:
			var posted []map[string]string
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			postedOldObjectID = posted[0]["oldObjectId"]
			writeJSON(t, w, map[string]interface{}{"value": []map[string]string{{"name": "refs/heads/feature", "objectId": adoZeroObjectID}}})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{Repository: adoLandingRepo(), Name: "feature"})
	if err != nil {
		t.Fatalf("DeleteBranch returned error: %v", err)
	}
	if !result.Deleted {
		t.Fatalf("Deleted = false, want true")
	}
	if postedOldObjectID != "live-sha" {
		t.Fatalf("posted oldObjectId = %q, want live-sha (resolved via lookup)", postedOldObjectID)
	}
}

// TestADOProviderDeleteBranchAlreadyAbsentIsNotAnError mirrors GitHub's
// 404-is-not-an-error DeleteBranch semantics: an unconditional delete of a
// branch that no longer exists reports Deleted=false, not an error.
func TestADOProviderDeleteBranchAlreadyAbsentIsNotAnError(t *testing.T) {
	postCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalled = true
		}
		writeJSON(t, w, map[string]interface{}{"value": []map[string]string{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{Repository: adoLandingRepo(), Name: "feature"})
	if err != nil {
		t.Fatalf("DeleteBranch returned error: %v", err)
	}
	if result.Deleted {
		t.Fatalf("Deleted = true, want false")
	}
	if postCalled {
		t.Fatal("POST refs called for an already-absent branch, want no mutation attempted")
	}
}

// TestADOProviderDeleteBranchLeaseLostReportsBranchTipChanged proves a
// caller-supplied ExpectedSHA that the ref-update batch doesn't confirm
// surfaces as BranchTipChangedError, matching GitHub's stale-lease
// classification.
func TestADOProviderDeleteBranchLeaseLostReportsBranchTipChanged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"value": []map[string]string{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{Repository: adoLandingRepo(), Name: "feature", ExpectedSHA: "stale-sha"})
	var tipChanged *BranchTipChangedError
	if !errors.As(err, &tipChanged) {
		t.Fatalf("error = %v, want *BranchTipChangedError", err)
	}
}
