package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestMergeReviewADOProviderNeverUsesWorkItemAPIForPRIDs(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/labels") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"value":[{"id":"l1","name":"goobers:merge-demoted"}]}`))
		case strings.Contains(r.URL.Path, "/labels") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"l2","name":"x"}`))
		case strings.HasSuffix(r.URL.Path, "/threads") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"value":[{"id":3,"comments":[{"id":1,"content":"sticky","commentType":"text"}]}]}`))
		case strings.HasSuffix(r.URL.Path, "/threads") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":3,"comments":[{"id":1,"content":"c"}]}`))
		case strings.HasSuffix(r.URL.Path, "/threads/3/comments/1") && r.Method == http.MethodPatch:
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["content"] != "updated sticky" {
				t.Errorf("sticky update body = %v, err = %v", body, err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = w.Write([]byte(`{"value":[]}`))
		}
	}))
	t.Cleanup(server.Close)
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "proj", Name: "repo"}
	provider := mergeReviewADOProvider{ADOProvider: providers.NewADOProvider("org", "proj", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})}
	ctx := context.Background()

	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: "42",
		AddLabels: []string{needsRemediationLabel}, RemoveLabels: []string{mergeDemotedLabel}, Comment: "body",
	}); err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	comments, err := provider.ListComments(ctx, repo, "42")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 1 || comments[0].ID != "42/3/1" || comments[0].Body != "sticky" {
		t.Fatalf("ListComments = %+v, want the PR thread comment and its composite ID", comments)
	}
	if err := postOrUpdateStickyComment(ctx, provider, repo, 42, comments[0].ID, "updated sticky"); err != nil {
		t.Fatalf("update sticky comment: %v", err)
	}
	if err := postOrUpdateStickyComment(ctx, provider, repo, 42, "", "sticky"); err != nil {
		t.Fatalf("postOrUpdateStickyComment: %v", err)
	}
	if err := provider.DeleteComment(ctx, repo, comments[0].ID); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	const prPath = "/org/proj/_apis/git/repositories/repo/pullrequests/42"
	want := []string{
		"GET " + prPath + "/labels",
		"DELETE " + prPath + "/labels/l1",
		"GET " + prPath + "/labels",
		"POST " + prPath + "/labels",
		"POST " + prPath + "/threads",
		"GET " + prPath + "/threads",
		"PATCH " + prPath + "/threads/3/comments/1",
		"POST " + prPath + "/threads",
		"DELETE " + prPath + "/threads/3/comments/1",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("API calls = %v, want only PR label/thread calls %v", calls, want)
	}
}

func TestMergeReviewADOProviderStopsOnUpdateFailure(t *testing.T) {
	const prPath = "/org/proj/_apis/git/repositories/repo/pullrequests/42"
	for _, failAt := range []string{"DELETE " + prPath + "/labels/l1", "POST " + prPath + "/labels", "POST " + prPath + "/threads"} {
		t.Run(failAt, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				call := r.Method + " " + r.URL.Path
				calls = append(calls, call)
				if call == failAt {
					http.Error(w, "refused PR mutation", http.StatusBadRequest)
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == prPath+"/labels" {
					_, _ = w.Write([]byte(`{"value":[{"id":"l1","name":"goobers:merge-demoted"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(server.Close)
			provider := mergeReviewADOProvider{ADOProvider: providers.NewADOProvider("org", "proj", "token", func(p *providers.ADOProvider) {
				p.BaseURL = server.URL
			})}
			_, err := provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
				Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "proj", Name: "repo"}, ID: "42",
				AddLabels: []string{needsRemediationLabel}, RemoveLabels: []string{mergeDemotedLabel}, Comment: "body",
			})
			if err == nil || !strings.Contains(err.Error(), "refused PR mutation") {
				t.Fatalf("UpdateWorkItem error = %v, want PR mutation failure", err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, call := range calls {
				if strings.Contains(call, "/_apis/wit/") {
					t.Errorf("work-item API called for a PR id: %s", call)
				}
			}
			if calls[len(calls)-1] != failAt {
				t.Errorf("continued mutating after failure at %s: %v", failAt, calls)
			}
		})
	}
}
