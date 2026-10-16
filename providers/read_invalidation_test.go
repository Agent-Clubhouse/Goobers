package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type observedMutationClient struct {
	HTTPClient
	kinds []ProviderKind
}

func (c *observedMutationClient) ObserveProviderMutation(_ context.Context, kind ProviderKind) {
	c.kinds = append(c.kinds, kind)
}

func TestMutationObserverSeesConfirmedMergeBeforeReceiptFailure(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-merged", true: "merged"}[merged], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{"merged": merged, "sha": "merged-sha"})
			}))
			defer server.Close()
			client := &observedMutationClient{HTTPClient: server.Client()}
			failure := errors.New("receipt failure")
			p := NewGitHubProvider("token", WithHTTPClient(client), WithMutationRecorder(&receiptFailureRecorder{failure: failure}), func(p *GitHubProvider) { p.BaseURL = server.URL })
			result, err := p.MergePullRequest(t.Context(), MergePullRequestRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", ExpectedHeadSHA: "head"})
			if merged {
				if !errors.Is(err, failure) || !result.Merged || len(client.kinds) != 1 || client.kinds[0] != ProviderGitHub {
					t.Fatalf("lost confirmed effect or receipt failure: %+v %v %v", result, err, client.kinds)
				}
			} else if err != nil || result.Merged || len(client.kinds) != 0 {
				t.Fatalf("unconfirmed merge invalidated: %+v %v %v", result, err, client.kinds)
			}
		})
	}
}

func TestMutationObserverSeesPartialLabelEditWithoutRecorder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		writeJSON(t, w, []any{})
	}))
	defer server.Close()
	client := &observedMutationClient{HTTPClient: server.Client()}
	p := NewGitHubProvider("token", WithHTTPClient(client), func(p *GitHubProvider) { p.BaseURL = server.URL })
	err := p.applyLabelChanges(t.Context(), RepositoryRef{Owner: "acme", Name: "app"}, "9", []string{"ready"}, []string{"blocked"})
	if err == nil || len(client.kinds) != 1 || client.kinds[0] != ProviderGitHub {
		t.Fatalf("partial label effect lost: %v %v", err, client.kinds)
	}
}

func TestMutationObserverIgnoresClaimAttemptTelemetry(t *testing.T) {
	client := &observedMutationClient{HTTPClient: http.DefaultClient}
	recorder := &recordingRecorder{}
	p := NewGitHubProvider("token", WithHTTPClient(client), WithMutationRecorder(recorder))
	for _, outcome := range []string{"failure", "contention"} {
		p.recordExternalRef(t.Context(), ExternalRef{Provider: ProviderGitHub, Operation: "claim", Outcome: outcome})
		ref, ok := recorder.last()
		if !ok || ref.Outcome != outcome || len(client.kinds) != 0 {
			t.Fatalf("attempt changed journal or cache: %+v %v", ref, client.kinds)
		}
	}
}
