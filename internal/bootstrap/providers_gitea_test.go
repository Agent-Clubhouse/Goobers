package bootstrap

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
