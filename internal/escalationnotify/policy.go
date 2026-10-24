// Package escalationnotify is the instance-level notification policy for a
// run's non-completed outcomes (#3054): what the runner posts, labels and
// records on the driving work item when a stage reports blocked, a run ends
// failed, escalated or aborted, or an implement stage finds the fix already on
// main. It decides WHAT to write; the provider the writes go through and the
// instance state they read and update are handed in by the composition root.
package escalationnotify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/terminalhooks"
	"github.com/goobers/goobers/internal/terminalhooks/parking"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

// Item is one claimed work item paired with the repository identity recorded
// for it at selection time.
type Item struct {
	ItemID string
	Repo   providers.RepositoryRef
}

// Block is parking.Block.
type Block = parking.Block

// State is the instance state the policy reads and updates: the claim ledger,
// the blocked-record file, the failure-streak store, the circuit-breaker
// outbox and the sibling streaks it settles alongside. The composition root
// implements it over the instance layout.
type State interface {
	// ClaimedItemIDs, BacklogRepository and RecordBlock.
	parking.State
	// ClaimedItems returns runID's claimed items with each one's recorded
	// repository; it fails closed when any item's identity is unknown.
	ClaimedItems(runID string) ([]Item, error)
	// LoadFailureStreak returns an item's persisted failure-streak count.
	LoadFailureStreak(ctx context.Context, poster gate.Commenter, repo providers.RepositoryRef, itemID string) (int, error)
	// WriteFailureStreak persists an item's failure-streak count.
	WriteFailureStreak(repo providers.RepositoryRef, itemID string, count int, runID, stage string) error
	// LoadInfraFailureStreak returns an item's persisted infrastructure
	// failure-streak count (#5638), kept apart from the genuine/work streak.
	LoadInfraFailureStreak(ctx context.Context, repo providers.RepositoryRef, itemID string) (int, error)
	// WriteInfraFailureStreak persists an item's infrastructure
	// failure-streak count.
	WriteInfraFailureStreak(repo providers.RepositoryRef, itemID string, count int, runID, stage string) error
	// ReconcileParkOutbox retries circuit-breaker parks that previously
	// failed to reach the provider (#3646).
	ReconcileParkOutbox(ctx context.Context, poster gate.Commenter) error
	// RecordParkFailure persists a circuit-breaker park whose provider
	// mutation failed, so a later terminal retries it.
	RecordParkFailure(repo providers.RepositoryRef, itemID, runID, stage string, streak int, cause error) error
	// ClearParks drops any pending circuit-breaker park for an item.
	ClearParks(repo providers.RepositoryRef, itemID string) error
	// VoidRemediationCharge refunds a pr-remediation cycle this run charged
	// when the run ends on an infrastructure fault (#5588/#5598).
	VoidRemediationCharge(ctx context.Context, poster gate.Commenter, runID string) error
	// SettleSupersededRun recognises a completed run that ended because the
	// pull request it was working on moved under it (#6019) — a reviewer or
	// stage pod refusing to land the run's work on a head someone else
	// pushed, or a pr-claim guard reporting a stale selection — and refunds
	// the remediation charge that run recorded. superseded=true tells the
	// caller the run is evidence about neither the work nor the item, so the
	// failure and no-work streaks must be left exactly as they were; an error
	// means the run could not be classified, and the caller leaves them alone
	// too.
	SettleSupersededRun(ctx context.Context, poster gate.Commenter, runID, finalState string) (superseded bool, err error)
	// SettleNoWorkStreak advances or resets the repeated-no-work streak for a
	// completed run (#5379).
	SettleNoWorkStreak(ctx context.Context, poster gate.Commenter, runID, finalState, runURL string) error
	// RunURL is the portal link for runID used in failure comments.
	RunURL(runID string) (string, error)
}

// Policy applies the notification policy through Poster against State.
// RunsDir is the instance's runs directory, read for a run's durable identity
// when a provider write has to be attributed to it.
//
// EngineInterruption, when set, reports the watchdog interruption
// (runner.RunStalledErrorCode or runner.RunDurationExceededErrorCode) that
// cancelled an engine-driven run, or "" when none did or an operator also
// asked to cancel it. The Temporal driver can only journal a cancellation as
// run_canceled — CancelWorkflow carries no reason — so the stall sweep's own
// record of why it cancelled is the evidence (#5638).
type Policy struct {
	Poster             gate.Commenter
	RunsDir            string
	State              State
	EngineInterruption func(runID string) (string, error)
	// Reports durably retains provider projections without replaying charges.
	Reports *terminalreport.Queue
	Gaggle  string
}

// Blocked is runner.Config.Blocked (#544/#545/#552); the policy itself is
// parking.Blocked, shared with the cloud controller.
func (p *Policy) Blocked(ctx context.Context, o terminalhooks.BlockedOutcome) error {
	ctx = p.terminalHandlerAttributionContext(ctx, o.RunID, o.Stage)
	return parking.Blocked(ctx, p.Poster, p.State, o)
}

// Failed is runner.Config.Failed (#1054): the instance-level consequence of a
// run reaching terminal PhaseFailed. Leaves a human-visible trace on the
// driving item — a comment recording a stable failure code and the run id —
// so repeated terminal failures on the same item accumulate a countable signal
// instead of the item silently returning to goobers:ready with no record.
// Detailed causes remain in the local run trace because execution errors can
// contain harness argv, prompts, credentials, environment values, or context.
//
// Circuit breaker: after FailureStreakThreshold consecutive terminal failures
// on the same item, applies goobers:needs-human and removes goobers:ready so
// the retry loop stops. The threshold is counted via a single editable
// failure-streak comment on the issue (one comment, updated in place, instead
// of one per run). A failed terminal never counts on the separate
// infrastructure streak: that streak is for item-correlated infrastructure,
// which only an escalated or aborted terminal's journal can show (#5638).
//
// Like Blocked, the handler runs before FinalizeTerminal releases the run's
// claims, so it resolves the driving item(s) from the claim ledger by run id.
// Best-effort per item: one item's provider failure doesn't skip the rest; the
// joined error is journaled by the runner (failed_handling_failed), never
// fatal to the terminal transition.
func (p *Policy) Failed(ctx context.Context, o terminalhooks.FailedOutcome) error {
	ctx = p.terminalHandlerAttributionContext(ctx, o.RunID, o.Stage)
	// #3361/#3364: an infra-fault terminal (credential materialization, git,
	// network, lock contention, a harness that never gave the agent a turn) is
	// weather, not evidence about the item — it must not accumulate strikes
	// that eventually park the item goobers:needs-human, on either streak.
	// The item returns to the pool untouched and the scheduler's auth circuit
	// / quota gates own the retry cadence. Item-judgment terminals (a
	// verified ISSUE_NOT_APPLICABLE refusal, #3363) are likewise not charged.
	// Timeout still counts as work: a recurring harness session timeout is
	// this circuit breaker's motivating case (#1054), and only the runner's
	// explicit infrastructure classification (FaultClass, #5638) moves a
	// terminal off the work streak.
	//
	// #4417: o.RepoRef is the run's dispatch-time gaggle project, not
	// necessarily the repo the item this run actually claimed belongs
	// to — the breaker resolves each claimed item's own recorded identity
	// instead of trusting a single repo for all of them.
	runURL, _ := p.State.RunURL(o.RunID)
	return p.chargeFailed(ctx, o.RunID, o.Stage, runURL, failedOutcomeClass(o))
}

// ExistingFix is runner.Config.ExistingFix (#3236); see parking.ExistingFix.
func (p *Policy) ExistingFix(ctx context.Context, o terminalhooks.ExistingFixOutcome) error {
	return parking.ExistingFix(ctx, p.Poster, o)
}

// TerminalNotifier wraps inner with circuit breaker logic for PhaseEscalated
// and PhaseAborted, and the streak resets for PhaseCompleted. PhaseFailed is
// handled by Failed (which applies the circuit breaker directly), so this
// wrapper skips PhaseFailed to avoid double-counting.
//
// Each claimed item is routed to its OWN recorded repository identity (#4417)
// rather than one repo computed from the gaggle's static project: a
// mid-run-claimed item's actual repo is not always that project, and #4417's
// own incident was a terminal circuit-breaker path routing to a completely
// unrelated repository because it reconstructed ownership from exactly that
// kind of instance-level default.
//
// Breaker errors are returned, not discarded (#3646): the runner journals a
// TerminalNotifier failure as terminal_notification_failed, so a park that did
// not reach the provider — or a claimed item with no recorded repository
// identity — leaves an actionable diagnostic in the run trace alongside the
// durable outbox entry.
func (p *Policy) TerminalNotifier(inner terminalhooks.TerminalNotifier) terminalhooks.TerminalNotifier {
	return func(runID string, phase journal.RunPhase, finalState string) error {
		var errs []error
		if phase == journal.PhaseCompleted || phase == journal.PhaseEscalated || phase == journal.PhaseAborted {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			attributedCtx, _ := AttributionContextForRun(ctx, p.RunsDir, runID, finalState)
			runURL, _ := p.State.RunURL(runID)
			if phase == journal.PhaseCompleted {
				errs = append(errs, p.settleCompleted(attributedCtx, runID, finalState, runURL)...)
			} else if err := p.chargeTerminal(attributedCtx, runID, finalState, runURL); err != nil {
				errs = append(errs, fmt.Errorf("apply circuit breaker for run %q: %w", runID, err))
			}
		}
		if inner != nil {
			if err := inner(runID, phase, finalState); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// settleCompleted is TerminalNotifier's completed-run arm. A superseded run
// (#6019) neither resets the failure streak nor touches the no-work streak:
// its subject moved under it, so it says nothing about whether the work on
// that item succeeds — crediting it would erase real strikes, and counting it
// as no-work would walk the item toward a park it did nothing to earn. A run
// the check could not classify fails closed the same way: its streaks are
// left alone and the error is reported, never an ordinary completion's reset.
func (p *Policy) settleCompleted(ctx context.Context, runID, finalState, runURL string) []error {
	var errs []error
	superseded, err := p.State.SettleSupersededRun(ctx, p.Poster, runID, finalState)
	if err != nil {
		return append(errs, fmt.Errorf("settle superseded run %q: %w", runID, err))
	}
	if superseded {
		return errs
	}
	if err := p.resetCircuitBreaker(ctx, runID, runURL); err != nil {
		errs = append(errs, fmt.Errorf("reset circuit breaker for run %q: %w", runID, err))
	}
	if err := p.State.SettleNoWorkStreak(ctx, p.Poster, runID, finalState, runURL); err != nil {
		errs = append(errs, fmt.Errorf("settle no-work streak for run %q: %w", runID, err))
	}
	return errs
}

// chargeTerminal is the escalated/aborted arm of the breaker. Like Failed, it
// honors the classification the run already carries (#5638), which before
// this was not consulted at all, so a local-ci timeout the gate had judged
// infra still posted a genuine/work-failure comment when the run then expired.
//
//   - Item infrastructure — a gate that exhausted its infrastructure repass
//     budget, or a watchdog expiry or stall that interrupted the gate's
//     infrastructure retry of the item's own validation — is charged to the
//     separate infrastructure streak, never to the genuine/work streak.
//   - Substrate infrastructure — an expiry or stall during a dispatch or
//     evaluator infrastructure retry, or an engine workflow the daemon had to
//     terminate — is not about the item and charges neither streak.
//
// A journal that cannot be read leaves the terminal unclassified and charged
// as work, as before; a read failure other than absence is reported with the
// breaker's errors.
func (p *Policy) chargeTerminal(ctx context.Context, runID, finalState, runURL string) error {
	fault, readErr := p.journaledTerminalFault(runID)
	var err error
	switch fault {
	case terminalhooks.TerminalFaultItemInfrastructure:
		err = errors.Join(
			p.State.VoidRemediationCharge(ctx, p.Poster, runID),
			p.applyInfraCircuitBreaker(ctx, runID, finalState, runURL),
		)
	case terminalhooks.TerminalFaultSubstrate:
		err = p.State.VoidRemediationCharge(ctx, p.Poster, runID)
	default:
		err = p.applyCircuitBreaker(ctx, runID, finalState, runURL)
	}
	return errors.Join(readErr, err)
}

func (p *Policy) journaledTerminalFault(runID string) (terminalhooks.TerminalFault, error) {
	reader, err := journal.OpenReadOnly(filepath.Join(p.RunsDir, runID))
	var events []journal.Event
	if err == nil {
		events, err = reader.Events()
	}
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		return "", nil
	default:
		return "", fmt.Errorf("read terminal classification for run %q: %w", runID, err)
	}
	var interruption string
	// Only a cancelled run can have been stopped by the engine stall sweep, so
	// only it consults the sweep's record: a local terminal never depends on
	// the instance log being readable.
	if p.EngineInterruption != nil && terminalhooks.EndedByCancellation(events) {
		var ierr error
		if interruption, ierr = p.EngineInterruption(runID); ierr != nil {
			// Without the sweep's record a cancelled engine run stays an
			// operator cancel: charged as work, never guessed to be infra.
			err = fmt.Errorf("read engine interruption for run %q: %w", runID, ierr)
		}
	}
	return terminalhooks.JournaledTerminalFault(events, interruption), err
}

// chargeFailed charges a failed terminal by its class:
//
//   - item judgment (#3363) is a correct conclusion about the item and is not
//     charged at all;
//   - an infrastructure fault on a failed terminal (#3361, #5638) — the
//     credential, git, network or lock substrate, a dispatch that exhausted
//     its infrastructure budget, a harness that never gave the agent a turn —
//     is not about the item, so it charges neither streak. It refunds a
//     pr-remediation cycle this run charged but never had evaluated
//     (#5588/#5598). This is #5638's original complaint: a harness bug must
//     never park good issues;
//   - everything else is a genuine/work terminal. A bare session timeout is
//     one: only an explicit infrastructure classification moves a timeout
//     off the work streak, so a recurring harness timeout still parks at the
//     work threshold (#1054).
func (p *Policy) chargeFailed(ctx context.Context, runID, stage, runURL string, class telemetry.ErrorClass) error {
	switch {
	case class == telemetry.ErrorClassItemJudgment:
		return nil
	case class.InfraFault():
		return p.State.VoidRemediationCharge(ctx, p.Poster, runID)
	default:
		return p.applyCircuitBreaker(ctx, runID, stage, runURL)
	}
}

// failedOutcomeClass is the terminal's class: the runner's explicit
// FaultClass when it set one, else the class of its code.
func failedOutcomeClass(o terminalhooks.FailedOutcome) telemetry.ErrorClass {
	if o.FaultClass != "" {
		return o.FaultClass
	}
	return telemetry.ClassifyError(o.Code)
}
