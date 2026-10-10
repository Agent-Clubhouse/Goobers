package runner

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestParentRetirementFailureDoesNotSkipTerminalFinalization(t *testing.T) {
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "retirement-failure"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	unavailable := errors.New("archive custody unavailable")
	retired, finalized := false, false
	r := &Runner{cfg: Config{
		RetireParentWorkspaces: func(owned *journal.Run) error {
			if owned != run {
				t.Fatal("retirement borrowed another writer")
			}
			reader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			phase, err := reader.Phase()
			if err != nil || phase != journal.PhaseEscalated {
				t.Fatal("retirement preceded durable terminal", phase, err)
			}
			retired = true
			return unavailable
		},
		FinalizeTerminal: func(_ string, phase journal.RunPhase) error {
			if !retired || phase != journal.PhaseEscalated {
				t.Fatal("finalizer lost retirement ordering")
			}
			finalized = true
			return nil
		},
	}}
	result, err := r.finish("retirement-failure", run, journal.PhaseEscalated, "review", 1)
	if !errors.Is(err, unavailable) || !finalized || result.Phase != journal.PhaseEscalated {
		t.Fatal("archive failure prevented terminal cleanup", result, finalized, err)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if journal.PhaseFromEvents(events) != journal.PhaseEscalated {
		t.Fatal("failure changed terminal phase")
	}
	var diagnosed bool
	for _, event := range events {
		diagnosed = diagnosed || event.Type == journal.EventError && event.Error != nil && event.Error.Code == "parent_workspace_retirement_failed"
	}
	if !diagnosed {
		t.Fatal("retirement failure lacked durable diagnosis")
	}
}

func TestParentRetirementRefusesUnsupportedForkHistory(t *testing.T) {
	for _, kind := range []string{"isolated.parent.fork.ready", "isolated.parent.fork.root.ready", "isolated.parent.fork.preparing"} {
		t.Run(kind, func(t *testing.T) {
			run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "unsupported-fork"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = run.Close() }()
			if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": kind}}); err != nil {
				t.Fatal(err)
			}
			reader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if candidates, err := ParentRetirementCandidates(reader); err == nil || len(candidates) != 0 {
				t.Fatal("unsupported workspace ownership was treated as safe to retire", candidates, err)
			}
		})
	}
}
