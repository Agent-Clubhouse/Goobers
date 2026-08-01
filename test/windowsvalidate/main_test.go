//go:build windows

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestFirstLine(t *testing.T) {
	cases := []struct {
		name, value, want string
	}{
		{"single line, no trailing newline", "go version go1.26.0 windows/amd64", "go version go1.26.0 windows/amd64"},
		{"multi-line takes only the first", "git version 2.44.0.windows.1\n", "git version 2.44.0.windows.1"},
		{"leading/trailing whitespace trimmed", "  git version 2.44.0.windows.1  \nextra line\n", "git version 2.44.0.windows.1"},
		{"empty input", "", ""},
		{"blank first line, content on second", "\nreal content\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstLine(c.value); got != c.want {
				t.Errorf("firstLine(%q) = %q, want %q", c.value, got, c.want)
			}
		})
	}
}

func TestEnvironmentValue(t *testing.T) {
	t.Run("unset reports local/unset", func(t *testing.T) {
		t.Setenv("GOOBERS_WINDOWSVALIDATE_TEST_VAR", "")
		if err := os.Unsetenv("GOOBERS_WINDOWSVALIDATE_TEST_VAR"); err != nil {
			t.Fatal(err)
		}
		if got := environmentValue("GOOBERS_WINDOWSVALIDATE_TEST_VAR"); got != "local/unset" {
			t.Errorf("environmentValue(unset) = %q, want local/unset", got)
		}
	})
	t.Run("set reports the value verbatim", func(t *testing.T) {
		t.Setenv("GOOBERS_WINDOWSVALIDATE_TEST_VAR", "windows-2025")
		if got := environmentValue("GOOBERS_WINDOWSVALIDATE_TEST_VAR"); got != "windows-2025" {
			t.Errorf("environmentValue(set) = %q, want windows-2025", got)
		}
	})
}

func TestRepositoryRootFrom(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/goobers/goobers\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "test", "windowsvalidate")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := repositoryRootFrom(nested)
	if err != nil {
		t.Fatalf("repositoryRootFrom(nested): %v", err)
	}
	// Compare via EvalSymlinks so a TMPDIR that's itself a symlink (e.g. macOS's
	// /var -> /private/var, harmless here as this test only runs on windows
	// build tag anyway, but the pattern generalizes) doesn't spuriously fail.
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	gotRoot, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot != wantRoot {
		t.Errorf("repositoryRootFrom(nested) = %q, want %q", gotRoot, wantRoot)
	}
}

func TestRepositoryRootFromRejectsUnrelatedModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/someone/else\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repositoryRootFrom(dir); err == nil {
		t.Fatal("expected an error walking from a directory whose go.mod names a different module, all the way to the filesystem root")
	}
}

func TestStageGateSequence(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "query-backlog"},
		{Type: journal.EventStageFinished, Stage: "query-backlog"}, // must be ignored
		{Type: journal.EventStageStarted, Stage: "implement"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass"},
		{Type: journal.EventArtifactRecorded, Name: "diff.patch"}, // must be ignored
		{Type: journal.EventGateEvaluated, Gate: "local-gate", Verdict: "fail"},
	}
	want := []string{
		"stage:query-backlog",
		"stage:implement",
		"gate:review=pass",
		"gate:local-gate=fail",
	}
	if got := stageGateSequence(events); !reflect.DeepEqual(got, want) {
		t.Errorf("stageGateSequence = %v, want %v", got, want)
	}
}

func TestStageGateSequenceEmptyForNoMatchingEvents(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventStageFinished, Stage: "query-backlog"},
		{Type: journal.EventArtifactRecorded, Name: "diff.patch"},
	}
	if got := stageGateSequence(events); len(got) != 0 {
		t.Errorf("stageGateSequence = %v, want empty", got)
	}
}
