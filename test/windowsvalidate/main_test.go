//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestValidateImplementationJournal(t *testing.T) {
	runDir := implementationJournal(t, true, implementationEvents())
	if err := validateImplementationJournal(runDir); err != nil {
		t.Fatalf("validateImplementationJournal: %v", err)
	}
}

func TestValidateImplementationJournalRejectsIncompleteRun(t *testing.T) {
	runDir := implementationJournal(t, false, implementationEvents())
	err := validateImplementationJournal(runDir)
	if err == nil || !strings.Contains(err.Error(), `phase = "running", want "completed"`) {
		t.Fatalf("validateImplementationJournal error = %v, want running phase rejection", err)
	}
}

func TestValidateImplementationJournalRejectsWrongSequence(t *testing.T) {
	events := implementationEvents()
	events = events[:len(events)-1]
	runDir := implementationJournal(t, true, events)
	err := validateImplementationJournal(runDir)
	if err == nil || !strings.Contains(err.Error(), "implementation workflow sequence") {
		t.Fatalf("validateImplementationJournal error = %v, want sequence rejection", err)
	}
}

func TestConfigureEphemeralAPI(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "instance.yaml")
	if err := instance.WriteConfig(path, &instance.Config{
		APIVersion: instance.ConfigAPIVersion,
		Kind:       instance.ConfigKind,
	}); err != nil {
		t.Fatal(err)
	}

	if err := configureEphemeralAPI(root); err != nil {
		t.Fatal(err)
	}
	config, err := instance.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.API.Listen != ephemeralAPIListenAddress {
		t.Fatalf("API listen = %q, want %q", config.API.Listen, ephemeralAPIListenAddress)
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "first\r\nsecond\r\n", want: "first"},
		{input: "  only  ", want: "only"},
		{input: "", want: ""},
	}
	for _, test := range tests {
		if got := firstLine(test.input); got != test.want {
			t.Errorf("firstLine(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func implementationJournal(t *testing.T, completed bool, events []journal.Event) string {
	t.Helper()
	runsDir := t.TempDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           "0af7651916cd43dd8448eb211c80319c",
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "issue-2031"},
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	for _, event := range events {
		if err := run.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if completed {
		if err := run.Append(journal.Event{
			Type:   journal.EventRunFinished,
			Status: string(journal.PhaseCompleted),
		}); err != nil {
			t.Fatalf("finish journal: %v", err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	return filepath.Join(runsDir, "0af7651916cd43dd8448eb211c80319c")
}

func implementationEvents() []journal.Event {
	return []journal.Event{
		{Type: journal.EventStageStarted, Stage: "query-backlog"},
		{Type: journal.EventStageStarted, Stage: "gather-implement-context"},
		{Type: journal.EventStageStarted, Stage: "implement"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass"},
		{Type: journal.EventStageStarted, Stage: "push-branch"},
		{Type: journal.EventStageStarted, Stage: "local-ci"},
		{Type: journal.EventGateEvaluated, Gate: "local-gate", Verdict: "pass"},
		{Type: journal.EventStageStarted, Stage: "open-pr"},
		{Type: journal.EventGateEvaluated, Gate: "open-pr-gate", Verdict: "pass"},
		{Type: journal.EventStageStarted, Stage: "ci-poll"},
		{Type: journal.EventGateEvaluated, Gate: "ci-gate", Verdict: "pass"},
		{Type: journal.EventStageStarted, Stage: "close-out"},
	}
}
