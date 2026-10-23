package engine

import (
	"fmt"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// fanOutPlan is frozen before controller entry. Static declarations, recorded
// selection results and frozen map rosters adapt to this same queue.
type fanOutPlan struct {
	Name                  string
	Branches              []fanOutBranch
	MaxConcurrentBranches int32
	BranchTimeoutSeconds  int32
	FailurePolicy         apiv1.BranchFailurePolicy
	Join, OnFailure       string
	Started, Finished     journal.Event
}

type fanOutBranch struct {
	ID                       int // ID is the stable declaration/roster ordinal, starting at 1.
	Name, Start              string
	ItemIdentity, ItemDigest string
	Reason                   string
	// PreSettled is only for optional branches skipped by recorded selection.
	PreSettled *journal.BranchOutcome
	Optional   bool
	Bind       func(stage string) (fanOutBindings, error)
}

// fanOutSettlement keeps results and their provenance in descriptor order.
// It never drops the grade attached to each result or context pointer.
type fanOutSettlement struct {
	Outcome  journal.BranchOutcome
	Results  map[string]apiv1.ResultEnvelope
	Pointers []apiv1.ContextPointer
}

type fanOutResult struct {
	Target            string
	Started, Finished livejournal.AppendReceipt
	Settlements       []fanOutSettlement
}

func staticFanOutPlan(p apiv1.Parallel) fanOutPlan {
	plan := fanOutPlan{Name: p.Name, MaxConcurrentBranches: max(1, p.MaxConcurrentBranches),
		BranchTimeoutSeconds: p.BranchTimeoutSeconds, FailurePolicy: p.FailurePolicy, Join: p.Join, OnFailure: p.OnFailure,
		Started: journal.Event{Type: journal.EventParallelStarted}, Finished: journal.Event{Type: journal.EventParallelFinished}}
	for i, branch := range p.Branches {
		plan.Branches = append(plan.Branches, fanOutBranch{ID: i + 1, Name: branch.Name, Start: branch.Start, Optional: branch.Optional})
	}
	return plan
}

func (p fanOutPlan) validate() error {
	if p.Name == "" || p.MaxConcurrentBranches < 1 || p.BranchTimeoutSeconds < 0 {
		return fmt.Errorf("engine: invalid fan-out identity or bounds")
	}
	if p.FailurePolicy != apiv1.BranchFailFast && p.FailurePolicy != apiv1.BranchAllOrNothing && p.FailurePolicy != apiv1.BranchContinueOnError {
		return fmt.Errorf("engine: invalid fan-out failure policy")
	}
	if (p.Started.Type != journal.EventParallelStarted || p.Finished.Type != journal.EventParallelFinished) &&
		(p.Started.Type != journal.EventMapStarted || p.Finished.Type != journal.EventMapFinished) {
		return fmt.Errorf("engine: invalid fan-out lifecycle pair")
	}
	seen := map[string]bool{}
	for i, b := range p.Branches {
		if b.ID != i+1 || b.Name == "" || b.Start == "" || seen[b.Name] {
			return fmt.Errorf("engine: invalid fan-out descriptor %d", i)
		}
		seen[b.Name] = true
		if s := b.PreSettled; s != nil && (!b.Optional || s.Branch != b.ID || s.Name != b.Name || s.Status != journal.BranchSkipped || s.Artifacts != 0 || s.Reason != b.Reason) {
			return fmt.Errorf("engine: invalid pre-settled branch %q", b.Name)
		}
	}
	return nil
}

// fanOut owns every scheduling and commit boundary. Adapters provide data, not
// their own goroutines, emitters or mutable external selection/roster reads.
func (r *runJournal) fanOut(ctx workflow.Context, in RunInput, m *wf.Machine, plan fanOutPlan, seed *branchWalkFrame) (*staticParallelWalk, fanOutResult, error) {
	var result fanOutResult
	if err := plan.validate(); err != nil {
		return nil, result, err
	}
	if r.owner == nil || r.stepBudget == nil || seed == nil {
		return nil, result, fmt.Errorf("engine: fan-out requires owner, shared budget and seed")
	}
	branchCtx, cancel := workflow.WithCancel(ctx)
	defer cancel()
	drainCtx, disconnect := workflow.NewDisconnectedContext(ctx)
	defer disconnect()
	p := &staticParallelWalk{root: r, in: in, machine: m, spec: plan, seed: seed,
		outcomes: make([]*staticBranchOutcome, len(plan.Branches)), results: workflow.NewBufferedChannel(ctx, max(1, len(plan.Branches))),
		ctx: branchCtx, drain: drainCtx, cancel: cancel}
	for i, b := range plan.Branches {
		if b.PreSettled != nil {
			p.outcomes[i] = &staticBranchOutcome{index: i, frame: &branchWalkFrame{status: journal.BranchSkipped}}
		}
	}
	start := plan.Started
	start.Parallel, start.Branch, start.Completeness = plan.Name, 0, p.completeness()
	var err error
	result.Started, err = r.commitLifecycle(ctx, start)
	if err != nil {
		return p, result, err
	}
	p.run(ctx)
	result.Target = p.target()
	finish := plan.Finished
	finish.Parallel, finish.Branch, finish.Completeness, finish.Target = plan.Name, 0, p.completeness(), result.Target
	result.Finished, err = r.commitLifecycle(drainCtx, finish)
	p.fail(err)
	result.Settlements = p.settlements()
	return p, result, p.err
}

func (p *staticParallelWalk) settlements() []fanOutSettlement {
	out := make([]fanOutSettlement, len(p.outcomes))
	for i, outcome := range p.completeness() {
		frame := p.outcomes[i].frame
		out[i] = fanOutSettlement{Outcome: outcome, Results: frame.results}
		start := min(len(p.seed.pointers), len(frame.pointers))
		out[i].Pointers = append([]apiv1.ContextPointer(nil), frame.pointers[start:]...)
	}
	return out
}
