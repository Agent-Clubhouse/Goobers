// Package parking is the runner-free instance-level handling of a run whose
// stage reported blocked, and of an implement stage that found its fix already
// on main: parking the driving item and recording the learned block (#544/#552),
// or stripping the labels that would let it be reclaimed (#3236). The provider
// and the block records are handed in, so the daemon and the cloud controller
// share one policy.
package parking

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/terminalhooks"
	"github.com/goobers/goobers/providers"
)

// Poster is the one provider write the handlers make.
type Poster interface {
	UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

// Block is one learned dependency block the blocked handler records so backlog
// selection can skip the item until its blockers close (#552). The store stamps
// the record's time when it persists it.
type Block struct {
	Repository providers.RepositoryRef
	ItemID     string
	Blockers   []string
	RunID      string
	Stage      string
	Reason     string
}

// State is the instance state the blocked handler reads and updates.
type State interface {
	// ClaimedItemIDs returns the item ids runID currently claims.
	ClaimedItemIDs(runID string) ([]string, error)
	// BacklogRepository maps a run's code repository to the backlog repository
	// its work items live in.
	BacklogRepository(repo providers.RepositoryRef) providers.RepositoryRef
	// RecordBlock persists b and returns the dependency cycle it closes, if any.
	// The cycle is returned even when persisting fails after detection.
	RecordBlock(b Block) (blockedcycle.Result, error)
}

// Blocked is the instance-level consequence of a stage reporting status
// "blocked" (#544/#545/#552).
// Every blocked driving issue is parked (swap off goobers:ready and the
// provider-visible claim marker) per the #544 ruling / #539 convention. This
// prevents the released claim from making the same item immediately eligible
// again.
//
// The park label depends on whether the stage named a blocker (#2028): a
// named, non-cyclic blocker is goobers:blocked-on-sibling — a self-healing
// dependency park, not a decision only a human can make; the record below is
// what actually self-heals it (backlog selection's blocked-eligibility filter
// over State.RecordBlock's records), the label just needs to say so. An
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
func Blocked(ctx context.Context, poster Poster, state State, o terminalhooks.BlockedOutcome) error {
	itemIDs := []string{o.ItemID}
	if o.ItemID == "" {
		ids, err := state.ClaimedItemIDs(o.RunID)
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
	repoRef = state.BacklogRepository(repoRef)
	for _, itemID := range itemIDs {
		// #2961: an item can never be its own blocker. The runner already
		// drops the self-reference when the run carried its driving item,
		// but a run that claims its item(s) mid-run resolves them here, so
		// the same guard has to apply per item — otherwise a self-edge
		// reaches the blocked records and cycle detection parks the issue
		// needs-human for a dependency cycle that does not exist.
		blockers, _ := terminalhooks.FilterSelfBlockers(o.Blockers, itemID)
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
			cycle, err := state.RecordBlock(Block{
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
				errs = append(errs, EscalateCycle(ctx, poster, cycle))
				continue
			}
		}
		if _, err := poster.UpdateWorkItem(ctx, req); err != nil {
			errs = append(errs, fmt.Errorf("park blocked item %s#%s: %w", repoRef.Name, itemID, err))
		}
	}
	return errors.Join(errs...)
}

// EscalateCycle parks every member of a real dependency cycle goobers:needs-human
// and posts the cycle report on each (a human has to break a cycle). Used by the
// blocked handler when a new block closes a cycle, and by the controller's sweep
// when it resolves a cycle it had deferred. Idempotent: the labels converge and
// the comments are keyed by the caller's poster.
func EscalateCycle(ctx context.Context, poster Poster, cycle blockedcycle.Result) error {
	var errs []error
	comments := blockedcycle.Comments(cycle)
	for _, cycleItem := range cycle.Affected {
		for _, comment := range comments {
			req := providers.UpdateWorkItemRequest{
				Repository:   cycleItem.Repository,
				ID:           cycleItem.ItemID,
				Comment:      comment,
				AddLabels:    []string{providers.LabelNeedsHuman},
				RemoveLabels: []string{providers.LabelReady, providers.LabelClaimed},
			}
			if _, err := poster.UpdateWorkItem(ctx, req); err != nil {
				errs = append(errs, fmt.Errorf("escalate circular dependency on %s#%s: %w", cycleItem.Repository.Name, cycleItem.ItemID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// ExistingFix (#3236): strips goobers:ready and
// goobers:critical labels from an issue when the implement stage returns
// no-work with existingFixCommit set, preventing a permanent reclaim loop when
// the fix is already on main.
func ExistingFix(ctx context.Context, poster Poster, o terminalhooks.ExistingFixOutcome) error {
	if o.ItemID == "" {
		return nil
	}
	repoRef := providers.RepositoryRef{
		Provider: providers.ProviderKind(o.RepoRef.Provider),
		Owner:    o.RepoRef.Owner,
		Name:     o.RepoRef.Name,
	}
	// Strip both goobers:ready and goobers:critical labels to prevent reclaim
	_, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository:   repoRef,
		ID:           o.ItemID,
		RemoveLabels: []string{providers.LabelReady, providers.LabelCritical},
	})
	return err
}
