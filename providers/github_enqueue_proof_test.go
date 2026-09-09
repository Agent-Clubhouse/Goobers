package providers

import (
	"context"
	"strings"
	"testing"
)

func TestGitHubEnqueueDoesNotClaimAcceptanceWithoutQueueEntry(t *testing.T) {
	for _, entry := range []any{
		nil,
		map[string]any{"state": "QUEUED", "position": 1},
		map[string]any{"id": "MQE_missing_time", "state": "QUEUED"},
		map[string]any{"id": strings.Repeat("x", 257), "enqueuedAt": "2026-09-01T12:00:00Z"},
		map[string]any{"id": " ", "enqueuedAt": "2026-09-01T12:00:00Z"},
	} {
		stub := &graphQLStub{t: t, lookup: lookupResponse(map[string]any{"id": "PR_node", "merged": false}), mutation: map[string]any{"enqueuePullRequest": map[string]any{"mergeQueueEntry": entry}}}
		server := stub.server()
		recorder := &recordingRecorder{}
		provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL }, WithMutationRecorder(recorder))
		_, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", ExpectedHeadSHA: "head"})
		server.Close()
		if err == nil {
			t.Errorf("enqueue accepted without a returned entry identity: %#v", entry)
		}
		if ref, recorded := recorder.last(); recorded {
			t.Errorf("uncertain enqueue recorded as performed: %+v", ref)
		}
	}
}
