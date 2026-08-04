package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// OverrideGateInput selects a configured branch for a nondeterministic gate
// whose verdict ended the run.
type OverrideGateInput struct {
	RunID        string
	Machine      *workflow.Machine
	GooberDigest string
	RepoRef      apiv1.RepoRef
	Gate         string
	Verdict      string
	Actor        string
	Rationale    string
}

// OverrideGate records an operator's rationale and continues a terminal run
// down the selected agentic or human gate branch.
func (r *Runner) OverrideGate(ctx context.Context, in OverrideGateInput) (Result, error) {
	if !apiv1.ValidRunID(in.RunID) {
		return Result{}, fmt.Errorf("runner: invalid run id %q", in.RunID)
	}
	if in.Machine == nil {
		return Result{}, fmt.Errorf("runner: Machine is required")
	}
	in.Gate = strings.TrimSpace(in.Gate)
	in.Verdict = strings.TrimSpace(in.Verdict)
	in.Actor = strings.TrimSpace(in.Actor)
	in.Rationale = strings.TrimSpace(in.Rationale)
	if in.Gate == "" {
		return Result{}, fmt.Errorf("runner: Gate is required")
	}
	if in.Verdict == "" {
		return Result{}, fmt.Errorf("runner: Verdict is required")
	}
	if in.Actor == "" {
		return Result{}, fmt.Errorf("runner: Actor is required")
	}
	if in.Rationale == "" {
		return Result{}, fmt.Errorf("runner: Rationale is required")
	}

	g, ok := in.Machine.Gate(in.Gate)
	if !ok {
		return Result{}, fmt.Errorf("runner: gate %q is not defined by workflow %q", in.Gate, in.Machine.Def.Name)
	}
	if g.Evaluator != apiv1.EvaluatorAgentic && g.Evaluator != apiv1.EvaluatorHuman {
		return Result{}, fmt.Errorf("runner: gate %q is deterministic and cannot be overridden", in.Gate)
	}
	target, ok := g.Branches[in.Verdict]
	if !ok {
		return Result{}, fmt.Errorf("runner: verdict %q has no configured branch on gate %q", in.Verdict, in.Gate)
	}
	journalTarget := target
	if journalTarget == workflow.TerminalComplete {
		journalTarget = journal.TargetComplete
	}

	dir := filepath.Join(r.cfg.RunsDir, in.RunID)
	registrar, scrubber := journal.DefaultScrubber()
	jr, _, err := journal.Recover(dir, journal.WithScrubber(scrubber), journal.WithAppendObserver(r.cfg.JournalAdvanced))
	if err != nil {
		return Result{}, fmt.Errorf("runner: recover run %q for gate override: %w", in.RunID, err)
	}
	defer func() { _ = jr.Close() }()

	return r.withActiveRun(ctx, in.RunID, jr, func(ctx context.Context) (Result, error) {
		rd, err := journal.OpenRead(dir)
		if err != nil {
			return Result{}, fmt.Errorf("runner: open run %q for gate override: %w", in.RunID, err)
		}
		id, err := rd.Identity()
		if err != nil {
			return Result{}, fmt.Errorf("runner: read identity for run %q: %w", in.RunID, err)
		}
		phase, err := rd.Phase()
		if err != nil {
			return Result{}, fmt.Errorf("runner: reconstruct phase for run %q: %w", in.RunID, err)
		}
		if id.Workflow != in.Machine.Def.Name ||
			id.WorkflowVersion != in.Machine.Def.Version ||
			id.WorkflowDigest == "" ||
			id.WorkflowDigest != in.Machine.Digest() ||
			(id.GooberDigest != "" && id.GooberDigest != in.GooberDigest) {
			return Result{}, fmt.Errorf("runner: run %q workflow or goober pin does not match the override definition (WF-016)", in.RunID)
		}
		events, err := rd.Events()
		if err != nil {
			return Result{}, fmt.Errorf("runner: read events for run %q: %w", in.RunID, err)
		}
		if !isCurrentTerminalGate(events, in.Gate, phase) {
			return Result{}, fmt.Errorf("runner: gate %q did not cause the run's current terminal phase %s", in.Gate, phase)
		}

		if err := jr.Append(journal.Event{
			Type:            journal.EventGateOverridden,
			Gate:            in.Gate,
			Verdict:         in.Verdict,
			Target:          journalTarget,
			Actor:           in.Actor,
			Rationale:       in.Rationale,
			Status:          string(phase),
			WorkflowVersion: id.WorkflowVersion,
			WorkflowDigest:  id.WorkflowDigest,
		}); err != nil {
			return Result{}, fmt.Errorf("runner: journal override for gate %q: %w", in.Gate, err)
		}

		return r.resumeOwned(ctx, ResumeInput{
			RunID: in.RunID, Machine: in.Machine, GooberDigest: in.GooberDigest, RepoRef: in.RepoRef,
		}, jr, registrar, dir)
	})
}

func isCurrentTerminalGate(events []journal.Event, gate string, phase journal.RunPhase) bool {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case journal.EventGateEvaluated:
			if events[i].Gate != gate {
				return false
			}
			switch phase {
			case journal.PhaseCompleted:
				return events[i].Target == workflow.TerminalComplete ||
					events[i].Target == journal.TargetComplete
			case journal.PhaseAborted:
				return events[i].Target == workflow.TargetAbort
			case journal.PhaseEscalated:
				return events[i].Target == workflow.TargetEscalate || events[i].Escalated
			default:
				return false
			}
		case journal.EventRunResumed, journal.EventGateOverridden, journal.EventStageRerunRequested:
			return false
		}
	}
	return false
}
