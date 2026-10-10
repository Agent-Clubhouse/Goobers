package runner

import (
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/worktree"
)

func TestParallelForkBudgetReservesWholeFanoutAndDeduplicatesHolds(t *testing.T) {
	states := map[uint64]*parentForkState{}
	state := &parentForkState{}
	for i := 0; i < 127; i++ {
		state.plan.Workspaces = append(state.plan.Workspaces, worktree.StageCustody{WorkspaceID: fmt.Sprintf("fork-%d", i), Branch: fmt.Sprintf("branch-%d", i)})
	}
	states[1] = state
	newFork := worktree.StageCustody{WorkspaceID: "new", Branch: "new"}
	if err := reserveParentForks(nil, states, []worktree.StageCustody{state.plan.Workspaces[0], newFork}); err != nil {
		t.Fatal("same owner counted twice", err)
	}
	extra := worktree.StageCustody{WorkspaceID: "extra", Branch: "extra"}
	if err := reserveParentForks(nil, states, []worktree.StageCustody{newFork, extra}); err == nil {
		t.Fatal("partial fanout could exceed bound")
	}
	changed := state.plan.Workspaces[0]
	changed.StartRef = "changed"
	if err := reserveParentForks(nil, states, []worktree.StageCustody{changed}); err == nil {
		t.Fatal("workspace identity changed")
	}
}
