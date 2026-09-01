package providers

import "testing"

// TestRepositoryCanonicalKeyDistinguishesProviderIdentity covers #3649: durable
// records keyed by owner/name alone let repositories that merely share an
// owner/name shape — a different provider, a different Azure DevOps project, a
// different self-hosted service, a different repository ID — collide.
func TestRepositoryCanonicalKeyDistinguishesProviderIdentity(t *testing.T) {
	base := RepositoryRef{Provider: ProviderGitHub, Owner: "contoso", Name: "web"}
	distinct := map[string]RepositoryRef{
		"same shape on another provider": {Provider: ProviderADO, Owner: "contoso", Name: "web"},
		"another ADO project": {
			Provider: ProviderADO, Owner: "contoso", Project: "tooling", Name: "web",
		},
		"another service URL": {
			Provider: ProviderGitea, Owner: "contoso", Name: "web", URL: "https://gitea.example.com",
		},
		"another self-hosted service": {
			Provider: ProviderGitea, Owner: "contoso", Name: "web", URL: "https://git.internal.test",
		},
		"another repository ID": {Provider: ProviderGitHub, Owner: "contoso", Name: "web", ID: "42"},
		"another owner":         {Provider: ProviderGitHub, Owner: "fabrikam", Name: "web"},
		"another name":          {Provider: ProviderGitHub, Owner: "contoso", Name: "api"},
	}
	seen := map[string]string{base.CanonicalKey(): "base"}
	for name, repo := range distinct {
		key := repo.CanonicalKey()
		if owner, clash := seen[key]; clash {
			t.Fatalf("%s shares canonical key %q with %s", name, key, owner)
		}
		seen[key] = name
		if SameRepository(base, repo) {
			t.Fatalf("SameRepository(base, %s) = true, want distinct identities", name)
		}
	}
}

func TestRepositoryCanonicalKeyNormalizesEquivalentSpellings(t *testing.T) {
	left := RepositoryRef{Provider: ProviderGitea, Owner: "Contoso", Name: "Web", URL: "https://Gitea.Example.com"}
	right := RepositoryRef{Provider: ProviderGitea, Owner: "contoso", Name: "web", URL: "https://gitea.example.com/contoso/web"}
	if !SameRepository(left, right) {
		t.Fatalf("canonical keys %q and %q differ, want the same repository", left.CanonicalKey(), right.CanonicalKey())
	}
	if !SameRepository(left, left) {
		t.Fatal("a reference is not equal to itself")
	}
}
