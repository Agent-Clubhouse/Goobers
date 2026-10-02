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
	"time"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// Item is one claimed work item paired with the repository identity recorded
// for it at selection time.
type Item struct {
	ItemID string
	Repo   providers.RepositoryRef
}

// Block is one learned dependency block the blocked handler records so
// backlog selection can skip the item until its blockers close (#552). State
// stamps the record's time when it persists it.
type Block struct {
	Repository providers.RepositoryRef
	ItemID     string
	Blockers   []string
	RunID      string
	Stage      string
	Reason     string
}

// State is the instance state the policy reads and updates: the claim ledger,
// the blocked-record file, the failure-streak store, the circuit-breaker
// outbox and the sibling streaks it settles alongside. The composition root
// implements it over the instance layout.
type State interface {
	// ClaimedItemIDs returns the item ids runID currently claims.
	ClaimedItemIDs(runID string) ([]string, error)
	// ClaimedItems returns runID's claimed items with each one's recorded
	// repository; it fails closed when any item's identity is unknown.
	ClaimedItems(runID string) ([]Item, error)
	// BacklogRepository maps a run's code repository to the backlog
	// repository its work items live in.
	BacklogRepository(repo providers.RepositoryRef) providers.RepositoryRef
	// RecordBlock persists b and returns the dependency cycle it closes, if
	// any. The cycle is returned even when persisting fails after detection.
	RecordBlock(b Block) (blockedcycle.Result, error)
	// LoadFailureStreak returns an item's persisted failure-streak count.
	LoadFailureStreak(ctx context.Context, poster gate.Commenter, repo providers.RepositoryRef, itemID string) (int, error)
	// WriteFailureStreak persists an item's failure-streak count.
	WriteFailureStreak(repo providers.RepositoryRef, itemID string, count int, runID, stage string) error
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
	// SettleNoWorkStreak advances or resets the repeated-no-work streak for a
	// completed run (#5379).
	SettleNoWorkStreak(ctx context.Context, poster gate.Commenter, runID, finalState, runURL string) error
	// RunURL is the portal link for runID used in failure comments.
	RunURL(runID string) (string, error)
}

// Policy applies the notification policy through Poster against State.
// RunsDir is the instance's runs directory, read for a run's durable identity
// when a provider write has to be attributed to it.
type Policy struct {
	Poster  gate.Commenter
	RunsDir string
	State   State
}

// Blocked is runner.Config.Blocked (#544/#545/#552): the instance-level
// consequences of a stage reporting status "blocked".
// Every blocked driving issue is parked (swap off goobers:ready and the
// provider-visible claim marker) per the #544 ruling / #539 convention. This
// prevents the released claim from making the same item immediately eligible
// again.
//
// The park label depends on whether the stage named a blocker (#2028): a
// named, non-cyclic blocker is goobers:blocked-on-sibling — a self-healing
// dependency park, not a decision only a human can make; the record below is
// what actually self-heals it, the label just needs to say so. An
// unattributed block (no blocker named) or a detected circular dependency is
// goobers:needs-human — the runner can't resolve either on its own, so it
// genuinely is a human decision.
//
// When the stage also references blockers through outputs.blockedBy, record
// them so #552's selection guard still protects the issue if a human
// re-promotes it before every dependency closes. Blockers naming the driving
// item itself are dropped first (#2961) — an item cannot depend on itself,
// and persisting that self-edge makes cycle detection report a one-node cycle
// and park the issue needs-human over a dependency that does not exist. If a
// new record closes a real cycle, every issue in that cycle is parked
// goobers:needs-human and receives a cycle-specific comment for human
// resolution. The runner's shared EscalationNotifier owns the normal
// explanatory provider comment.
//
// The handler runs before FinalizeTerminal releases the run's claims, so a
// run with no StartInput.Item (scheduled/fan-out implementation runs claim
// their item mid-run) resolves its driving item(s) from the claim ledger by
// run id. Best-effort per item: one item's provider failure doesn't skip the
// rest; the joined error is journaled by the runner (blocked_handling_failed),
// never fatal to the terminal transition.
func (p *Policy) Blocked(ctx context.Context, o runner.BlockedOutcome) error {
	ctx = p.terminalHandlerAttributionContext(ctx, o.RunID, o.Stage)
	itemIDs := []string{o.ItemID}
	if o.ItemID == "" {
		ids, err := p.State.ClaimedItemIDs(o.RunID)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			// No driving item anywhere (a producer run) — nothing to
			// record or park; the journaled blocked_by_agent cause and the
			// escalated phase are the whole story.
			return nil
		}
		itemIDs = ids
	}

	var errs []error
	repoRef := providers.RepositoryRef{
		Provider: providers.ProviderKind(o.RepoRef.Provider),
		Owner:    o.RepoRef.Owner,
		Name:     o.RepoRef.Name,
	}
	if blockedcycle.RepositoryEmpty(repoRef) {
		return fmt.Errorf("blocked outcome for run %s has no repository", o.RunID)
	}
	// Scope blocked records to the backlog project, not the code repo.
	// Work items live in the gaggle's backlog project (e.g. "Example Backlog"), which
	// is a different ADO project than the code repo ("Example Service").
	// The selection guard evaluates records against the backlog repo, so
	// records must be keyed/stored under the backlog repo or a parked parent
	// is never skipped and gets re-claimed. Idempotent for GitHub (backlog ==
	// code repo) and re-applied safely by the poster before the work-item call.
	repoRef = p.State.BacklogRepository(repoRef)
	for _, itemID := range itemIDs {
		// #2961: an item can never be its own blocker. The runner already
		// drops the self-reference when the run carried its driving item,
		// but a run that claims its item(s) mid-run resolves them here, so
		// the same guard has to apply per item — otherwise a self-edge
		// reaches the blocked records and cycle detection parks the issue
		// needs-human for a dependency cycle that does not exist.
		blockers, _ := runner.FilterSelfBlockers(o.Blockers, itemID)
		// #2028: a named blocker is a self-healing dependency park
		// (blocked-on-sibling), not a human decision; only an
		// unattributed block stays needs-human. A detected cycle
		// overrides this below with its own needs-human cycleReq.
		// A block whose only named blocker was the item itself is
		// unattributed once filtered, so it correctly stays needs-human.
		label := providers.LabelNeedsHuman
		if len(blockers) > 0 {
			label = providers.LabelBlockedOnSibling
		}
		req := providers.UpdateWorkItemRequest{
			Repository:   repoRef,
			ID:           itemID,
			AddLabels:    []string{label},
			RemoveLabels: []string{providers.LabelReady, providers.LabelClaimed},
		}
		if len(blockers) > 0 {
			cycle, err := p.State.RecordBlock(Block{
				Repository: repoRef,
				ItemID:     itemID,
				Blockers:   blockers,
				RunID:      o.RunID,
				Stage:      o.Stage,
				Reason:     o.Reason,
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("record block for %s: %w", itemID, err))
			}
			if len(cycle.Affected) > 0 {
				comments := blockedcycle.Comments(cycle)
				for _, cycleItem := range cycle.Affected {
					for _, comment := range comments {
						cycleReq := providers.UpdateWorkItemRequest{
							Repository:   cycleItem.Repository,
							ID:           cycleItem.ItemID,
							Comment:      comment,
							AddLabels:    []string{providers.LabelNeedsHuman},
							RemoveLabels: []string{providers.LabelReady, providers.LabelClaimed},
						}
						if _, err := p.Poster.UpdateWorkItem(ctx, cycleReq); err != nil {
							errs = append(errs, fmt.Errorf("escalate circular dependency on %s#%s: %w", cycleItem.Repository.Name, cycleItem.ItemID, err))
						}
					}
				}
				continue
			}
		}
		if _, err := p.Poster.UpdateWorkItem(ctx, req); err != nil {
			errs = append(errs, fmt.Errorf("park blocked item %s#%s: %w", repoRef.Name, itemID, err))
		}
	}
	return errors.Join(errs...)
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
// of one per run).
//
// Like Blocked, the handler runs before FinalizeTerminal releases the run's
// claims, so it resolves the driving item(s) from the claim ledger by run id.
// Best-effort per item: one item's provider failure doesn't skip the rest; the
// joined error is journaled by the runner (failed_handling_failed), never
// fatal to the terminal transition.
func (p *Policy) Failed(ctx context.Context, o runner.FailedOutcome) error {
	ctx = p.terminalHandlerAttributionContext(ctx, o.RunID, o.Stage)
	// #3361/#3364: an infra-fault terminal (credential materialization, git,
	// network, lock contention) is weather, not evidence about the item —
	// it must not accumulate failure-streak strikes that eventually park
	// the item goobers:needs-human. The item returns to the pool untouched
	// and the scheduler's auth circuit / quota gates own the retry cadence.
	// Item-judgment terminals (a verified ISSUE_NOT_APPLICABLE refusal,
	// #3363) are likewise not work failures. Timeout deliberately still
	// counts: a recurring harness session timeout is this circuit
	// breaker's motivating case (#1054).
	if failureStreakExempt(o) {
		// #5588/#5598: a pr-remediation cycle this run already charged
		// never had its fix evaluated. Mark it so the next checkpoint
		// refunds the charge instead of escalating the PR.
		if failedOutcomeClass(o).InfraFault() {
			return p.State.VoidRemediationCharge(ctx, p.Poster, o.RunID)
		}
		return nil
	}
	// #4417: o.RepoRef is the run's dispatch-time gaggle project, not
	// necessarily the repo the item this run actually claimed belongs
	// to — applyCircuitBreaker resolves each claimed item's own
	// recorded identity instead of trusting a single repo for all of
	// them.
	runURL, _ := p.State.RunURL(o.RunID)
	return p.applyCircuitBreaker(ctx, o.RunID, o.Stage, runURL)
}

// ExistingFix is runner.Config.ExistingFix (#3236): strips goobers:ready and
// goobers:critical labels from an issue when the implement stage returns
// no-work with existingFixCommit set, preventing a permanent reclaim loop when
// the fix is already on main.
func (p *Policy) ExistingFix(ctx context.Context, o runner.ExistingFixOutcome) error {
	if o.ItemID == "" {
		return nil
	}
	repoRef := providers.RepositoryRef{
		Provider: providers.ProviderKind(o.RepoRef.Provider),
		Owner:    o.RepoRef.Owner,
		Name:     o.RepoRef.Name,
	}
	// Strip both goobers:ready and goobers:critical labels to prevent reclaim
	_, err := p.Poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository:   repoRef,
		ID:           o.ItemID,
		RemoveLabels: []string{providers.LabelReady, providers.LabelCritical},
	})
	return err
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
func (p *Policy) TerminalNotifier(inner runner.TerminalNotifier) runner.TerminalNotifier {
	return func(runID string, phase journal.RunPhase, finalState string) error {
		var errs []error
		if phase == journal.PhaseCompleted || phase == journal.PhaseEscalated || phase == journal.PhaseAborted {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			attributedCtx, _ := AttributionContextForRun(ctx, p.RunsDir, runID, finalState)
			runURL, _ := p.State.RunURL(runID)
			if phase == journal.PhaseCompleted {
				if err := p.resetCircuitBreaker(attributedCtx, runID, runURL); err != nil {
					errs = append(errs, fmt.Errorf("reset circuit breaker for run %q: %w", runID, err))
				}
				if err := p.State.SettleNoWorkStreak(attributedCtx, p.Poster, runID, finalState, runURL); err != nil {
					errs = append(errs, fmt.Errorf("settle no-work streak for run %q: %w", runID, err))
				}
			} else if err := p.applyCircuitBreaker(attributedCtx, runID, finalState, runURL); err != nil {
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

// failureStreakExempt reports whether a failed terminal must stay out of the
// failure streak. The runner's own classification wins (#5638): a dispatch
// that exhausted its infrastructure retry budget is infra whatever code it
// surfaced under — a no-agent-turn harness startup failure carried a code
// ClassifyError could only call executor/unknown. Only an explicit class
// exempts that way; an unclassified terminal is judged by its code, so a
// bare session timeout still counts (#1054).
func failureStreakExempt(o runner.FailedOutcome) bool {
	class := failedOutcomeClass(o)
	return class.InfraFault() || class == telemetry.ErrorClassItemJudgment
}

// failedOutcomeClass is the terminal's class: the runner's explicit
// FaultClass when it set one, else the class of its code.
func failedOutcomeClass(o runner.FailedOutcome) telemetry.ErrorClass {
	if o.FaultClass != "" {
		return o.FaultClass
	}
	return telemetry.ClassifyError(o.Code)
}
