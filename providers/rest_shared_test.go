package providers

import "testing"

// The shared REST helpers (#4234) are parameterized by the calling backend, so
// the projection each backend gets must still carry its own provider identity.
func TestRepositoryRefCarriesCallingProviderKind(t *testing.T) {
	repo := &restRepository{Name: "goobers", HTMLURL: "https://example.test/acme/goobers"}
	repo.Owner.Login = "acme"

	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		ref := repositoryRef(kind, repo)
		if ref == nil {
			t.Fatalf("repositoryRef(%s) = nil, want a ref", kind)
		}
		if ref.Provider != kind {
			t.Errorf("repositoryRef(%s).Provider = %s, want %s", kind, ref.Provider, kind)
		}
		if ref.Owner != "acme" || ref.Name != "goobers" || ref.URL != repo.HTMLURL {
			t.Errorf("repositoryRef(%s) = %+v, want acme/goobers at %s", kind, *ref, repo.HTMLURL)
		}
	}

	if ref := repositoryRef(ProviderGitea, nil); ref != nil {
		t.Errorf("repositoryRef(gitea, nil) = %+v, want nil", *ref)
	}
}

func TestNormalizeCombinedStatusState(t *testing.T) {
	for state, want := range map[string]CheckState{
		"success": CheckStatePassing,
		"SUCCESS": CheckStatePassing,
		"failure": CheckStateFailing,
		"error":   CheckStateFailing,
		"pending": CheckStatePending,
		"":        CheckStatePending,
	} {
		if got := normalizeCombinedStatusState(state); got != want {
			t.Errorf("normalizeCombinedStatusState(%q) = %s, want %s", state, got, want)
		}
	}
}
