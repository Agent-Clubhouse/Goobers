package providers

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestADOReadBranchHeadMatchesExactRefUsingDeliveredIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body, sha string
		wantErr         bool
	}{
		{"sibling-only", `{"value":[{"name":"refs/heads/repair-old","objectId":"sibling"}]}`, "", false},
		{"exact-after-sibling", `{"value":[{"name":"refs/heads/repair-old","objectId":"sibling"},{"name":"refs/heads/repair","objectId":"exact"}]}`, "exact", false},
		{"duplicate", `{"value":[{"name":"refs/heads/repair","objectId":"one"},{"name":"refs/heads/repair","objectId":"two"}]}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := NewADODeliveredCredentialSource(ADOCredentialKindBearer, "human-token", "restart")
			if err != nil {
				t.Fatal(err)
			}
			p := NewADOProvider("org", "project", "automation-token", WithADOCredentialSource(source))
			p.Client = adoHTTPClientFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer human-token" || r.URL.Query().Get("filter") != "heads/repair" {
					t.Fatalf("wrong source or query: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			sha, found, err := p.ReadBranchHead(t.Context(), RepositoryRef{Provider: ProviderADO, Owner: "org", Project: "project", Name: "repo"}, "repair")
			if (err != nil) != tc.wantErr || sha != tc.sha || found != (tc.sha != "") {
				t.Fatalf("head=%q found=%t err=%v", sha, found, err)
			}
		})
	}
}
