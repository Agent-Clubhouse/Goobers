package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
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

// TestStageWarnsWhenGaggleConfigIsUnreadable pins the brokered-pod fallback:
// a stage that names a gaggle but has no instance config (a stage pod) warns
// once per setting on stderr instead of silently using the default
// doneStates and the code project for backlog work.
func TestStageWarnsWhenGaggleConfigIsUnreadable(t *testing.T) {
	var warnings bytes.Buffer
	previous := stageGaggleConfigWarnings
	stageGaggleConfigWarnings = &warnings
	clearWarned := func() {
		stageGaggleConfigWarned.Range(func(key, _ any) bool {
			stageGaggleConfigWarned.Delete(key)
			return true
		})
	}
	clearWarned()
	t.Cleanup(func() {
		stageGaggleConfigWarnings = previous
		clearWarned()
	})
	t.Setenv(executor.GaggleEnvVar, "example")
	root := t.TempDir() // no instance config, as in a stage pod

	routed := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "code", Name: "repo"}
	for range 2 {
		if got := backlogRepoRefForStage(root, routed); got != routed {
			t.Fatalf("backlogRepoRefForStage = %+v, want the routed repo unchanged", got)
		}
		applyGaggleDoneStates(root, providers.NewADOProvider("example-org", "code", "token"))
	}
	out := warnings.String()
	for _, setting := range []string{"the backlog project override", "backlog.doneStates"} {
		if n := strings.Count(out, setting+" is not applied"); n != 1 {
			t.Errorf("warnings for %q = %d, want exactly 1:\n%s", setting, n, out)
		}
	}
	if !strings.Contains(out, `gaggle "example"`) {
		t.Errorf("warning does not name the gaggle:\n%s", out)
	}
}
