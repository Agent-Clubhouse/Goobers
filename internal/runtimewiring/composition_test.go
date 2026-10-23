package runtimewiring_test

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

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
	input := runtimewiring.ExecutorCompositionInput{
		ExecutionFence: func(ctx context.Context, _ apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
			return ctx, func() {}, nil
		},
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
	_, manager, err := runtimewiring.BuildExecutorConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || observed != manager {
		t.Fatalf("new manager guard wiring: calls=%d observed=%p returned=%p", calls, observed, manager)
	}
	input.WorktreeManager = manager
	_, reused, err := runtimewiring.BuildExecutorConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || reused != manager || observed != reused {
		t.Fatalf("reused manager guard wiring: calls=%d reused=%p original=%p observed=%p", calls, reused, manager, observed)
	}
	refused := errors.New("recovery ownership is ambiguous")
	input.ConfigureWorktree = func(*worktree.Manager) error { return refused }
	if _, _, err := runtimewiring.BuildExecutorConfig(input); !errors.Is(err, refused) {
		t.Fatalf("cleanup guard failure was ignored: %v", err)
	}
}

func TestCompositionRequiresExecutionFence(t *testing.T) {
	if _, _, err := runtimewiring.BuildExecutorConfig(runtimewiring.ExecutorCompositionInput{}); err == nil {
		t.Fatal("accepted runner without execution fence")
	}
}
