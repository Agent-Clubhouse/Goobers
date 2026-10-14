package spec

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// ResultRequest is supplied by the host after branch writers have stopped.
// Seed and Plan identify the original fork; Custody identifies its exact checkout.
type ResultRequest struct {
	Request
	Plan    journal.Ref
	Seed    Source
	Branch  int
	Status  journal.BranchStatus
	Custody worktree.StageCustody
	Join    bool
}
