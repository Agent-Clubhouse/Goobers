package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/providers"
)

// exampleStampedBacklog is a gaggle's backlog settings as the dispatcher stamps
// them into a stage pod: a separate backlog project, a claim scope, and a
// doneStates stricter than the default (Resolved no longer counts as done).
const exampleStampedBacklog = `{"provider":"ado","project":"example-backlog","labels":["team-a"],"doneStates":{"categories":["Completed","Removed"]}}`

var exampleRoutedADORepo = providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "example-code", Name: "example-repo"}

// captureStageGaggleWarnings redirects the stage-side gaggle warnings into a
// buffer and forgets which were already given.
func captureStageGaggleWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
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
	return &warnings
}

// TestStagePodAppliesTheStampedGaggleBacklog is #5957's headline: a stage pod
// has no instance config, and with the dispatcher's stamp it still addresses
// the gaggle's backlog project, keeps its claim scope and applies its stricter
// doneStates, exactly as a local stage applies them from the config.
func TestStagePodAppliesTheStampedGaggleBacklog(t *testing.T) {
	warnings := captureStageGaggleWarnings(t)
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(dispatcher.GaggleBacklogEnv, exampleStampedBacklog)
	root := t.TempDir() // no instance config, as in a stage pod

	want := exampleRoutedADORepo
	want.Project = "example-backlog"
	if got := backlogRepoRefForStage(root, exampleRoutedADORepo); got != want {
		t.Fatalf("backlogRepoRefForStage = %+v, want the stamped backlog project %+v", got, want)
	}
	if err := requireStageGaggleBacklog(root, exampleRoutedADORepo); err != nil {
		t.Fatalf("requireStageGaggleBacklog: %v", err)
	}
	if got := stageADOBacklogScope(root, &bytes.Buffer{}); !reflect.DeepEqual(got, []string{"team-a"}) {
		t.Fatalf("claim scope = %v, want the stamped labels", got)
	}
	gaggle, set, err := stageGaggleBacklog(root)
	if err != nil {
		t.Fatal(err)
	}
	states, ok := providerconfig.GaggleADODoneStates(set, gaggle)
	if !ok || !reflect.DeepEqual(states.Categories, []string{"Completed", "Removed"}) {
		t.Fatalf("doneStates = %+v (ok=%t), want the stamped categories", states, ok)
	}
	if err := applyGaggleDoneStates(root, providers.NewADOProvider("example-org", "example-code", "token")); err != nil {
		t.Fatalf("applyGaggleDoneStates: %v", err)
	}
	if warnings.Len() != 0 {
		t.Fatalf("warnings = %q, want none when the settings were delivered", warnings.String())
	}
}

// TestStageGaggleBacklogStampWinsOverAReadableConfig: like the bot-login
// stamp, the dispatcher's stamp is authoritative over any config the stage
// can read. In a pod the only readable config is inside the workspace, which
// the run itself may have authored.
func TestStageGaggleBacklogStampWinsOverAReadableConfig(t *testing.T) {
	root := initDemo(t)
	t.Setenv(executor.GaggleEnvVar, "example")
	want := exampleRoutedADORepo
	want.Project = "example-backlog"
	t.Setenv(dispatcher.GaggleBacklogEnv, exampleStampedBacklog)
	if got := backlogRepoRefForStage(root, exampleRoutedADORepo); got != want {
		t.Fatalf("backlogRepoRefForStage = %+v, want the stamp's %+v over the readable config", got, want)
	}
}

// TestStageGaggleBacklogFailsClosed is the decision #5957 asked for: when a
// stage of a named gaggle cannot resolve its backlog settings, its Azure
// DevOps provider refuses to build and doneStates is an error, instead of the
// silent fall back to the code project and the default doneStates that let a
// Resolved predecessor count as done.
func TestStageGaggleBacklogFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stamp   *string
		wantErr string
	}{
		{name: "no stamp and no instance config", wantErr: "stamped no " + dispatcher.GaggleBacklogEnv},
		{name: "empty stamp", stamp: stampValue(""), wantErr: "stamped an empty " + dispatcher.GaggleBacklogEnv},
		{name: "malformed stamp", stamp: stampValue("{not json"), wantErr: dispatcher.GaggleBacklogEnv + " is malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureStageGaggleWarnings(t)
			t.Setenv(executor.GaggleEnvVar, "example")
			if tc.stamp != nil {
				t.Setenv(dispatcher.GaggleBacklogEnv, *tc.stamp)
			}
			root := t.TempDir()

			err := requireStageGaggleBacklog(root, exampleRoutedADORepo)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), `gaggle "example"`) {
				t.Fatalf("requireStageGaggleBacklog = %v, want a refusal naming the gaggle and %q", err, tc.wantErr)
			}
			if err := applyGaggleDoneStates(root, providers.NewADOProvider("example-org", "example-code", "token")); err == nil {
				t.Fatal("applyGaggleDoneStates applied the default doneStates instead of refusing")
			}
			t.Setenv(executor.CredentialEnvVar("github:issues:write"), "issues-write-token")
			if _, err := newProviderForStage(root, exampleRoutedADORepo, false); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ADO stage provider error = %v, want the backlog refusal", err)
			}
			// The ref falls back only defensively, and says why.
			if got := backlogRepoRefForStage(root, exampleRoutedADORepo); got != exampleRoutedADORepo || !strings.Contains(warnings.String(), tc.wantErr) {
				t.Fatalf("backlogRepoRefForStage = %+v, warnings = %q", got, warnings.String())
			}
		})
	}
}

func stampValue(s string) *string { return &s }

// TestStageGaggleBacklogOnlyGatesNamedADOStages: an ungaggled stage, and a
// stage routed to another provider, have no gaggle backlog settings to apply
// and are not refused.
func TestStageGaggleBacklogOnlyGatesNamedADOStages(t *testing.T) {
	root := t.TempDir()
	t.Setenv(executor.GaggleEnvVar, "")
	if err := requireStageGaggleBacklog(root, exampleRoutedADORepo); err != nil {
		t.Fatalf("ungaggled ADO stage refused: %v", err)
	}
	if err := applyGaggleDoneStates(root, providers.NewADOProvider("example-org", "example-code", "token")); err != nil {
		t.Fatalf("ungaggled doneStates refused: %v", err)
	}
	t.Setenv(executor.GaggleEnvVar, "example")
	github := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := requireStageGaggleBacklog(root, github); err != nil {
		t.Fatalf("GitHub stage refused: %v", err)
	}
	if got := backlogRepoRefForStage(root, github); got != github {
		t.Fatalf("backlogRepoRefForStage(github) = %+v, want unchanged", got)
	}
}
