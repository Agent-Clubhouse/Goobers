package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func TestBacklogScheduledResweepProviderFailureIsJournaled(t *testing.T) {
	var definition apiv1.Workflow
	data, err := os.ReadFile("../../reference-workflows/gaggles/goobers/workflows/curate-resweep.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &definition); err != nil {
		t.Fatal(err)
	}
	var curator apiv1.Goober
	data, err = os.ReadFile("../../reference-workflows/gaggles/goobers/goobers/curator/goober.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &curator); err != nil {
		t.Fatal(err)
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: definition.Name, Version: 1, DSLVersion: definition.DSLVersion, Spec: definition.Spec,
	}, workflow.WithGoobers(map[string]apiv1.GooberSpec{curator.Name: curator.Spec}))
	if err != nil {
		t.Fatal(err)
	}
	root := initDemo(t)
	origin := newDocsDryRunOrigin(t)
	server := newFakeGitHubServer(t, "fixture", "repository")
	server.addIssue(7, "Blocked item", providers.LabelApproved, blockedOnSiblingLabel)
	server.dependencyFailureStatus = map[int]int{7: http.StatusForbidden}
	t.Setenv("GOOBERS_TEST_GITHUB_API_URL", server.server.URL)
	const tokenEnv = "GOOBERS_RESWEEP_JOURNAL_TOKEN"
	t.Setenv(tokenEnv, "resweep-test-token")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "resweep", Env: tokenEnv}})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	runsDir := filepath.Join(root, "runs")
	r, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, registrar runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(resolver, []credentials.Grant{
				{Capability: "github:issues:write", Ref: "resweep"},
				{Capability: "github:pr:write", Ref: "resweep"},
			}, registrar)
			if err != nil {
				return nil, err
			}
			shell, err := executor.NewShellExecutor(injector, rec)
			if err != nil {
				return nil, err
			}
			shell.InstanceRoot = root
			shell.SelfBin = executable
			shell.ExtraEnvAllowlist = []string{"GOOBERS_TEST_GITHUB_API_URL"}
			return shell, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: manager, RunsDir: runsDir, ScratchDir: filepath.Join(root, "scratch"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return origin, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	const runID = "resweep-provider-failure"
	result, err := r.Start(context.Background(), runner.StartInput{
		RunID: runID, Machine: machine, Gaggle: definition.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fixture", Name: "repository", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase == journal.PhaseCompleted {
		t.Fatalf("failed sweep completed: %+v", result)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var failure *journal.ErrorDetail
	for _, event := range events {
		if event.Type == journal.EventStageStarted && event.Stage != "query-resweep" {
			t.Fatalf("provider failure reached downstream stage %s", event.Stage)
		}
		if event.Type == journal.EventStageFinished && event.Stage == "query-resweep" && event.Status == string(apiv1.ResultFailure) {
			failure = event.Error
		}
	}
	if failure == nil || failure.Code == "" || failure.Code == "nonzero_exit" || !strings.Contains(failure.Message, "dependency recheck item 7") {
		t.Fatalf("provider failure missing from query-resweep journal: %+v; events=%+v", failure, events)
	}
}
