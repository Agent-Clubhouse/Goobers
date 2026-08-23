package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestRunRunContinueRejectsLegacySourceWithoutWorkflowPin(t *testing.T) {
	root := t.TempDir()
	runsDir := instance.NewLayout(root).RunsDir()
	sourceID := "0af7651916cd43dd8448eb211c80319c"
	source, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: sourceID, Workflow: "wf", Gaggle: "g",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Append(journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(root, "input.txt")
	if err := os.WriteFile(inputPath, []byte("operator input"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := runRunContinue([]string{
		"--from", sourceID, "--terminal-seq", "2", "--target", "implement",
		"--operator", "operator@example.test", "--integrity", "maintainer",
		"--input", "issue=" + inputPath, root,
	}, &stdout, &stderr)
	if exitCode == 0 {
		t.Fatalf("legacy source was admitted; stdout = %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "resolve continuation source workflow") {
		t.Fatalf("stderr = %s, want source workflow resolution error", stderr.String())
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("runs directory entries = %d, want only source run", len(entries))
	}
}

func TestRunRunContinueResolvesEachHistoricalDigestBeforeCreatingWork(t *testing.T) {
	root := initDeterministicDemo(t)
	runsDir := instance.NewLayout(root).RunsDir()
	candidate, err := currentWorkflowMachine(root, journal.RunIdentity{
		Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "example",
	})
	if err != nil {
		t.Fatalf("currentWorkflowMachine: %v", err)
	}
	for index, command := range []string{"historical-one", "historical-two"} {
		machine, err := workflow.Compile(workflow.Definition{
			Name: "default-implement", Version: 1,
			Spec: apiv1.WorkflowSpec{
				Gaggle: "example", Start: "local-ci",
				Tasks: []apiv1.Task{{
					Name: "local-ci", Type: apiv1.TaskDeterministic,
					Goal: "historical command", Run: &apiv1.DeterministicRun{Command: []string{command}},
				}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		sourceID := []string{"historical-source-a", "historical-source-b"}[index]
		definition, err := json.Marshal(machine.Def)
		if err != nil {
			t.Fatal(err)
		}
		source, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: sourceID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
			WorkflowDigest: machine.Digest(), Gaggle: "example",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
			journal.WithInputIntegrity(map[string]apiv1.Integrity{
				journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
			}))
		if err != nil {
			t.Fatal(err)
		}
		if err := source.Append(journal.Event{
			Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
		}); err != nil {
			t.Fatal(err)
		}
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		exitCode := runRunContinue([]string{
			"--from", sourceID, "--terminal-seq", "2", "--target", "local-ci",
			"--operator", "operator@example.test", root,
		}, &stdout, &stderr)
		if exitCode == 0 {
			t.Fatalf("historical source %q was admitted", sourceID)
		}
		if !strings.Contains(stderr.String(), machine.Digest()) {
			t.Fatalf("stderr = %s, want historical digest %s", stderr.String(), machine.Digest())
		}
		if !strings.Contains(stderr.String(), candidate.Digest()) {
			t.Fatalf("stderr = %s, want candidate digest %s", stderr.String(), candidate.Digest())
		}
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("runs directory entries = %d, want only two historical sources", len(entries))
	}
}
