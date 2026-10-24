package terminalwriter

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func gaggleSet(gaggles ...apiv1.Gaggle) *instance.ConfigSet {
	return &instance.ConfigSet{Gaggles: gaggles}
}

func gaggle(name string, project apiv1.RepoRef) apiv1.Gaggle {
	g := apiv1.Gaggle{}
	g.Name = name
	g.Spec.Project = project
	return g
}

func TestInstanceBindingFindsTheGaggleWhoseBacklogIsTheRepository(t *testing.T) {
	web := apiv1.RepoRef{Provider: "github", Owner: "acme", Name: "web"}
	api := apiv1.RepoRef{Provider: "github", Owner: "acme", Name: "api"}
	set := gaggleSet(gaggle("web", web), gaggle("api", api))
	got, ok := InstanceBinding(set, "digest-1", providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"})
	if !ok || got.Gaggle != "api" || got.Generation != "digest-1" || got.RunID != "" {
		t.Fatalf("binding = %+v, %v; want gaggle api at digest-1, bound to no run", got, ok)
	}
	if got.WorkItems.Name != "api" || got.Workspace.Name != "api" {
		t.Fatalf("binding repositories = %+v", got)
	}
}

func TestInstanceBindingRefusesARepositoryNoGaggleOwns(t *testing.T) {
	set := gaggleSet(gaggle("web", apiv1.RepoRef{Provider: "github", Owner: "acme", Name: "web"}))
	if _, ok := InstanceBinding(set, "d", providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "elsewhere"}); ok {
		t.Fatal("a repository no configured gaggle uses was bound; credentials would be borrowed")
	}
	if _, ok := InstanceBinding(nil, "d", providers.RepositoryRef{}); ok {
		t.Fatal("a nil configuration bound")
	}
	if _, ok := InstanceBinding(gaggleSet(gaggle("empty", apiv1.RepoRef{})), "d", providers.RepositoryRef{}); ok {
		t.Fatal("a gaggle with no project bound")
	}
}
