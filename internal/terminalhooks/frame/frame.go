// Package frame is the terminal-hook frame: the instance-level policy fired for
// one finished engine run, in the local runner's order, with every hook failure
// recorded and none allowed to skip a later hook. It lives apart from
// internal/terminalhooks (outcome types and helpers) because it imports the
// engine, which the runner's dependency chain must not.
package frame

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/terminalhooks"
)

// Outcome is one finished engine run, as the frame sees it.
type Outcome struct {
	// RunID, Gaggle and Workflow identify the run for the instance log.
	RunID    string
	Gaggle   string
	Workflow string
	Backprop bool
	// Phase is the journal phase the run reached: engine.PhaseForStatus of the
	// workflow's status, never a re-derivation from the status word.
	Phase journal.RunPhase
	// Result is the workflow's own return value. Zero when the run ended through
	// an error rather than a status (Err is then non-nil).
	Result engine.RunResult
	// Item is the driving backlog item, when the run had one at start.
	Item *apiv1.BacklogItem
	// Err is the workflow's terminal error, if it ended by failing rather than
	// by returning.
	Err error
}

// Recorder is the instance-level sink terminal cleanup records to. The run's
// own journal is closed and complete, so nothing here appends to it.
type Recorder interface {
	AppendBestEffort(journal.Event)
}

// Hooks is the terminal-hook frame: the instance-level policy fired for one
// finished run. A nil hook is skipped. The handlers are the SAME closures the
// local runner is configured with; the cloud supplies PostgreSQL- and
// provider-backed ones.
type Hooks struct {
	SelfExecutionObserved func(bool)
	RepoRef               apiv1.RepoRef

	ExistingFix terminalhooks.ExistingFixHandler
	Blocked     terminalhooks.BlockedHandler
	Failed      terminalhooks.FailedHandler
	// Escalate posts one stage-escalation comment for one driving item.
	Escalate func(ctx context.Context, out Outcome, itemID, stage, reason string) error
	// ClaimedItems resolves the items a run currently claims (claim ledger).
	ClaimedItems func(runID string) ([]string, error)
	// Context returns the context hooks run under (for example provider-write
	// attribution of the run that caused them); nil keeps ctx.
	Context func(ctx context.Context, out Outcome) context.Context
	// Prepare is branch cleanup and the aborted-run PR label.
	Prepare func(ctx context.Context, out Outcome) error
	// Notify is the circuit breaker, then the terminal notification.
	Notify func(ctx context.Context, out Outcome) error
	// Finalize releases the claim ledger lease and the provider claim marker. It
	// must be idempotent: it runs again when a run is observed already terminal.
	Finalize func(ctx context.Context, out Outcome) error
	// Attribute is backprop attribution, isolated after publication.
	Attribute func(ctx context.Context, out Outcome) error

	Record Recorder
}

// ExistingFixStage is the one stage name the #3236 arm applies to.
const ExistingFixStage = "implement"

// Run fires the terminal hooks for one finished run, in the local runner's
// order, which is load-bearing:
//
//  1. ExistingFix (#3236): strips labels that would otherwise let the item be
//     reclaimed between here and the claim release.
//  2. Escalation, then Blocked (#544/#545): the escalation comment cites the
//     stage's reason and the blocked record parks the item; both run while the
//     claim ledger still holds this run's claims, because both resolve the
//     driving item from those claims.
//  3. Failed (#1054): same claim-lifetime requirement.
//  4. Prepare: branch cleanup and the aborted-run PR label.
//  5. Notify: the circuit breaker, then the terminal notification.
//  6. Finalize: releases the claims. Last among the required publication
//     hooks, because everything above needs them.
//  7. Attribute: after publication and release so analysis latency cannot delay
//     terminal cleanup.
//
// Handler failures are recorded and never abort the remaining hooks: a
// blocked-notification failure that skipped Finalize would leak the very claims
// this frame exists to release.
func (h *Hooks) Run(ctx context.Context, out Outcome) { _ = h.RunFinalizing(ctx, out) }

// RunFinalizing is Run that also returns Finalize's error (already recorded), so
// a caller that can retry the release knows to. Other hooks' failures stay
// recorded-only.
func (h *Hooks) RunFinalizing(ctx context.Context, out Outcome) error {
	if h == nil {
		return nil
	}
	if h.Context != nil {
		ctx = h.Context(ctx, out)
	}
	h.fireExistingFix(ctx, out)
	h.fireBlocked(ctx, out)
	h.fireFailed(ctx, out)
	_ = h.fire(ctx, out, h.Prepare, "engine_terminal_prepare_failed")
	// Errors are recorded, never fatal: notification delivery can never affect
	// run processing.
	_ = h.fire(ctx, out, h.Notify, "engine_terminal_notification_failed")
	finalizeErr := h.fire(ctx, out, h.Finalize, "engine_terminal_finalize_failed")
	if out.Backprop {
		_ = h.fire(ctx, out, h.Attribute, "backprop_attribution_failed")
	}
	return finalizeErr
}

// Refinalize runs only Finalize (under the same Context), for retrying a release
// that failed. Finalize is idempotent.
func (h *Hooks) Refinalize(ctx context.Context, out Outcome) error {
	if h == nil {
		return nil
	}
	if h.Context != nil {
		ctx = h.Context(ctx, out)
	}
	return h.fire(ctx, out, h.Finalize, "engine_terminal_finalize_failed")
}

func (h *Hooks) fire(ctx context.Context, out Outcome, hook func(context.Context, Outcome) error, code string) error {
	if hook == nil {
		return nil
	}
	err := hook(ctx, out)
	if err != nil {
		h.recordFailure(out, "", code, err)
	}
	return err
}

func (h *Hooks) fireExistingFix(ctx context.Context, out Outcome) {
	if h.ExistingFix == nil || out.Phase != journal.PhaseCompleted {
		return
	}
	env, ok := out.Result.Outputs[ExistingFixStage]
	if !ok || env.Status != apiv1.ResultNoWork {
		return
	}
	commit, _ := env.Outputs["existingFixCommit"].(string)
	if commit == "" {
		return
	}
	if err := h.ExistingFix(ctx, terminalhooks.ExistingFixOutcome{RunID: out.RunID, ItemID: h.itemID(out), RepoRef: h.RepoRef, Commit: commit}); err != nil {
		h.recordFailure(out, ExistingFixStage, "existingfix_handling_failed", err)
	}
}

// fireBlocked mirrors the runner's #544 blocked terminal. The trigger is not the
// phase alone: a stage reporting blocked ends the walk at escalated, and so does
// an @escalate route; the two are told apart by the final stage's own status.
func (h *Hooks) fireBlocked(ctx context.Context, out Outcome) {
	if engine.IsSelfExecutionDenied(out.Err) {
		if h.SelfExecutionObserved != nil {
			h.SelfExecutionObserved(true)
		}
		h.fireStageEscalation(ctx, out, out.Result.FinalState, out.Err.Error())
		if h.Blocked != nil {
			if err := h.Blocked(ctx, terminalhooks.BlockedOutcome{RunID: out.RunID, RepoRef: h.RepoRef, Stage: out.Result.FinalState, ItemID: h.itemID(out), Reason: out.Err.Error()}); err != nil {
				h.recordFailure(out, out.Result.FinalState, "blocked_handling_failed", err)
			}
		}
		return
	}
	if out.Phase != journal.PhaseEscalated {
		return
	}
	env, ok := out.Result.Outputs[out.Result.FinalState]
	if !ok || env.Status != apiv1.ResultBlocked {
		return
	}
	stage := out.Result.FinalState
	if env.Error != nil && env.Error.Code == runsemantics.SelfExecutionDeniedCode && h.SelfExecutionObserved != nil {
		h.SelfExecutionObserved(true)
	}
	o := terminalhooks.BlockedOutcome{
		RunID:    out.RunID,
		RepoRef:  h.RepoRef,
		Stage:    stage,
		Reason:   terminalhooks.BlockedReason(env),
		Blockers: terminalhooks.ParseBlockedBy(env.Outputs),
	}
	if itemID := h.itemID(out); itemID != "" {
		o.ItemID = itemID
		// #2961: an item cannot block itself.
		if kept, dropped := terminalhooks.FilterSelfBlockers(o.Blockers, itemID); len(dropped) > 0 {
			o.Blockers = kept
			h.annotate(out, journal.Event{
				Type:   journal.EventRunnerAnnotation,
				Stage:  stage,
				Reason: "self-referential blockedBy dropped",
				Runner: map[string]any{
					"kind":              terminalhooks.SelfBlockerDroppedKind,
					"runID":             out.RunID,
					"itemID":            itemID,
					"droppedBlockers":   dropped,
					"remainingBlockers": len(kept),
					"driver":            string(journal.DriverEngine),
				},
			})
		}
	}
	// Escalation first, then the blocked record: the comment citing the stage's
	// reason lands before the item is parked.
	h.fireStageEscalation(ctx, out, stage, o.Reason)
	if h.Blocked == nil {
		return
	}
	if err := h.Blocked(ctx, o); err != nil {
		h.recordFailure(out, stage, "blocked_handling_failed", err)
	}
}

// fireStageEscalation posts one escalation comment per driving item, resolved
// from the claim ledger for a run that claimed its item mid-walk.
func (h *Hooks) fireStageEscalation(ctx context.Context, out Outcome, stage, reason string) {
	if h.Escalate == nil {
		return
	}
	itemIDs, err := h.terminalItemIDs(out)
	if err != nil {
		h.recordFailure(out, stage, "stage_terminal_item_resolution_failed", err)
		return
	}
	for _, itemID := range itemIDs {
		if err := h.Escalate(ctx, out, itemID, stage, reason); err != nil {
			h.recordFailure(out, stage, "stage_terminal_notification_failed", err)
		}
	}
}

// fireFailed mirrors the runner's #1054 failed terminal.
func (h *Hooks) fireFailed(ctx context.Context, out Outcome) {
	if h.Failed == nil || out.Phase != journal.PhaseFailed {
		return
	}
	cause, code := out.Result.FailureMessage, out.Result.FailureCode
	var faultClass telemetry.ErrorClass
	if out.Err != nil {
		// A walk-level failure returns (RunResult{}, err): the workflow's error IS
		// the cause. Preserve a typed infrastructure failure for the work-health
		// and failure-streak consumers.
		if cause == "" {
			cause = out.Err.Error()
		}
		if code == "" {
			code = FailureCode(out.Err)
		}
		faultClass = FaultClass(out.Err)
	}
	if err := h.Failed(ctx, terminalhooks.FailedOutcome{
		RunID: out.RunID, RepoRef: h.RepoRef, Stage: out.Result.FinalState,
		Cause: cause, Code: code, FaultClass: faultClass,
	}); err != nil {
		h.recordFailure(out, out.Result.FinalState, "failed_handling_failed", err)
	}
}

// WalkFailureCode is the failure code for an engine run that ended through a
// walk-level error rather than a stage's typed failure.
const WalkFailureCode = "run_failed"

// FailureCode classifies a workflow error. Error types survive the activity and
// workflow boundaries, so the engine's classifier keeps an exhausted
// infrastructure budget from becoming an item failure merely because RunResult
// is absent.
func FailureCode(err error) string {
	if engine.IsSelfExecutionDenied(err) {
		return runsemantics.SelfExecutionDeniedCode
	}
	if class, classifyErr := engine.ClassifyDispatchFailure(err); classifyErr == nil && class == journal.AttemptInfra {
		return telemetry.ErrCodeInfraFailure
	}
	return WalkFailureCode
}

// FaultClass is terminalhooks.FailedOutcome.FaultClass (#5638), from the same classifier as
// FailureCode, so the failure streak sees the same explicit infra class.
func FaultClass(err error) telemetry.ErrorClass {
	if class, classifyErr := engine.ClassifyDispatchFailure(err); classifyErr == nil && class == journal.AttemptInfra {
		return telemetry.ErrorClassInfra
	}
	return ""
}

// PhaseFor maps a finished run to its journal phase through engine.PhaseForStatus
// (the E2 correction, #3874: blocked projects to aborted and escalated to
// escalated). A run that reported no status ended through an error or
// cancellation and is failed; claims do not release themselves, so neither case
// may skip the frame.
func PhaseFor(res engine.RunResult) journal.RunPhase {
	phase, err := engine.PhaseForStatus(res.Status)
	if err != nil {
		return journal.PhaseFailed
	}
	return phase
}

// itemID resolves the run's single driving backlog item: the one pinned at
// start, else the first item the claim ledger records for this run.
func (h *Hooks) itemID(out Outcome) string {
	if out.Item != nil && out.Item.ID != "" {
		return out.Item.ID
	}
	ids, err := h.terminalItemIDs(out)
	if err != nil || len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (h *Hooks) terminalItemIDs(out Outcome) ([]string, error) {
	if out.Item != nil && out.Item.ID != "" {
		return []string{out.Item.ID}, nil
	}
	if h.ClaimedItems == nil {
		return nil, nil
	}
	return h.ClaimedItems(out.RunID)
}

// recordFailure journals one hook failure to the instance log: never fatal,
// because the run is already over and the remaining hooks must still release its
// claims.
func (h *Hooks) recordFailure(out Outcome, stage, code string, err error) {
	if err == nil {
		return
	}
	h.annotate(out, journal.Event{
		Type:   journal.EventError,
		Stage:  stage,
		Reason: "engine terminal hook failed",
		Error:  journal.ErrorDetailFor(code, err),
		Runner: map[string]any{"driver": string(journal.DriverEngine)},
	})
}

// annotate stamps the run identity onto an instance-log event: an instance-log
// event carries no implicit run scope, so an unstamped record is an orphan.
func (h *Hooks) annotate(out Outcome, ev journal.Event) {
	if h.Record == nil {
		return
	}
	ev.RunID, ev.Gaggle, ev.Workflow = out.RunID, out.Gaggle, out.Workflow
	if ev.Runner == nil {
		ev.Runner = map[string]any{}
	}
	if _, ok := ev.Runner["driver"]; !ok {
		ev.Runner["driver"] = string(journal.DriverEngine)
	}
	h.Record.AppendBestEffort(ev)
}
