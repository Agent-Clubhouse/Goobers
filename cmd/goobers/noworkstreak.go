package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/nowork"
	"github.com/goobers/goobers/providers"
)

// noWorkTerminalForRun reads the completed run's journal for its terminal verdict.
func noWorkTerminalForRun(l instance.Layout, runID, finalState string) (nowork.Terminal, bool, error) {
	if finalState == "" {
		return nowork.Terminal{}, false, nil
	}
	reader, err := journal.OpenReadOnly(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return nowork.Terminal{}, false, fmt.Errorf("open journal for run %q: %w", runID, err)
	}
	events, err := reader.Events()
	if err != nil {
		return nowork.Terminal{}, false, fmt.Errorf("read journal for run %q: %w", runID, err)
	}
	terminal, ok := nowork.TerminalFromEvents(events, finalState)
	return terminal, ok, nil
}

// settleNoWorkStreak is the completed-terminal entry point: it decides whether
// this run counted as work and routes to the increment or the reset.
//
// It is kept as one call so the terminal notifier gains a single branch
// rather than the classification logic itself — the same decomposition idiom
// #5107 used for noWorkRepassOutcome, and what the complexity gate rewards.
//
// A journal that cannot be read leaves the streak ALONE — it neither
// increments nor resets.
//
// Resetting would be the more obvious "fail safe", but it is not safe here: a
// transient read fault on a run that really was no-work would silently discard
// an accumulated streak, and a recurrent one (a pruned run directory, a
// truncated journal, an I/O error) would make the park permanently unreachable
// with no diagnostic. Incrementing would be worse still, walking an item
// toward a park on no evidence. Doing nothing preserves whatever the last
// readable terminal established.
//
// The read error is deliberately NOT propagated, matching the posture its
// immediate sibling in escalationnotify.Policy.TerminalNotifier takes
// (`attributedCtx, _ := AttributionContextForRun(...)`): a terminal whose run
// directory has been pruned, or which never had a journal, is an ordinary
// condition on this path and must not be reported as a failed terminal
// notification. Errors from the state-plane work below ARE returned, because
// those represent a streak the instance failed to account for.
func settleNoWorkStreak(
	ctx context.Context,
	poster gate.Commenter,
	l instance.Layout,
	runID, finalState, runURL string,
) error {
	terminal, isNoWork, err := noWorkTerminalForRun(l, runID, finalState)
	if err != nil {
		return nil
	}
	if isNoWork {
		return applyNoWorkStreak(ctx, poster, l, runID, terminal, runURL)
	}
	return resetNoWorkStreaks(ctx, poster, l, runID, runURL)
}

// applyNoWorkStreak increments the repeated-no-work counter for every item the
// completed run held, and parks the item once it reaches the threshold.
//
// Ordering mirrors the escalationnotify circuit breaker deliberately: the
// authoritative state update happens FIRST and the provider mutation second,
// so a park that cannot reach the provider still leaves a durable record that
// the protection was owed. #5379's scope decision requires "idempotent
// finalization" — re-running this for an already-parked item is safe because
// the provider mutation is a label swap that converges, and because a terminal notification that is
// retried after a partial failure re-reads the persisted count rather than
// recomputing it from provider state.
// Only a run holding EXACTLY ONE claimed item is counted. A no-work verdict
// says "this run found nothing to do"; when a run holds a batch, nothing in
// that verdict attributes the conclusion to any particular member of the
// batch, so charging it to all of them is simply wrong.
//
// This is not hypothetical. backlog-curation claims up to 20 items per run
// (maxItems: "20") and its agentic `curate` stage short-circuits to a
// completed terminal without ever reaching `release-claim`, so all 20 claims
// are still held here. Charging each of them would stamp goobers:needs-human
// across 20 backlog items per no-work curation run — items that do not even
// carry goobers:ready, so no re-offer loop is being broken and the park is
// pure damage. Worse, that workflow's own selection filters on park labels, so
// the items would be permanently excluded from curation thereafter.
//
// The escalationnotify circuit breaker fans out across every claimed item, but
// it is only reached from failure and escalation terminals where "everything
// this run held is implicated" is defensible. On a completed terminal it is
// not.
//
// #5379's own loop is a single-item implementation run, so the narrow rule
// covers the reported defect exactly.
//
// A curation claim is never counted either (#6293). curate-resweep re-selects
// items that are already goobers:ready precisely to confirm they still are, so
// "no mutation needed" is its expected steady state; charging that to the
// item would strip readiness from correctly ready, actionable work. The
// purpose comes from the workflow's own declared selection (`curation:
// "true"` or `--resweep`), recorded with the claim, not from its name.
func applyNoWorkStreak(
	ctx context.Context,
	poster gate.Commenter,
	l instance.Layout,
	runID string,
	terminal nowork.Terminal,
	runURL string,
) error {
	items, err := claimedItemsForRun(l, runID)
	if err != nil {
		return err
	}
	if len(items) != 1 || items[0].Purpose == itemPurposeCuration {
		return nil
	}
	repoRef, itemID := items[0].Repo, items[0].ItemID
	recorded, previous, err := incrementNoWorkStreak(ctx, l, repoRef, itemID, runID, terminal)
	if err != nil {
		return fmt.Errorf("persist no-work streak on %s#%s: %w", repoRef.Name, itemID, err)
	}
	contradiction := nowork.VerdictConflict(previous, recorded)
	if recorded.Count < nowork.StreakThreshold {
		// #5643: every verdict below the park threshold is written to the
		// issue too, so it is never left looking like an item nobody read.
		comment := nowork.VerdictComment(recorded, contradiction, runID, runURL)
		if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repoRef, ID: itemID, Comment: comment,
		}); err != nil {
			return fmt.Errorf("record no-work verdict on %s#%s: %w", repoRef.Name, itemID, err)
		}
		return nil
	}
	comment := nowork.ParkComment(recorded, contradiction, runID, runURL)
	if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository:   repoRef,
		ID:           itemID,
		Comment:      comment,
		AddLabels:    []string{providers.LabelNeedsHuman},
		RemoveLabels: []string{providers.LabelReady},
	}); err != nil {
		return fmt.Errorf("park repeated no-work on %s#%s: %w", repoRef.Name, itemID, err)
	}
	return nil
}

// resetNoWorkStreaks clears the repeated-no-work counter for every item a
// PRODUCTIVE completion held.
//
// The reset itself depends on nothing but the state plane. #5379's scope
// decision requires that "productive completion resets the correct streaks
// even when provider notifications fail", so the one provider call here — the
// #5643 contradiction comment — runs only after every reset has landed, and
// its failure is reported without undoing any of them. That failure is not
// retried: a replayed notification finds the record already cleared, so the
// flag is lost rather than ever blocking or repeating a reset. A stale park comment
// left on an item is cosmetic; a streak that failed to reset would eventually
// park an item that is demonstrably producing work.
func resetNoWorkStreaks(ctx context.Context, poster gate.Commenter, l instance.Layout, runID, runURL string) error {
	items, err := claimedItemsForRun(l, runID)
	if err != nil {
		return err
	}
	var errs []error
	contradicted := map[int]nowork.Record{}
	for i, item := range items {
		cleared, err := resetNoWorkStreakState(ctx, l, item.Repo, item.ItemID, runID)
		if err != nil {
			errs = append(errs, fmt.Errorf("reset no-work streak on %s#%s: %w", item.Repo.Name, item.ItemID, err))
		} else if cleared.Count > 0 {
			contradicted[i] = cleared
		}
	}
	// #5643: a run that completed without a no-work verdict disagrees with the
	// verdict it just cleared, so say so on the issue — after every reset has
	// landed, so a provider outage can never block one. Single-item runs only,
	// for applyNoWorkStreak's reason: a batch run's completion says nothing
	// about any one member.
	if len(items) == 1 && len(contradicted) == 1 {
		item := items[0]
		comment := nowork.ContradictionComment(contradicted[0], runID, runURL)
		if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: item.Repo, ID: item.ItemID, Comment: comment,
		}); err != nil {
			errs = append(errs, fmt.Errorf("flag contradicted no-work verdict on %s#%s: %w", item.Repo.Name, item.ItemID, err))
		}
	}
	return errors.Join(errs...)
}
