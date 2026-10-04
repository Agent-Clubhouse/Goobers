package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

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

// TestStageBacklogProjectFromEnvWithoutConfig covers a stage pod that has no
// instance config but whose workflow declares GOOBERS_BACKLOG_PROJECT: backlog
// work targets that project instead of the routed code project.
func TestStageBacklogProjectFromEnvWithoutConfig(t *testing.T) {
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(stageBacklogProjectEnvVar, " backlog ")
	root := t.TempDir()

	routed := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "code", Name: "repo"}
	want := routed
	want.Project = "backlog"
	if got := backlogRepoRefForStage(root, routed); got != want {
		t.Fatalf("backlogRepoRefForStage = %+v, want %+v", got, want)
	}

	github := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if got := backlogRepoRefForStage(root, github); got != github {
		t.Fatalf("backlogRepoRefForStage(github) = %+v, want unchanged", got)
	}
}
