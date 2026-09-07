package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderClaimFailuresAreRecorded(t *testing.T) {
	for _, providerID := range []ProviderKind{ProviderGitHub, ProviderGitea, ProviderADO} {
		t.Run(string(providerID), func(t *testing.T) {
			for _, operation := range []string{"claim", "claim-release"} {
				t.Run(operation, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						http.Error(w, "provider denied request secret-canary", http.StatusUnauthorized)
					}))
					defer server.Close()
					recorder := &recordingRecorder{}
					var provider interface {
						ClaimWorkItem(context.Context, ClaimWorkItemRequest) (ClaimResult, error)
						ReleaseWorkItemClaim(context.Context, ClaimWorkItemRequest) (WorkItem, error)
					}
					wantRef := "team/repo#7"
					switch providerID {
					case ProviderGitHub:
						provider = NewGitHubProvider("secret-canary", WithMutationRecorder(recorder), func(p *GitHubProvider) { p.BaseURL = server.URL })
					case ProviderGitea:
						provider = NewGiteaProvider(server.URL, "secret-canary", WithGiteaMutationRecorder(recorder))
					case ProviderADO:
						ado := NewADOProvider("team", "repo", "secret-canary", func(p *ADOProvider) { p.BaseURL = server.URL })
						ado.SetMutationRecorder(recorder)
						provider = ado
						wantRef = "ado#7"
					}
					req := ClaimWorkItemRequest{Repository: RepositoryRef{Provider: providerID, Owner: "team", Name: "repo"}, ID: "7", RunID: "active-run"}
					var err error
					if operation == "claim" {
						_, err = provider.ClaimWorkItem(context.Background(), req)
					} else {
						_, err = provider.ReleaseWorkItemClaim(context.Background(), req)
					}
					if err == nil {
						t.Fatal("denied mutation succeeded")
					}
					ref, ok := recorder.last()
					if !ok {
						t.Fatal("failed claim operation produced no telemetry")
					}
					if ref.Provider != providerID || ref.RunID != "active-run" || ref.Operation != operation || ref.Ref != wantRef {
						t.Fatalf("wrong attribution: %+v", ref)
					}
					data, err := json.Marshal(ref)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(data), "secret-canary") {
						t.Fatalf("provider secret leaked into telemetry: %s", data)
					}
					recorder.mu.Lock()
					count := len(recorder.refs)
					recorder.mu.Unlock()
					if count != 1 {
						t.Fatalf("one denied operation produced %d events", count)
					}
					var fields map[string]any
					if err := json.Unmarshal(data, &fields); err != nil {
						t.Fatal(err)
					}
					if fields["outcome"] != "failure" || fields["errorCode"] != "provider_claim_failed" {
						t.Fatalf("missing structured failure: %s", data)
					}
				})
			}
		})
	}
}
