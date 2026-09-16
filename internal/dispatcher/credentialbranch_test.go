package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestOwnedBranchCheckoutHTTPBoundary(t *testing.T) {
	binding := &apiv1.WorkspaceBranchBinding{
		Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "base"},
		Ref:        "refs/heads/ns/workflow/run", StartingSHA: strings.Repeat("a", 40),
	}
	for _, tc := range []struct {
		name, body string
		status     int
		allowed    bool
		anonymous  bool
	}{
		{"read", `{"credentials":[{"capability":"workspace-branch:checkout","value":"fixture-read"}]}`, 200, true, false},
		{"public", `{"credentials":[]}`, 200, true, true},
		{"missing", `{}`, 200, false, false},
		{"null", `{"credentials":null}`, 200, false, false},
		{"broad", `{"credentials":[{"capability":"repo:push","value":"fixture-write"}]}`, 200, false, false},
		{"refused", `{"error":"unauthorized"}`, 403, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					RunID        string                        `json:"runId"`
					Stage        string                        `json:"stage"`
					Binding      *apiv1.WorkspaceBranchBinding `json:"workspaceBranchBinding"`
					Capabilities []string                      `json:"capabilities"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.RunID != "run" || request.Stage != "author" || len(request.Capabilities) != 0 || !reflect.DeepEqual(request.Binding, binding) {
					t.Error("checkout request changed durable ownership")
				}
				if r.Header.Get("Authorization") != "Bearer fixture-pod" {
					t.Error("checkout request did not authenticate its pod")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := &CredentialResolveClient{BaseURL: server.URL, Token: "fixture-pod"}
			creds, err := client.ResolveBranchCheckout(context.Background(), "run", "author", binding)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed = %t, error = %v", tc.allowed, err)
			}
			if tc.allowed && (len(creds) != 1 || creds[0].Anonymous != tc.anonymous || creds[0].Capability != WorkspaceBranchCheckoutCapability) {
				t.Fatal("checkout authorization lost its exclusive scope")
			}
		})
	}
}
