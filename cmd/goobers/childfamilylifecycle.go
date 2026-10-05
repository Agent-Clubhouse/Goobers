package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type childFamilyLifecycle struct {
	layout  instance.Layout
	queue   *triggerqueue.Store
	runners *daemonRunnerRegistry
	after   triggerqueue.ChildParent
}

// Fence is called before delivering cancellation. Even a parent that has not
// issued its first tool grant must be fenced if its pinned graph enables child
// work, closing cancellation against a concurrently prepared grant.
func (f *childFamilyLifecycle) Fence(ctx context.Context, input httpapi.CancelRunRequest) error {
	dir, err := f.layout.FindRunDir(input.RunID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		return err
	}
	id, err := rd.Identity()
	if err != nil {
		return err
	}
	if id.RunID != input.RunID || (input.Gaggle != "" && input.Gaggle != id.Gaggle) {
		return errors.New("child cancellation parent scope differs from journal")
	}
	parent := triggerqueue.ChildParent{Gaggle: id.Gaggle, ParentRunID: id.RunID}
	known, err := f.queue.IsChildParent(ctx, parent)
	if err != nil {
		return err
	}
	if !known {
		machine, err := runner.PinnedWorkflowMachine(rd, id)
		if err != nil {
			// Legacy runs with no pinned definition cannot issue child authority.
			return nil
		}
		for _, task := range machine.Def.Spec.Tasks {
			known = known || task.ChildWorkflows != nil
		}
	}
	if !known {
		return nil
	}
	actor := input.Actor
	if actor == "" {
		actor = "cancel-api"
	}
	return f.queue.FenceChildParent(ctx, parent, actor, time.Now())
}

// Sweep bounds work per drain. Failure/escalation remains open for existing
// human continuation paths; completion and abort close the logical family only
// after its execution owner has left the registry. Unacknowledged children stay
// pinned by the queue's independent retention predicates.
func (f *childFamilyLifecycle) Sweep(ctx context.Context) error {
	parents, err := f.queue.UnsettledChildParents(ctx, f.after, 100)
	if err != nil {
		return err
	}
	if len(parents) == 0 {
		f.after = triggerqueue.ChildParent{}
		return nil
	}
	active := map[string]bool{}
	for _, run := range f.runners.ActiveRuns() {
		active[run.RunID] = true
	}
	var failures error
	for _, parent := range parents {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		f.after = parent
		if active[parent.ParentRunID] {
			continue
		}
		failures = errors.Join(failures, f.settle(ctx, parent))
	}
	return failures
}

func (f *childFamilyLifecycle) settle(ctx context.Context, parent triggerqueue.ChildParent) error {
	release, available := f.runners.acquireChildCustody(parent.ParentRunID)
	if !available {
		return nil
	}
	defer release()
	dir, err := f.layout.FindRunDir(parent.ParentRunID)
	if err != nil {
		return err
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		return err
	}
	id, err := rd.Identity()
	if err != nil {
		return err
	}
	if id.RunID != parent.ParentRunID || id.Gaggle != parent.Gaggle {
		return errors.New("child family journal identity differs")
	}
	phase, err := rd.PhaseBounded(ctx)
	if err != nil || (phase != journal.PhaseCompleted && phase != journal.PhaseAborted) {
		return err
	}
	if err = requireRetiredParentContributions(rd); err != nil {
		return err
	}
	if err = f.queue.FenceChildParent(ctx, parent, "parent-terminal", time.Now()); err != nil {
		return err
	}
	return f.queue.MarkChildParentSettled(ctx, parent, time.Now())
}

// Archive retention follows the queue's full-lineage lifetime, not just live
// journals. Closing/compacting a parent journal cannot erase accepted source
// provenance. Install once during daemon startup before concurrent pruning.
func attachChildGenerationPins(retainer *configgeneration.Retainer, queue *triggerqueue.Store) {
	if retainer == nil {
		return
	}
	previous := retainer.DurablePins
	retainer.DurablePins = func(ctx context.Context) (map[string]bool, error) {
		pins := map[string]bool{}
		if previous != nil {
			var err error
			pins, err = previous(ctx)
			if err != nil {
				return nil, err
			}
		}
		if pins == nil {
			pins = map[string]bool{}
		}
		for after := ""; ; {
			records, err := queue.RetainedChildStarts(ctx, after, 100)
			if err != nil {
				return nil, err
			}
			for _, record := range records {
				envelope, err := childworkflow.DecodeStartEnvelope(record.Payload)
				if err != nil {
					return nil, fmt.Errorf("retained child generation pin: %w", err)
				}
				pins[envelope.ConfigGeneration] = true
				after = record.ID
			}
			if len(records) < 100 {
				return pins, nil
			}
		}
	}
}
