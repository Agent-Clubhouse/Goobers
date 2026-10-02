package providerconfig

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// Topology (b) (docs/design/ado-parity-dsl-2-0.md §7.2, ADO-N31): a GitHub
// backlog for Azure DevOps code. Neutral placeholders throughout.
const (
	topologyBOrg         = "example-org"
	topologyBCodeProject = "example-project"
	topologyBCodeRepo    = "service"
	topologyBBacklog     = "example-org/example-backlog"
	topologyBIssueURL    = "https://github.com/example-org/example-backlog/issues/"
)

func topologyBRouted() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderADO, Owner: topologyBOrg, Project: topologyBCodeProject, Name: topologyBCodeRepo}
}

func topologyBBacklogRef() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "example-org", Name: "example-backlog"}
}

func topologyBGaggleSet(backlog apiv1.BacklogRef) *instance.ConfigSet {
	return namedGaggleSet(apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: topologyBOrg, Project: topologyBCodeProject, Name: topologyBCodeRepo}, backlog)
}

func namedGaggleSet(project apiv1.RepoRef, backlog apiv1.BacklogRef) *instance.ConfigSet {
	g := apiv1.Gaggle{Spec: apiv1.GaggleSpec{Project: project, Backlog: backlog}}
	g.Name = "example"
	return &instance.ConfigSet{Gaggles: []apiv1.Gaggle{g}}
}

// ApplyBacklogProject routes by role: a GitHub or Gitea backlog for ADO code
// resolves to the backlog provider's own ref, and the ADO project split is
// unchanged.
func TestApplyBacklogProjectRoutesTopologyB(t *testing.T) {
	routed := topologyBRouted()
	for _, tc := range []struct {
		name    string
		backlog apiv1.BacklogRef
		want    providers.RepositoryRef
	}{
		{
			name:    "github backlog",
			backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: topologyBBacklog},
			want:    topologyBBacklogRef(),
		},
		{
			name:    "gitea backlog",
			backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitea, Project: topologyBBacklog, BaseURL: "https://gitea.example.com"},
			want:    providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "example-org", Name: "example-backlog", URL: "https://gitea.example.com"},
		},
		{
			name:    "ado project split is unchanged",
			backlog: apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "example-backlog-project"},
			want:    providers.RepositoryRef{Provider: providers.ProviderADO, Owner: topologyBOrg, Project: "example-backlog-project", Name: topologyBCodeRepo},
		},
		{
			name:    "malformed github backlog keeps routed",
			backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "example-backlog"},
			want:    routed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyBacklogProject(topologyBGaggleSet(tc.backlog), "example", routed)
			if got != tc.want {
				t.Fatalf("ApplyBacklogProject = %+v, want %+v", got, tc.want)
			}
		})
	}

	// A non-ADO mismatch (GitHub code, Gitea backlog) keeps its routed
	// provider: GitHub and Gitea behaviour does not change.
	github := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "example-org", Name: "service"}
	set := namedGaggleSet(
		apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example-org", Name: "service"},
		apiv1.BacklogRef{Provider: apiv1.ProviderGitea, Project: topologyBBacklog, BaseURL: "https://gitea.example.com"},
	)
	if got := ApplyBacklogProject(set, "example", github); got != github {
		t.Fatalf("GitHub project with a Gitea backlog routed to %+v, want the unchanged routed repo", got)
	}
}

// TestGaggleADODoneStatesCarriesBacklogSetting pins the ADO-N32 plumbing:
// an ADO gaggle's backlog.doneStates reaches the provider form unchanged,
// and a gaggle without it, a non-ADO backlog, or an unknown gaggle leaves the
// provider on its default.
func TestGaggleADODoneStatesCarriesBacklogSetting(t *testing.T) {
	gaggle := func(name string, provider apiv1.Provider, done *apiv1.BacklogDoneStates) apiv1.Gaggle {
		g := apiv1.Gaggle{Spec: apiv1.GaggleSpec{Backlog: apiv1.BacklogRef{
			Provider: provider, Project: "example-project", DoneStates: done,
		}}}
		g.Name = name
		return g
	}
	done := &apiv1.BacklogDoneStates{
		Categories: []apiv1.BacklogStateCategory{"Completed", "Removed"},
		ByType:     map[string][]string{"Bug": {"Closed"}},
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
		gaggle("ado-configured", apiv1.ProviderADO, done),
		gaggle("ado-default", apiv1.ProviderADO, nil),
		gaggle("github", apiv1.ProviderGitHub, done),
	}}

	got, ok := GaggleADODoneStates(set, "ado-configured")
	want := providers.ADODoneStates{
		Categories: []string{"Completed", "Removed"},
		ByType:     map[string][]string{"Bug": {"Closed"}},
	}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("GaggleADODoneStates(ado-configured) = %+v, %v; want %+v, true", got, ok, want)
	}
	for _, name := range []string{"ado-default", "github", "missing"} {
		if got, ok := GaggleADODoneStates(set, name); ok {
			t.Errorf("GaggleADODoneStates(%s) = %+v, true; want the provider default", name, got)
		}
	}
}
