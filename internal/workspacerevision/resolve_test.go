package workspacerevision

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestResolveVerifiesNamesIDsAndRepositoryURLs(t *testing.T) {
	for _, kind := range []apiv1.Provider{apiv1.ProviderGitHub, apiv1.ProviderGitea, apiv1.ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			configured := apiv1.RepoRef{Provider: kind, BaseURL: "https://forge.test/prefix", Owner: "org", Name: "repo", Branch: "main",
				Checkout: &apiv1.CheckoutSpec{Sparse: []string{"src"}}}
			if kind == apiv1.ProviderADO {
				configured.Project = "project"
			}
			metadata, _ := lookupFixture(context.Background(), configured)
			identity := apiv1.RepositoryIdentity{Provider: kind, Owner: "org", Project: configured.Project, Name: "repo", ID: "17", URL: metadata.Repository.URL}
			for _, tc := range []struct {
				name  string
				edit  func(*apiv1.RepositoryIdentity)
				valid bool
			}{
				{"repository URL", func(*apiv1.RepositoryIdentity) {}, true},
				{"service root", func(i *apiv1.RepositoryIdentity) { i.URL = configured.BaseURL }, true},
				{"clone URL", func(i *apiv1.RepositoryIdentity) { i.URL += ".git" }, true},
				{"default port", func(i *apiv1.RepositoryIdentity) { i.URL = strings.Replace(i.URL, "forge.test", "FORGE.TEST:443", 1) }, true},
				{"wrong ID", func(i *apiv1.RepositoryIdentity) { i.ID = "forged" }, false},
				{"wrong name", func(i *apiv1.RepositoryIdentity) { i.Name = "forged" }, false},
				{"wrong project", func(i *apiv1.RepositoryIdentity) { i.Project = "forged" }, false},
				{"wrong repo path", func(i *apiv1.RepositoryIdentity) { i.URL += "/other" }, false},
				{"missing prefix", func(i *apiv1.RepositoryIdentity) { i.URL = strings.Replace(i.URL, "/prefix", "", 1) }, false},
				{"wrong host", func(i *apiv1.RepositoryIdentity) { i.URL = strings.Replace(i.URL, "forge.test", "attacker.test", 1) }, false},
				{"missing self hosted URL", func(i *apiv1.RepositoryIdentity) { i.URL = "" }, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					candidate := identity
					tc.edit(&candidate)
					revision := apiv1.WorkspaceRevision{Repository: candidate, CommitSHA: strings.Repeat("a", 40)}
					got, err := Resolve(context.Background(), revision, configured, nil, lookupFixture)
					if tc.valid && (err != nil || !reflect.DeepEqual(got, configured)) {
						t.Fatalf("authorized policy = %+v, error = %v", got, err)
					}
					if !tc.valid && err == nil {
						t.Fatalf("mixed identity authorized: %+v", candidate)
					}
					if tc.valid {
						got.Checkout.Sparse[0] = "changed"
						if configured.Checkout.Sparse[0] != "src" {
							t.Fatal("authorization exposed mutable configuration")
						}
					}
				})
			}
		})
	}
}

func TestResolveClassifiesProviderLookupFailures(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			configured := apiv1.RepoRef{Provider: apiv1.ProviderGitea, BaseURL: server.URL, Owner: "org", Name: "repo"}
			revision := apiv1.WorkspaceRevision{Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitea, URL: server.URL, Owner: "org", Name: "repo"}, CommitSHA: strings.Repeat("a", 40)}
			p := providers.NewGiteaProvider(server.URL, "token", providers.WithGiteaMaxTransientRetries(0), providers.WithGiteaMaxRateLimitRetries(0))
			_, err := Resolve(context.Background(), revision, configured, nil, func(ctx context.Context, route apiv1.RepoRef) (providers.RepositoryMetadata, error) {
				return p.ReadRepository(ctx, providers.RepositoryRef{Owner: route.Owner, Name: route.Name})
			})
			want := CodeUnauthorized
			if status == 429 || status == 503 {
				want = CodeAcquisition
			}
			var coded *Error
			if !errors.As(err, &coded) || coded.Code != want || coded.NonRetryable() != (want != CodeAcquisition) {
				t.Fatalf("status=%d error=%v", status, err)
			}
		})
	}
}

func TestResolveRejectsForgedTrustedMetadataAndMissingLookup(t *testing.T) {
	configured := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo"}
	revision := validRevision()
	revision.BaseRepository = nil
	if _, err := Resolve(context.Background(), *revision, configured, nil, nil); errorCode(err) != CodeUnauthorized {
		t.Fatalf("missing verifier: %v", err)
	}
	if _, err := Resolve(context.Background(), *revision, configured, []apiv1.RepoRef{configured}, lookupFixture); errorCode(err) != CodeUnauthorized {
		t.Fatalf("duplicate configured authority: %v", err)
	}
	for _, edit := range []func(*providers.RepositoryMetadata){
		func(m *providers.RepositoryMetadata) { m.ServiceRoot = "https://other.test" },
		func(m *providers.RepositoryMetadata) { m.Repository.Name = "other" },
		func(m *providers.RepositoryMetadata) { m.Repository.URL = "https://github.com/org/other" },
		func(m *providers.RepositoryMetadata) { m.Repository.Owner = "other" },
		func(m *providers.RepositoryMetadata) { m.Repository.ID = "" },
	} {
		_, err := Resolve(context.Background(), *revision, configured, nil, func(ctx context.Context, route apiv1.RepoRef) (providers.RepositoryMetadata, error) {
			metadata, err := lookupFixture(ctx, route)
			edit(&metadata)
			return metadata, err
		})
		if errorCode(err) != CodeUnauthorized {
			t.Fatalf("untrusted metadata accepted: %v", err)
		}
	}
}

func TestResolveDoesNotStripRepositoryNameSuffix(t *testing.T) {
	configured := apiv1.RepoRef{Provider: apiv1.ProviderGitea, BaseURL: "https://forge.test", Owner: "org", Name: "repo.git"}
	revision := apiv1.WorkspaceRevision{Repository: apiv1.RepositoryIdentity{
		Provider: apiv1.ProviderGitea, Owner: "org", Name: "repo.git", URL: "https://forge.test/org/repo",
	}, CommitSHA: strings.Repeat("a", 40)}
	if _, err := Resolve(context.Background(), revision, configured, nil, lookupFixture); errorCode(err) != CodeUnauthorized {
		t.Fatalf("URL named a different repository: %v", err)
	}
	revision.Repository.URL += ".git.git"
	if _, err := Resolve(context.Background(), revision, configured, nil, lookupFixture); err != nil {
		t.Fatalf("matching clone URL refused: %v", err)
	}
}
