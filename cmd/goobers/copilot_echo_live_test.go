//go:build integration

package main

import (
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

const copilotEchoSentinel = "GOOBERS_GHCP_AUTH_SPIKE_V1"

func TestIntegrationCopilotEchoWorkflow(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_COPILOT_LIVE_SMOKE")
	testdep.RequireEnv(t, "COPILOT_GITHUB_TOKEN")
	testdep.Require(t, "copilot", "git")

	profile := t.TempDir()
	t.Setenv("HOME", profile)
	t.Setenv("COPILOT_HOME", filepath.Join(profile, ".copilot"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(profile, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(profile, ".local", "share"))
	t.Setenv("GOOBERS_GITHUB_TOKEN", "ghp_copilot_echo_fixture_dummy_token")
	root := initDemo(t)
	preserveCopilotEchoEvidence(t, root)
	installCopilotEchoFixture(t, root)

	fixtureRepo := newDaemonFixtureRepo(t)
	previousRepoCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }
	previousPreflight := preflightHarnesses
	preflightHarnesses = preflightAgenticHarnesses
	t.Cleanup(func() {
		repoCloneURL = previousRepoCloneURL
		preflightHarnesses = previousPreflight
	})

	code, stdout, stderr := runArgs(t, "run", "copilot-echo", root)
	if code != 0 {
		t.Fatalf("goobers run copilot-echo: code = %d, stderr = %q", code, stderr)
	}

	runID := runIDFromRunStdout(t, stdout)
	reader, err := journal.OpenRead(filepath.Join(root, "runs", runID))
	if err != nil {
		t.Fatalf("OpenRead(%s): %v", runID, err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	stageMatches := 0
	runMatches := 0
	for _, event := range events {
		switch {
		case event.Type == journal.EventStageFinished &&
			event.Stage == "echo" &&
			event.Status == string(apiv1.ResultSuccess) &&
			event.Outputs["sentinel"] == copilotEchoSentinel:
			stageMatches++
		case event.Type == journal.EventRunFinished &&
			event.Status == string(journal.PhaseCompleted):
			runMatches++
		}
	}
	if stageMatches != 1 {
		t.Fatalf("matching echo stage.finished events = %d, want 1", stageMatches)
	}
	if runMatches != 1 {
		t.Fatalf("matching completed run.finished events = %d, want 1", runMatches)
	}
}

func preserveCopilotEchoEvidence(t *testing.T, root string) {
	t.Helper()

	evidenceDir := os.Getenv("GOOBERS_COPILOT_EVIDENCE_DIR")
	if evidenceDir == "" {
		return
	}
	t.Cleanup(func() {
		runsDir := filepath.Join(root, "runs")
		if _, err := os.Stat(runsDir); err != nil {
			if os.IsNotExist(err) {
				return
			}
			t.Errorf("stat Copilot echo runs: %v", err)
			return
		}
		if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
			t.Errorf("create Copilot echo evidence directory: %v", err)
			return
		}
		if err := os.CopyFS(filepath.Join(evidenceDir, "runs"), os.DirFS(runsDir)); err != nil {
			t.Errorf("preserve Copilot echo runs: %v", err)
		}
	})
}

func installCopilotEchoFixture(t *testing.T, root string) {
	t.Helper()

	instancePath := filepath.Join(root, "instance.yaml")
	instanceYAML, err := os.ReadFile(instancePath)
	if err != nil {
		t.Fatal(err)
	}
	instanceYAML = append(instanceYAML, []byte(`
credentials:
  - capability: agent:model
    token:
      env: COPILOT_GITHUB_TOKEN
`)...)
	if err := os.WriteFile(instancePath, instanceYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	gaggleDir := filepath.Join(root, "config", "gaggles", "example")
	for _, name := range []string{"goobers", "workflows"} {
		if err := os.RemoveAll(filepath.Join(gaggleDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.CopyFS(gaggleDir, os.DirFS("testdata/copilot-echo")); err != nil {
		t.Fatal(err)
	}
}
