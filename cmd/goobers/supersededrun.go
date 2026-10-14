package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// supersededRunEvidence is what a completed run's journal says about why it
// stopped, when what it says is "the pull request moved under it" (#6019).
type supersededRunEvidence struct {
	// selectedNumber is the PR a pr-claim stale-selection result named. That
	// guard releases the run's claim itself, so by the terminal the claim
	// ledger no longer says which PR the run was working on.
	selectedNumber string
	// published is true when the run's own push-remediated published its
	// work before the PR moved: that cycle's attempt was real and spent.
	published bool
}

// settleSupersededRun is the completed-terminal half of #6019. A run is
// superseded when the pull request it was remediating moved under it —
// someone pushed to it while the run was working — and it stopped rather than
// discard that push or build on a revision it never selected:
//
//   - a pod refused to land the run's delta on the moved head and the engine
//     ended the run on runner.WorkspaceHeadMovedErrorCode (a journaled error
//     event at the stage or gate the run ended on), or
//   - a pr-claim guard (or another revision check reporting the same typed
//     outcome) found the PR at a different head and ended the run no-work
//     with outcome stale_selection.
//
// Either way the run is evidence about neither the work nor the item, so it
// reports superseded=true and the caller leaves the failure and no-work
// streaks exactly as they were. The cycle's provisional remediation charge is
// refunded (remediationState.Superseded) unless the run had already published
// its own work: the next cycle remediates the PR at its new head without
// having been charged an attempt for work the push made moot.
//
// A journal that cannot be read is not superseded, which is what every
// completed run was before this existed — the same posture settleNoWorkStreak
// takes, for the same reason.
func settleSupersededRun(ctx context.Context, poster gate.Commenter, l instance.Layout, runID, finalState string) (bool, error) {
	if finalState == "" {
		return false, nil
	}
	reader, err := journal.OpenReadOnly(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return false, nil
	}
	events, err := reader.Events()
	if err != nil {
		return false, nil
	}
	evidence, superseded := supersededRunFromEvents(events, finalState)
	if !superseded || evidence.published {
		return superseded, nil
	}
	items, err := supersededRunItems(l, runID, evidence)
	if err != nil {
		return true, fmt.Errorf("resolve superseded run's pull request: %w", err)
	}
	return true, markRemediationCharges(ctx, poster, items, runID, func(state *remediationState) {
		state.Superseded = true
	})
}

// supersededRunFromEvents reads the run's terminal reason from its journal.
// Only the stage or gate the run ended on counts (event.Stage == finalState,
// top-level events only), the same filter nowork.TerminalFromEvents applies
// and for the same reason: an earlier stage's outcome is not the run's.
func supersededRunFromEvents(events []journal.Event, finalState string) (supersededRunEvidence, bool) {
	var evidence supersededRunEvidence
	superseded := false
	for _, event := range events {
		if _, ok := prPublishedHead(event); ok {
			evidence.published = true
		}
		if event.Stage != finalState || event.Branch != 0 {
			continue
		}
		switch {
		case event.Type == journal.EventError && event.Error != nil && event.Error.Code == runner.WorkspaceHeadMovedErrorCode:
			superseded = true
		case event.Type == journal.EventStageFinished:
			stale := event.Status == string(apiv1.ResultNoWork) && stringOutputValue(event.Outputs, "outcome") == prClaimOutcomeStaleSelection
			superseded = stale
			if stale {
				evidence.selectedNumber = stringOutputValue(event.Outputs, "selectedNumber")
			}
		}
	}
	return evidence, superseded
}

// supersededRunItems resolves the pull request a superseded run was working
// on: the run's still-held claim (a head-moved stop keeps it until terminal
// finalization), else the PR its stale-selection result named, through the
// repository identity the claim recorded when it was taken (#4417).
func supersededRunItems(l instance.Layout, runID string, evidence supersededRunEvidence) ([]claimedItem, error) {
	items, err := claimedItemsForRun(l, runID)
	if err != nil || len(items) > 0 || evidence.selectedNumber == "" {
		return items, err
	}
	number, err := strconv.Atoi(strings.TrimSpace(evidence.selectedNumber))
	if err != nil || number <= 0 {
		return nil, fmt.Errorf("stale-selection result names PR %q", evidence.selectedNumber)
	}
	id := pullRequestClaimKey(number)
	recorded, err := loadItemRepositories(l, runID, []string{id})
	if err != nil {
		return nil, err
	}
	entry, ok := recorded[id]
	if !ok {
		return nil, fmt.Errorf("%w: run %s item %s", ErrItemRepositoryUnknown, runID, id)
	}
	return []claimedItem{{ItemID: id, Kind: entry.kind, Purpose: entry.purpose, Repo: entry.repo}}, nil
}

func stringOutputValue(outputs map[string]any, key string) string {
	value, _ := outputs[key].(string)
	return strings.TrimSpace(value)
}
