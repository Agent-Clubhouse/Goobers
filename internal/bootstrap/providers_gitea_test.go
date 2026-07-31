package bootstrap

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// TestBacklogProviderForGitea proves the gitea backlog-dispatch branch: a
// *providers.GiteaProvider is returned, the RepositoryRef carries
// Provider=gitea/Owner/Name derived from the "owner/name" project string, and
// BaseURL is required (self-hosted Gitea has no fixed host).
func TestBacklogProviderForGitea(t *testing.T) {
	p, repo, err := BacklogProviderFor(apiv1.BacklogRef{
		Provider: apiv1.ProviderGitea,
		Project:  "acme/web",
		BaseURL:  "https://gitea.example.com",
	}, "tok", nil, nil, nil)
	if err != nil {
		t.Fatalf("BacklogProviderFor: %v", err)
	}
	giteaProvider, ok := p.(*providers.GiteaProvider)
	if !ok {
		t.Fatalf("provider = %T, want *providers.GiteaProvider", p)
	}
	if giteaProvider.Kind() != providers.ProviderGitea {
		t.Fatalf("Kind() = %q, want gitea", giteaProvider.Kind())
	}
	if repo.Provider != providers.ProviderGitea || repo.Owner != "acme" || repo.Name != "web" {
		t.Fatalf("repo = %+v, want gitea acme/web", repo)
	}
}

func TestBacklogProviderForGiteaRequiresBaseURL(t *testing.T) {
	if _, _, err := BacklogProviderFor(apiv1.BacklogRef{
		Provider: apiv1.ProviderGitea,
		Project:  "acme/web",
	}, "tok", nil, nil, nil); err == nil {
		t.Fatal("expected an error for a gitea backlog with no baseUrl (self-hosted Gitea has no fixed host)")
	}
}

func TestBacklogProviderForGiteaRequiresOwnerSlashName(t *testing.T) {
	if _, _, err := BacklogProviderFor(apiv1.BacklogRef{
		Provider: apiv1.ProviderGitea,
		Project:  "not-owner-slash-name",
		BaseURL:  "https://gitea.example.com",
	}, "tok", nil, nil, nil); err == nil {
		t.Fatal("expected an error for a malformed gitea project")
	}
}

// TestRepoProviderForGitea proves RepoProviderFor's gitea branch mirrors
// BacklogProviderFor's: a *providers.GiteaProvider, a RepositoryRef with
// Provider/Owner/Name, and a required BaseURL.
func TestRepoProviderForGitea(t *testing.T) {
	p, ref, err := RepoProviderFor(apiv1.RepoRef{
		Provider: apiv1.ProviderGitea,
		Owner:    "acme",
		Name:     "app",
		BaseURL:  "https://gitea.example.com",
	}, "tok", nil, nil, nil)
	if err != nil {
		t.Fatalf("RepoProviderFor: %v", err)
	}
	if _, ok := p.(*providers.GiteaProvider); !ok {
		t.Fatalf("provider = %T, want *providers.GiteaProvider", p)
	}
	if ref.Provider != providers.ProviderGitea || ref.Owner != "acme" || ref.Name != "app" {
		t.Fatalf("ref = %+v, want gitea acme/app", ref)
	}
}

func TestRepoProviderForGiteaRequiresBaseURL(t *testing.T) {
	if _, _, err := RepoProviderFor(apiv1.RepoRef{
		Provider: apiv1.ProviderGitea,
		Owner:    "acme",
		Name:     "app",
	}, "tok", nil, nil, nil); err == nil {
		t.Fatal("expected an error for a gitea repo with no baseUrl (self-hosted Gitea has no fixed host)")
	}
}

// TestRepoProviderForDispatchesEveryKindAndRejectsUnknown proves
// RepoProviderFor's full kind dispatch: github, ado, and gitea each route to
// their concrete provider type, and an unrecognized kind is rejected.
func TestRepoProviderForDispatchesEveryKindAndRejectsUnknown(t *testing.T) {
	if p, ref, err := RepoProviderFor(apiv1.RepoRef{
		Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "app",
	}, "tok", nil, nil, nil); err != nil {
		t.Fatalf("github: %v", err)
	} else if _, ok := p.(*providers.GitHubProvider); !ok || ref.Provider != providers.ProviderGitHub {
		t.Fatalf("github provider/ref = %T, %+v", p, ref)
	}

	if p, ref, err := RepoProviderFor(apiv1.RepoRef{
		Provider: apiv1.ProviderADO, Owner: "org", Project: "project", Name: "repo",
	}, "tok", nil, journal.NewRegistryScrubber(), nil); err != nil {
		t.Fatalf("ado: %v", err)
	} else if _, ok := p.(*providers.ADOProvider); !ok || ref.Provider != providers.ProviderADO {
		t.Fatalf("ado provider/ref = %T, %+v", p, ref)
	}

	if _, _, err := RepoProviderFor(apiv1.RepoRef{
		Provider: "gitlab", Owner: "acme", Name: "app",
	}, "tok", nil, nil, nil); err == nil {
		t.Fatal("expected an error for an unsupported repo provider kind")
	}
}
