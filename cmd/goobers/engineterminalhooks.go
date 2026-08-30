package main

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

// engineTerminalHooks is the daemon's terminal-hook frame for engine-driven
// runs: the seven instance-level side effects a run's terminal is supposed to
// have, fired in the local runner's order, from the engine.RunResult the
// workflow returned.
//
// # Why this exists
//
// Every one of these hooks is instance-level policy that lives in the
// composition root, not in the walk: releasing the claim ledger's lease,
// retiring the provider-visible claim marker (#3347), deleting the run branch,
// labeling an aborted run's PR, parking a blocked item on goobers:needs-human,
// leaving a failure trace (#1054), stripping labels when the fix already
// existed (#3236), and updating the failure-streak circuit breaker. The engine
// walks stages on a Temporal worker and cannot perform any of them: they need
// the daemon's credential stores, its claim ledger and its instance log.
//
// Before decision 005 D1 that was invisible, because nothing but `goobers
// engine-start` ever started an engine run and nothing waited for one. The
// moment the scheduler can dispatch to the engine, a run that ends without
// this frame leaks its claims until the lease expires, never records its
// failure streak, and leaves its branch and its PR behind — silently, because
// the run's own journal says `completed`.
//
// # Why one value, derived from buildRunnerConfig
//
// The hooks are NOT re-derived here. They are the same closures
// buildRunnerConfig built for this gaggle's local runner, captured in one
// value at one call site. Rebuilding them would mean a second set of
// credential resolutions, a second claim-marker repo decision and a second
// circuit-breaker instance — and the two would drift on the next change to
// either, which is precisely how a "parity" path stops being parity.
type engineTerminalHooks struct {
	layout  instance.Layout
	log     *journal.InstanceLog
	repoRef apiv1.RepoRef

	existingFix  runner.ExistingFixHandler
	blocked      runner.BlockedHandler
	failed       runner.FailedHandler
	escalation   *gate.EscalationNotifier
	claimedItems func(runID string) ([]string, error)
	prepare      terminalBranchPreparer
	notify       runner.TerminalNotifier
	finalize     runner.TerminalFinalizer
}

// engineTerminalOutcome is one finished engine run, as the frame sees it.
type engineTerminalOutcome struct {
	// RunID, Gaggle and Workflow identify the run for the instance log.
	RunID    string
	Gaggle   string
	Workflow string
	// Phase is the journal phase the run reached — engine.PhaseForStatus of
	// the workflow's status, NEVER a re-derivation from the status word.
	Phase journal.RunPhase
	// Result is the workflow's own return value. Zero when the run ended
	// through an error rather than a status (Err is then non-nil).
	Result engine.RunResult
	// Item is the driving backlog item, when the run had one at start.
	Item *apiv1.BacklogItem
	// Err is the workflow's terminal error, if it ended by failing rather
	// than by returning.
	Err error
}

// run fires the terminal hooks for one finished engine run, in the local
// runner's order.
//
// The order is load-bearing and is the runner's, not a convenient one:
//
//  1. ExistingFix (#3236) — before the terminal, because it strips the labels
//     that would otherwise let the item be reclaimed between here and the
//     claim release.
//  2. Escalation, then Blocked (#544/#545) — the escalation comment cites the
//     stage's stated reason, and the blocked record parks the item; both run
//     while the claim ledger still holds this run's claims, because both
//     resolve the driving item FROM those claims for a run that claimed its
//     item mid-walk.
//  3. Failed (#1054) — same claim-lifetime requirement, different terminal.
//  4. PrepareTerminal — branch cleanup and the aborted-run PR label.
//  5. NotifyTerminal — the circuit breaker, then the terminal notification.
//  6. FinalizeTerminal — releases the claim ledger lease and the provider
//     claim marker. LAST, because everything above needs the claims.
//
// Nothing here appends to the RUN's journal: for an engine run that journal
// is the workflow's, it already carries run.finished, and appending after it
// would be reported as a live-journal divergence (see terminalAnnotator).
// Handler failures are journaled to the INSTANCE log instead — the daemon's
// record of what the daemon did — and never abort the remaining hooks. A
// blocked-notification failure that skipped FinalizeTerminal would leak the
// very claims this frame exists to release.
func (h *engineTerminalHooks) run(ctx context.Context, out engineTerminalOutcome) {
	if h == nil {
		return
	}
	h.fireExistingFix(ctx, out)
	h.fireBlocked(ctx, out)
	h.fireFailed(ctx, out)

	if h.prepare != nil {
		if err := h.prepare(out.RunID, out.Phase, h.annotator(out)); err != nil {
			h.recordHookFailure(out, "", "engine_terminal_prepare_failed", err)
		}
	}
	if h.notify != nil {
		// Errors are ignored exactly as the runner ignores them
		// (TerminalNotifier's contract: "notification delivery can never
		// affect run processing"), but recorded so an operator can see a
		// breaker that is not updating.
		if err := h.notify(out.RunID, out.Phase, out.Result.FinalState); err != nil {
			h.recordHookFailure(out, "", "engine_terminal_notification_failed", err)
		}
	}
	if h.finalize != nil {
		if err := h.finalize(out.RunID, out.Phase); err != nil {
			h.recordHookFailure(out, "", "engine_terminal_finalize_failed", err)
		}
	}
}

// fireExistingFix mirrors internal/runner's #3236 arm: an implement stage that
// completed with no-work because the fix already exists on main, identified by
// the existingFixCommit output it declared.
//
// The engine's RunResult carries the upstream envelope map keyed by stage, so
// the same three facts the runner reads off the live result — the stage is
// "implement", its status is no-work, and it declared a non-empty commit —
// are read off Outputs["implement"] here.
func (h *engineTerminalHooks) fireExistingFix(ctx context.Context, out engineTerminalOutcome) {
	if h.existingFix == nil || out.Phase != journal.PhaseCompleted {
		return
	}
	env, ok := out.Result.Outputs[existingFixStage]
	if !ok || env.Status != apiv1.ResultNoWork {
		return
	}
	commit, _ := env.Outputs["existingFixCommit"].(string)
	if commit == "" {
		return
	}
	itemID := h.itemID(out)
	if err := h.existingFix(ctx, runner.ExistingFixOutcome{
		RunID:   out.RunID,
		ItemID:  itemID,
		RepoRef: h.repoRef,
		Commit:  commit,
	}); err != nil {
		h.recordHookFailure(out, existingFixStage, "existingfix_handling_failed", err)
	}
}

// existingFixStage is the one stage name the #3236 arm applies to, matching
// internal/runner's own literal.
const existingFixStage = "implement"

// fireBlocked mirrors internal/runner's #544 blocked terminal.
//
// The trigger is deliberately NOT the phase alone. A stage reporting
// apiv1.ResultBlocked ends the engine walk at StatusEscalated (taskOutcome's
// blocked arm — a schema-valid producer value is never punished as a
// failure), and so does a @escalate routing target; both project to
// journal.PhaseEscalated. Only the first is a "blocked" outcome with a
// reason and blockers to record, and the two are told apart exactly as the
// runner tells them apart: by the FINAL STAGE'S OWN reported status.
func (h *engineTerminalHooks) fireBlocked(ctx context.Context, out engineTerminalOutcome) {
	if out.Phase != journal.PhaseEscalated {
		return
	}
	env, ok := out.Result.Outputs[out.Result.FinalState]
	if !ok || env.Status != apiv1.ResultBlocked {
		return
	}
	stage := out.Result.FinalState
	o := runner.BlockedOutcome{
		RunID:    out.RunID,
		RepoRef:  h.repoRef,
		Stage:    stage,
		Reason:   runner.BlockedReason(env),
		Blockers: runner.ParseBlockedBy(env.Outputs),
	}
	itemID := h.itemID(out)
	if itemID != "" {
		o.ItemID = itemID
		// #2961: an item cannot block itself.
		if kept, dropped := runner.FilterSelfBlockers(o.Blockers, itemID); len(dropped) > 0 {
			o.Blockers = kept
			h.annotate(out, journal.Event{
				Type:   journal.EventRunnerAnnotation,
				Stage:  stage,
				Reason: "self-referential blockedBy dropped",
				Runner: map[string]any{
					"kind":              runner.SelfBlockerDroppedKind,
					"runID":             out.RunID,
					"itemID":            itemID,
					"droppedBlockers":   dropped,
					"remainingBlockers": len(kept),
					"driver":            string(journal.DriverEngine),
				},
			})
		}
	}
	// Escalation first, then the blocked record — the runner's order
	// (notifyBlockedEscalation precedes notifyBlocked), because the comment
	// citing the stage's reason should land before the item is parked.
	h.fireStageEscalation(ctx, out, stage, o.Reason)
	if h.blocked == nil {
		return
	}
	if err := h.blocked(ctx, o); err != nil {
		h.recordHookFailure(out, stage, "blocked_handling_failed", err)
	}
}

// fireStageEscalation mirrors internal/runner's notifyStageEscalation: one
// escalation comment per driving item, resolved from the claim ledger for a
// run that claimed its item mid-walk.
func (h *engineTerminalHooks) fireStageEscalation(ctx context.Context, out engineTerminalOutcome, stage, reason string) {
	if h.escalation == nil {
		return
	}
	itemIDs, err := h.terminalItemIDs(out)
	if err != nil {
		h.recordHookFailure(out, stage, "stage_terminal_item_resolution_failed", err)
		return
	}
	for _, itemID := range itemIDs {
		// Seq 0: the run's journal is the workflow's and this daemon did not
		// write the terminal event, so there is no local sequence number to
		// cite. The notifier uses it only for idempotency keying within a
		// run, and an engine run's terminal notification happens once.
		if err := h.escalation.NotifyStageEscalated(
			ctx, engineTerminalRepositoryRef(h.repoRef), itemID, out.RunID, 0, stage, reason,
		); err != nil {
			h.recordHookFailure(out, stage, "stage_terminal_notification_failed", err)
		}
	}
}

// fireFailed mirrors internal/runner's #1054 failed terminal.
func (h *engineTerminalHooks) fireFailed(ctx context.Context, out engineTerminalOutcome) {
	if h.failed == nil || out.Phase != journal.PhaseFailed {
		return
	}
	cause := out.Result.FailureMessage
	code := out.Result.FailureCode
	if out.Err != nil {
		// A walk-level failure returns (RunResult{}, err): there is no status,
		// no final state and no failure code, so the workflow's error IS the
		// cause and the code falls back to the runner's own default for a
		// failure no stage claimed.
		if cause == "" {
			cause = out.Err.Error()
		}
		if code == "" {
			code = engineWalkFailureCode
		}
	}
	if err := h.failed(ctx, runner.FailedOutcome{
		RunID:   out.RunID,
		RepoRef: h.repoRef,
		Stage:   out.Result.FinalState,
		Cause:   cause,
		Code:    code,
	}); err != nil {
		h.recordHookFailure(out, out.Result.FinalState, "failed_handling_failed", err)
	}
}

// engineWalkFailureCode is the failure code for an engine run that ended
// through a walk-level error rather than a stage's typed failure — the
// engine's analogue of the runner's bare "run_failed".
const engineWalkFailureCode = "run_failed"

// itemID resolves the run's single driving backlog item: the one pinned at
// start when the trigger carried one, else the first item the claim ledger
// records for this run (the schedule/backlog-item-triggered shape, where the
// run claims its item mid-walk).
func (h *engineTerminalHooks) itemID(out engineTerminalOutcome) string {
	if out.Item != nil && out.Item.ID != "" {
		return out.Item.ID
	}
	ids, err := h.terminalItemIDs(out)
	if err != nil || len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// terminalItemIDs resolves every backlog item this run currently claims,
// mirroring internal/runner's terminalGateItemIDs.
func (h *engineTerminalHooks) terminalItemIDs(out engineTerminalOutcome) ([]string, error) {
	if out.Item != nil && out.Item.ID != "" {
		return []string{out.Item.ID}, nil
	}
	if h.claimedItems == nil {
		return nil, nil
	}
	return h.claimedItems(out.RunID)
}

// annotator returns the sink this run's terminal cleanup records to. See
// terminalAnnotator: for an engine run it is the instance log, because the
// run's own journal is closed and normatively complete.
func (h *engineTerminalHooks) annotator(out engineTerminalOutcome) terminalAnnotator {
	return &engineInstanceAnnotator{log: h.log, out: out}
}

func (h *engineTerminalHooks) annotate(out engineTerminalOutcome, ev journal.Event) {
	_ = h.annotator(out).Append(ev)
}

// recordHookFailure journals one terminal-hook failure to the instance log.
// It is the engine path's replacement for the runner's in-journal error
// append: same code vocabulary, different sink, and — unlike the runner,
// which fails the run terminal when it cannot journal — never fatal, because
// the run is already over and the remaining hooks still have to release its
// claims.
func (h *engineTerminalHooks) recordHookFailure(out engineTerminalOutcome, stage, code string, err error) {
	if err == nil {
		return
	}
	h.annotate(out, journal.Event{
		Type:   journal.EventError,
		Stage:  stage,
		Reason: "engine terminal hook failed",
		Error:  &journal.ErrorDetail{Code: code, Message: err.Error()},
		Runner: map[string]any{"driver": string(journal.DriverEngine)},
	})
}

// engineInstanceAnnotator adapts the instance log to terminalAnnotator,
// stamping the run identity onto every event.
//
// The stamp is required, not cosmetic: an instance-log event carries no
// implicit run scope the way a run-journal append does, so an un-stamped
// ref.touched would be an orphan record of a branch deletion nobody can
// attribute.
type engineInstanceAnnotator struct {
	log *journal.InstanceLog
	out engineTerminalOutcome
}

func (a *engineInstanceAnnotator) Append(ev journal.Event) error {
	if a == nil || a.log == nil {
		return nil
	}
	ev.RunID = a.out.RunID
	ev.Gaggle = a.out.Gaggle
	ev.Workflow = a.out.Workflow
	if ev.Runner == nil {
		ev.Runner = map[string]any{}
	}
	if _, ok := ev.Runner["driver"]; !ok {
		ev.Runner["driver"] = string(journal.DriverEngine)
	}
	return a.log.Append(ev)
}

// engineTerminalPhase maps a finished engine run to its journal phase.
//
// It delegates to engine.PhaseForStatus rather than switching on the status
// word here — the E2 correction (#3874): blocked projects to PhaseAborted and
// escalated to PhaseEscalated, which is NOT the mapping a reader guesses from
// the names, and a second switch in the daemon would eventually disagree with
// the one the run's own journal used. A run that ended through an error and
// reported no status is PhaseFailed.
func engineTerminalPhaseFor(res engine.RunResult, _ error) journal.RunPhase {
	phase, err := engine.PhaseForStatus(res.Status)
	if err != nil {
		// No status at all is the walk-level error/cancellation shape
		// (engine.run returns (RunResult{}, err)); an unrecognized one is a
		// vocabulary the daemon is older than. Both are failures the daemon
		// must still fire the terminal frame for — claims do not release
		// themselves — so neither is allowed to skip it.
		return journal.PhaseFailed
	}
	return phase
}

// engineTerminalRepositoryRef mirrors internal/runner's providerRepositoryRef:
// the escalation notifier speaks the providers vocabulary, the run's pinned
// RepoRef speaks the API one.
func engineTerminalRepositoryRef(repo apiv1.RepoRef) providers.RepositoryRef {
	return providers.RepositoryRef{
		Provider: providers.ProviderKind(repo.Provider),
		Owner:    repo.Owner,
		Name:     repo.Name,
	}
}

// engineStartResult maps a finished engine run to the scheduler's
// StartResult, preserving NoWork.
//
// NoWork is the field that would be silently lost: it is the scheduler's
// idle-backoff signal (recordScheduledPollResult consumes it), it is
// `omitempty` on the wire, and a run that found nothing to do looks exactly
// like a successful one without it. An engine lane that dropped it would
// re-tick a genuinely empty backlog at full schedule rate forever, burning
// provider quota — the reason E2 (#3874) plumbed NoWork through RunResult in
// the first place.
func engineStartResult(res engine.RunResult, phase journal.RunPhase, err error) localscheduler.StartResult {
	out := localscheduler.StartResult{
		Phase:          phase,
		FinalState:     res.FinalState,
		NoWork:         res.NoWork,
		FailureStage:   res.FinalState,
		FailureCode:    res.FailureCode,
		FailureMessage: res.FailureMessage,
	}
	if phase != journal.PhaseFailed {
		out.FailureStage = ""
		out.FailureCode = ""
		out.FailureMessage = ""
		return out
	}
	if out.FailureCode == "" {
		out.FailureCode = engineWalkFailureCode
	}
	if out.FailureMessage == "" && err != nil {
		out.FailureMessage = err.Error()
	}
	return out
}

// errEngineTerminalHooksUnavailable is what an engine dispatch reports when
// the daemon could not derive the terminal-hook frame for its gaggle.
var errEngineTerminalHooksUnavailable = errors.New("engine terminal hooks are not wired for this gaggle")

func (h *engineTerminalHooks) validate(gaggle string) error {
	if h == nil {
		return fmt.Errorf("gaggle %q: %w", gaggle, errEngineTerminalHooksUnavailable)
	}
	return nil
}
