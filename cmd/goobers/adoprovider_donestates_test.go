package main

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

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

	got, ok := gaggleADODoneStates(set, "ado-configured")
	want := providers.ADODoneStates{
		Categories: []string{"Completed", "Removed"},
		ByType:     map[string][]string{"Bug": {"Closed"}},
	}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("gaggleADODoneStates(ado-configured) = %+v, %v; want %+v, true", got, ok, want)
	}
	for _, name := range []string{"ado-default", "github", "missing"} {
		if got, ok := gaggleADODoneStates(set, name); ok {
			t.Errorf("gaggleADODoneStates(%s) = %+v, true; want the provider default", name, got)
		}
	}
}
