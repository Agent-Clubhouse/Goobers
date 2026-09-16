package workspacerevision

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestResolveConfiguredAuthority(t *testing.T) {
	base := apiv1.RepoRef{
		Provider: apiv1.ProviderGitHub, Owner: "org", Name: "base",
		Branch: "main", ConnectionRef: "trusted", Checkout: &apiv1.CheckoutSpec{Sparse: []string{"src"}},
	}
	fork := base
	fork.Owner = "fork-owner"
	fork.ConnectionRef = "fork-trusted"
	r := testRevision()
	r.Repository.Owner = fork.Owner
	r.Repository.ID = "untrusted-native-id"
	r.Repository.URL = "https://github.com/fork-owner/base.git"
	got, err := Resolve(*r, base, []apiv1.RepoRef{fork})
	if err != nil || !reflect.DeepEqual(got, fork) {
		t.Fatalf("got=%+v err=%v, want original configuration", got, err)
	}
	got.Checkout.Sparse[0] = "mutated"
	if fork.Checkout.Sparse[0] != "src" {
		t.Fatal("resolved reference aliases configured checkout policy")
	}
	_, err = Resolve(*r, base, nil)
	requireCode(t, err, CodeUnauthorized)
	conflicting := fork
	conflicting.ConnectionRef = "other"
	_, err = Resolve(*r, base, []apiv1.RepoRef{fork, conflicting})
	requireCode(t, err, CodeUnauthorized)
	r.BaseRepository.Name = "other"
	_, err = Resolve(*r, base, []apiv1.RepoRef{fork})
	requireCode(t, err, CodeUnauthorized)
	r.CommitSHA = "main"
	_, err = Resolve(*r, base, []apiv1.RepoRef{fork})
	requireCode(t, err, CodeInvalid)
}

func TestResolveIdentityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured apiv1.RepoRef
		identity   apiv1.RepositoryIdentity
		valid      bool
	}{
		{"github default", apiv1.RepoRef{Provider: "github", Owner: "org", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "github", Owner: "org", Name: "repo"}, true},
		{"github spoofed host", apiv1.RepoRef{Provider: "github", Owner: "org", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "github", Owner: "org", Name: "repo", URL: "https://evil.test/org/repo"}, false},
		{"github spoofed URL path", apiv1.RepoRef{Provider: "github", Owner: "org", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "github", Owner: "org", Name: "repo", URL: "https://github.com/other/repo"}, false},
		{"github downgrade", apiv1.RepoRef{Provider: "github", Owner: "org", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "github", Owner: "org", Name: "repo", URL: "http://github.com/org/repo"}, false},
		{"gitea service prefix", apiv1.RepoRef{Provider: "gitea", BaseURL: "https://git.test/forge", Owner: "org", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "gitea", Owner: "org", Name: "repo", URL: "https://git.test/forge/org/repo"}, true},
		{"gitea wrong prefix", apiv1.RepoRef{Provider: "gitea", BaseURL: "https://git.test/forge", Owner: "org", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "gitea", Owner: "org", Name: "repo", URL: "https://git.test/other/org/repo"}, false},
		{"gitea wrong port", apiv1.RepoRef{Provider: "gitea", BaseURL: "https://git.test", Owner: "org", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "gitea", Owner: "org", Name: "repo", URL: "https://git.test:444/org/repo"}, false},
		{"ADO project match", apiv1.RepoRef{Provider: "ado", Owner: "org", Project: "one", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "ado", Owner: "org", Project: "one", Name: "repo", ID: "native", URL: "https://dev.azure.com/org/one/_git/repo"}, true},
		{"ADO project substitution", apiv1.RepoRef{Provider: "ado", Owner: "org", Project: "one", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "ado", Owner: "org", Project: "two", Name: "repo"}, false},
		{"ADO URL project substitution", apiv1.RepoRef{Provider: "ado", Owner: "org", Project: "one", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "ado", Owner: "org", Project: "one", Name: "repo", URL: "https://dev.azure.com/org/two/_git/repo"}, false},
		{"ADO REST ID evidence", apiv1.RepoRef{Provider: "ado", Owner: "org", Project: "one", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "ado", Owner: "org", Project: "one", Name: "repo", ID: "native", URL: "https://dev.azure.com/org/one/_apis/git/repositories/native"}, true},
		{"ADO REST ID disagreement", apiv1.RepoRef{Provider: "ado", Owner: "org", Project: "one", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "ado", Owner: "org", Project: "one", Name: "repo", ID: "native", URL: "https://dev.azure.com/org/one/_apis/git/repositories/other"}, false},
		{"ADO configured ID", apiv1.RepoRef{Provider: "ado", Owner: "org", Project: "one", Name: "native"}, apiv1.RepositoryIdentity{Provider: "ado", Owner: "org", Project: "one", Name: "repo", ID: "native", URL: "https://dev.azure.com/org/one/_git/repo"}, true},
		{"ADO unconfigured ID cannot replace name", apiv1.RepoRef{Provider: "ado", Owner: "org", Project: "one", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "ado", Owner: "org", Project: "one", Name: "other", ID: "native"}, false},
		{"custom service needs URL", apiv1.RepoRef{Provider: "ado", BaseURL: "https://ado.test", Owner: "org", Project: "one", Name: "repo"}, apiv1.RepositoryIdentity{Provider: "ado", Owner: "org", Project: "one", Name: "repo"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revision := apiv1.WorkspaceRevision{Repository: tc.identity, CommitSHA: strings.Repeat("a", 40)}
			got, err := Resolve(revision, tc.configured, nil)
			if tc.valid {
				if err != nil || !reflect.DeepEqual(got, tc.configured) {
					t.Fatalf("got=%+v err=%v", got, err)
				}
			} else {
				requireCode(t, err, CodeUnauthorized)
			}
		})
	}
}
