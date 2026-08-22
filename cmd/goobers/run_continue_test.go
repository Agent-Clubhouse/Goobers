package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestRunRunContinuePersistsInputSource(t *testing.T) {
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
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %s", exitCode, stderr.String())
	}
	continuationID := strings.TrimSpace(stdout.String())
	reader, err := journal.OpenRead(filepath.Join(runsDir, continuationID))
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if len(id.Inputs) != 1 || id.Inputs[0].Name != "issue" ||
		id.Inputs[0].Source != inputPath {
		t.Fatalf("continuation input = %+v, want source %q", id.Inputs, inputPath)
	}
}
