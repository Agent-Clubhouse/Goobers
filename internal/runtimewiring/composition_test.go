package runtimewiring_test

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/worktree"
)

func TestCompositionInstallsWorkspaceGuardsOnNewAndReusedManagers(t *testing.T) {
	layout := instance.NewLayout(initDemo(t)).ForGaggle("example")
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	var observed *worktree.Manager
	input := runtimewiring.RunnerCompositionInput{
		Layout: layout, Config: cfg, SharedRegistry: journal.NewRegistryScrubber(),
		ConfigureWorktree: func(manager *worktree.Manager) error {
			if manager == nil {
				t.Fatal("cleanup guards received a nil manager")
			}
			calls++
			observed = manager
			return nil
		},
	}
	_, manager, err := runtimewiring.BuildRunnerConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || observed != manager {
		t.Fatalf("new manager guard wiring: calls=%d observed=%p returned=%p", calls, observed, manager)
	}
	input.WorktreeManager = manager
	_, reused, err := runtimewiring.BuildRunnerConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || reused != manager || observed != reused {
		t.Fatalf("reused manager guard wiring: calls=%d reused=%p original=%p observed=%p", calls, reused, manager, observed)
	}
	refused := errors.New("recovery ownership is ambiguous")
	input.ConfigureWorktree = func(*worktree.Manager) error { return refused }
	if _, _, err := runtimewiring.BuildRunnerConfig(input); !errors.Is(err, refused) {
		t.Fatalf("cleanup guard failure was ignored: %v", err)
	}
}
