package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestSharedVisibilityOnlyChangesClaimedLabel(t *testing.T) {
	var writes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer issues-token" {
			t.Error("visibility did not use issue credentials")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo/issues/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "labels": []map[string]string{{"name": LabelClaimed}, {"name": "feature"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/repo/issues/42/labels":
			var body map[string][]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !reflect.DeepEqual(body, map[string][]string{"labels": {LabelClaimed}}) {
				t.Errorf("unexpected label write: %v", body)
			}
			writes = append(writes, "add")
			_, _ = w.Write([]byte("[]"))
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/repo/issues/42/labels/"+LabelClaimed:
			writes = append(writes, "remove")
			w.WriteHeader(http.StatusNotFound) // An already absent label is success.
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	v := GitHubSharedClaimVisibility{
		Provider:   NewGitHubProvider("issues-token", func(p *GitHubProvider) { p.BaseURL = server.URL; p.maxRetries = 0 }),
		Repository: RepositoryRef{Provider: ProviderGitHub, Owner: "acme", Name: "repo"},
	}
	if present, err := v.ReadClaimed(t.Context(), "42"); err != nil || !present {
		t.Fatalf("read visibility: %v %v", present, err)
	}
	for _, present := range []bool{true, false} {
		if err := v.SetClaimed(t.Context(), "42", present); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(writes, []string{"add", "remove"}) {
		t.Fatalf("writes: %v", writes)
	}
}

func TestSharedVisibilityRejectsNonIssueKeysBeforeIO(t *testing.T) {
	v := GitHubSharedClaimVisibility{Provider: NewGitHubProvider(""), Repository: RepositoryRef{Provider: ProviderGitHub, Owner: "acme", Name: "repo"}}
	for _, key := range []string{"", "0", "042", "+42", "-1", "42/labels", "pr/42", "decomposition-target:42", "18446744073709551616"} {
		if _, err := v.ReadClaimed(t.Context(), key); err == nil {
			t.Errorf("read accepted %q", key)
		}
		if err := v.SetClaimed(t.Context(), key, true); err == nil {
			t.Errorf("write accepted %q", key)
		}
	}
}
