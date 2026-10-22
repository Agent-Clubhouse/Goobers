package runner

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// Selective parallels (DSL 3.1, design plan-driven-dynamic-fan-out.md §4.2,
// §5). A selective parallel freezes one selection document before any branch
// starts, writes the normalized result into parallel.started, and from then
// on never consults the producer again — not on resume, not on rerun.

// FanOutSourceInvalidCode is the typed failure of a fan-out state whose
// source document cannot be frozen. It takes the fan-out's onFailure route,
// or @abort when none is declared; it is never a silent empty fan-out.
const FanOutSourceInvalidCode = "fanout_source_invalid"

// WorkflowIntegrityCode is the typed refusal for a journal whose recorded
// fan-out state is missing, extra, duplicated or self-contradictory.
const WorkflowIntegrityCode = "workflow_integrity"

// FanOutSourceError is a fan-out source that failed to freeze (§4.2 steps
// 1–3). It carries FanOutSourceInvalidCode.
type FanOutSourceError struct {
	// State is the fan-out state that reads the source.
	State string
	// Source is the declared producer task and slot.
	Source apiv1.FanOutSource
	// Err is the reason the document was refused.
	Err error
}

func (e *FanOutSourceError) Error() string {
	return fmt.Sprintf("runner: fan-out %q source %s.%s is invalid: %v", e.State, e.Source.Task, e.Source.Slot, e.Err)
}

// Unwrap exposes the underlying refusal.
func (e *FanOutSourceError) Unwrap() error { return e.Err }

// StageErrorCode classifies the failure as FanOutSourceInvalidCode.
func (e *FanOutSourceError) StageErrorCode() string { return FanOutSourceInvalidCode }

// WorkflowIntegrityError refuses a resume or rerun whose recorded fan-out
// state conflicts with itself or with the pinned definition. It is never
// repaired by rerunning the source task.
type WorkflowIntegrityError struct {
	// State is the fan-out state whose record is inconsistent.
	State string
	// Reason names the conflict.
	Reason string
}

func (e *WorkflowIntegrityError) Error() string {
	return fmt.Sprintf("runner: workflow integrity: parallel %q: %s; refusing to continue (the source task is never rerun to repair a fan-out record)", e.State, e.Reason)
}

// StageErrorCode classifies the refusal as WorkflowIntegrityCode.
func (e *WorkflowIntegrityError) StageErrorCode() string { return WorkflowIntegrityCode }

// freezeParallelSelection resolves and validates a selective parallel's
// source document and returns its normalized selection. It returns nil for a
// parallel without selection, which then journals exactly what it always has.
// A non-nil error is always a *FanOutSourceError.
func (r *Runner) freezeParallelSelection(ctx context.Context, jr *journal.Run, machine *workflow.Machine, p apiv1.Parallel) (*journal.ParallelSelection, error) {
	if p.Selection == nil {
		return nil, nil
	}
	source := p.Selection.Source
	refuse := func(format string, args ...any) error {
		return &FanOutSourceError{State: p.Name, Source: source, Err: fmt.Errorf(format, args...)}
	}
	payload, err := resolveSelectionSource(ctx, jr, machine, source)
	if err != nil {
		return nil, &FanOutSourceError{State: p.Name, Source: source, Err: err}
	}
	ref := journal.FanOutSourceRef{
		Task: source.Task, Slot: source.Slot,
		Visit: payload.Binding.Visit, Attempt: int(payload.Binding.Attempt),
		Digest: payload.Binding.Artifact.Digest, Integrity: payload.Binding.Artifact.Integrity,
		Path: payload.Binding.Artifact.Path, Size: payload.Binding.Artifact.Size,
		MediaType: payload.Binding.Artifact.MediaType,
	}
	selection, err := fanout.FreezeSelection(p, ref, payload.Bytes)
	if err != nil {
		return nil, refuse("%v", err)
	}
	return &selection, nil
}

// resolveSelectionSource reads the source slot from the producer's most
// recent recorded attempt on the root branch — the journal-bound producer
// and digest — never from the live pointer accumulation, and never from a
// producer still to run. The byte bound is checked before the payload is
// read or decoded.
func resolveSelectionSource(ctx context.Context, jr *journal.Run, machine *workflow.Machine, source apiv1.FanOutSource) (artifactset.SlotPayload, error) {
	var empty artifactset.SlotPayload
	reader, err := journal.OpenReadOnly(jr.Dir())
	if err != nil {
		return empty, err
	}
	events, err := reader.Events()
	if err != nil {
		return empty, err
	}
	finished, ok := latestProducerAttempt(events, source.Task)
	if !ok {
		return empty, fmt.Errorf("producer %q has no recorded attempt", source.Task)
	}
	if finished.Status != string(apiv1.ResultSuccess) {
		return empty, fmt.Errorf("producer %q attempt %d finished %s, not success", source.Task, finished.Attempt, finished.Status)
	}
	limit := workflow.MaxFanOutSourceBytes
	if task, ok := machine.Task(source.Task); ok {
		for _, slot := range task.ArtifactSlots {
			if slot.Name == source.Slot && slot.MaxSize > 0 && slot.MaxSize < limit {
				limit = slot.MaxSize
			}
		}
	}
	artifacts, err := artifactset.OpenJournal(jr.Dir())
	if err != nil {
		return empty, err
	}
	defer func() { _ = artifacts.Close() }()
	pointers := contextPointersFor(source.Task, artifactPointersFrom(finished.Artifacts))
	payload, err := artifactset.ResolveSlot(ctx, artifacts, pointers, source.Task, source.Slot, limit)
	if err != nil {
		return empty, err
	}
	if int(payload.Binding.Attempt) != finished.Attempt {
		return empty, fmt.Errorf("slot binding attempt %d differs from the recorded producer attempt %d", payload.Binding.Attempt, finished.Attempt)
	}
	return payload, nil
}

func latestProducerAttempt(events []journal.Event, task string) (journal.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type == journal.EventStageFinished && event.Branch == 0 && event.Stage == task && !isInterruptedAttemptMarker(event) {
			return event, true
		}
	}
	return journal.Event{}, false
}

// skippedBranchReason is the reason recorded for a branch the selection did
// not choose.
func skippedBranchReason(selection *journal.ParallelSelection) string {
	return fmt.Sprintf("not selected by %s.%s", selection.Source.Task, selection.Source.Slot)
}

// applySelection settles every optional branch the frozen selection did not
// choose as skipped, before any branch starts, and records each selected
// branch's reason. Branch ids are untouched: they stay in declaration order
// whatever the selection. A nil selection is a no-op.
func (p *parallelExec) applySelection(selection *journal.ParallelSelection) {
	if selection == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	selected := make(map[string]bool, len(selection.Selected))
	for _, name := range selection.Selected {
		selected[name] = true
	}
	for i, branch := range p.branches {
		if !p.spec.Branches[i].Optional {
			continue
		}
		if selected[branch.name] {
			branch.reason = selection.Reasons[branch.name]
			continue
		}
		branch.settled = true
		branch.status = journal.BranchSkipped
		branch.machine = ""
		branch.reason = skippedBranchReason(selection)
	}
	p.active = len(p.branches)
	for i, branch := range p.branches {
		if !branch.settled {
			p.active = i
			break
		}
	}
}

// runnable reports how many branches the selection left to run.
func (p *parallelExec) runnable() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, branch := range p.branches {
		if branch.status != journal.BranchSkipped {
			count++
		}
	}
	return count
}

// routeFanOutSourceFailure journals a typed fanout_source_invalid failure of
// the parallel state and follows its onFailure route, or @abort when none is
// declared. No parallel.started is written: no branch ever started.
func (r *Runner) routeFanOutSourceFailure(ws *walkState, p apiv1.Parallel, cause error) (string, Result, bool, error) {
	if err := ws.jr.Append(journal.Event{
		Type: journal.EventError, Parallel: p.Name,
		Error: journal.ErrorDetailFor(FanOutSourceInvalidCode, cause),
	}); err != nil {
		return "", Result{}, true, err
	}
	target := p.OnFailure
	if target == "" {
		target = workflow.TargetAbort
	}
	switch target {
	case workflow.TargetAbort:
		res, err := r.finish(ws.in.RunID, ws.jr, journal.PhaseAborted, p.Name, ws.steps)
		return "", res, true, err
	case workflow.TargetEscalate:
		res, err := r.finish(ws.in.RunID, ws.jr, journal.PhaseEscalated, p.Name, ws.steps)
		return "", res, true, err
	}
	ws.fanIn = nil
	return target, Result{}, false, nil
}

// verifyRecordedSelections checks every recorded selective-parallel visit
// against itself and the pinned definition before a resume or rerun trusts
// it (§8). It is the only place a skip set is ever re-derived, and it
// re-derives it from parallel.started alone.
func verifyRecordedSelections(events []journal.Event, machine *workflow.Machine) error {
	for i, started := range events {
		if started.Type != journal.EventParallelStarted || started.Branch != 0 {
			continue
		}
		spec, ok := machine.Parallel(started.Parallel)
		if !ok {
			continue
		}
		if (spec.Selection != nil) != (started.Selection != nil) {
			return &WorkflowIntegrityError{State: spec.Name, Reason: "parallel.started selection presence differs from the pinned definition"}
		}
		if started.Selection == nil {
			continue
		}
		skipped, err := recordedSkipSet(spec, started)
		if err != nil {
			return err
		}
		if err := verifySkippedBranchesStayedIdle(spec, skipped, events[i+1:]); err != nil {
			return err
		}
	}
	return nil
}

// recordedSkipSet validates one parallel.started selection and returns the
// branch ids it skipped.
func recordedSkipSet(spec apiv1.Parallel, started journal.Event) (map[int]bool, error) {
	conflict := func(format string, args ...any) error {
		return &WorkflowIntegrityError{State: spec.Name, Reason: fmt.Sprintf(format, args...)}
	}
	selection := started.Selection
	if selection.Source.Task != spec.Selection.Source.Task || selection.Source.Slot != spec.Selection.Source.Slot {
		return nil, conflict("recorded selection source %s.%s differs from the declared source", selection.Source.Task, selection.Source.Slot)
	}
	optional := map[string]int{}
	for i, branch := range spec.Branches {
		if branch.Optional {
			optional[branch.Name] = i + 1
		}
	}
	selected := map[string]bool{}
	previous := 0
	for _, name := range selection.Selected {
		id, ok := optional[name]
		switch {
		case !ok:
			return nil, conflict("recorded selection names %q, which is not a declared optional branch", name)
		case selected[name]:
			return nil, conflict("recorded selection names %q more than once", name)
		case id < previous:
			return nil, conflict("recorded selection is not in branch-id order")
		}
		selected[name], previous = true, id
	}
	for name := range selection.Reasons {
		if !selected[name] {
			return nil, conflict("recorded reason for %q, which the selection did not choose", name)
		}
	}
	if len(started.Completeness) != len(spec.Branches) {
		return nil, conflict("parallel.started records %d branches, the definition declares %d", len(started.Completeness), len(spec.Branches))
	}
	skipped := map[int]bool{}
	for i, branch := range spec.Branches {
		entry := started.Completeness[i]
		skip := branch.Optional && !selected[branch.Name]
		if entry.Branch != i+1 || entry.Name != branch.Name || (entry.Status == journal.BranchSkipped) != skip {
			return nil, conflict("parallel.started completeness entry %d differs from the recorded selection", i+1)
		}
		if skip {
			skipped[i+1] = true
		}
	}
	return skipped, nil
}

// verifySkippedBranchesStayedIdle refuses a visit in which a skipped branch
// left any trace, or whose parallel.finished disagrees with the skip set.
func verifySkippedBranchesStayedIdle(spec apiv1.Parallel, skipped map[int]bool, after []journal.Event) error {
	for _, event := range after {
		if event.Type == journal.EventParallelStarted && event.Branch == 0 && event.Parallel == spec.Name {
			return nil
		}
		if event.Type == journal.EventParallelFinished && event.Parallel == spec.Name {
			return verifyFinishedSkipSet(spec, skipped, event)
		}
		if skipped[event.Branch] && branchActivityEvent(event.Type) {
			return &WorkflowIntegrityError{State: spec.Name, Reason: fmt.Sprintf("skipped branch %d recorded %s", event.Branch, event.Type)}
		}
		if event.Type == journal.EventBranchFinished && event.Parallel == spec.Name && event.BranchStatus == journal.BranchSkipped {
			return &WorkflowIntegrityError{State: spec.Name, Reason: fmt.Sprintf("branch %d finished as skipped after it was chosen", event.Branch)}
		}
	}
	return nil
}

func verifyFinishedSkipSet(spec apiv1.Parallel, skipped map[int]bool, finished journal.Event) error {
	if len(finished.Completeness) != len(spec.Branches) {
		return &WorkflowIntegrityError{State: spec.Name, Reason: "parallel.finished completeness does not cover every declared branch"}
	}
	for i, entry := range finished.Completeness {
		if entry.Branch != i+1 || (entry.Status == journal.BranchSkipped) != skipped[i+1] {
			return &WorkflowIntegrityError{State: spec.Name, Reason: fmt.Sprintf("parallel.finished entry %d differs from the frozen skip set", i+1)}
		}
	}
	return nil
}

func branchActivityEvent(kind journal.EventType) bool {
	switch kind {
	case journal.EventBranchStarted, journal.EventBranchFinished, journal.EventStageStarted, journal.EventStageFinished,
		journal.EventGatePaused, journal.EventGateStarted, journal.EventGateEvaluated, journal.EventArtifactRecorded:
		return true
	}
	return false
}

// refuseSkippedBranchRerun refuses a human rerun of a stage whose owning
// branch the active visit's frozen selection skipped: rerunning it would run
// a branch the selection never chose.
func refuseSkippedBranchRerun(active *parallelExec, owner *branchState) error {
	if active == nil || owner == nil || owner.status != journal.BranchSkipped {
		return nil
	}
	return &WorkflowIntegrityError{State: active.spec.Name, Reason: fmt.Sprintf("rerun target belongs to branch %q, which the frozen selection skipped", owner.name)}
}

var errSelectiveChildParallel = errors.New("a selective parallel whose branches can start child workflows needs per-branch fork custody for selected branches only, which the local runner does not implement yet")

// refuseSelectiveChildParallel keeps the run-start refusal for the one
// selective-parallel shape the local runner cannot execute faithfully: a
// child-capable parallel reserves one isolated fork per DECLARED branch, and
// a skipped branch must create none (§5.2, §5.4).
func refuseSelectiveChildParallel(machine *workflow.Machine) error {
	for _, parallel := range machine.Def.Spec.Parallels {
		if parallel.Selection != nil && parallelHasChildStage(machine, parallel) {
			return &workflow.UnsupportedFanOutError{
				State: parallel.Name, Feature: "spec.parallels.selection",
				Err: fmt.Errorf("%w: %w", workflow.ErrSelectiveParallelUnsupported, errSelectiveChildParallel),
			}
		}
	}
	return nil
}
