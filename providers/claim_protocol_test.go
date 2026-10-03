package providers

import (
	"context"
	"testing"
)

func TestReleaseClaimWithExpectedHolderKeepsLabelWhenNewWinnerAppears(t *testing.T) {
	ctx := context.Background()
	winnerCalls := 0
	removed := false
	before := WorkItem{ID: "7", Labels: []string{LabelClaimed}}
	finalWithNewClaim := WorkItem{ID: "7", Labels: []string{LabelClaimed}}

	gotBefore, gotFinal, releasedRunID, err := releaseClaimWithProtocol(ctx, ClaimWorkItemRequest{
		ID:                 "7",
		RunID:              "current-run",
		LedgerAuthorized:   true,
		ExpectedClaimRunID: "old-run",
	}, releaseClaimProtocolHooks{
		validate: func() (string, error) { return LabelClaimed, nil },
		winner: func(context.Context) (string, bool, error) {
			winnerCalls++
			if winnerCalls == 1 {
				return "old-run", true, nil
			}
			return "new-run", true, nil
		},
		getItem: func(context.Context) (WorkItem, error) {
			if winnerCalls <= 1 {
				return before, nil
			}
			return finalWithNewClaim, nil
		},
		postRelease: func(context.Context, string) error { return nil },
		hasLabel:    func(item WorkItem, label string) bool { return item.HasLabel(label) },
		removeLabel: func(context.Context, string) (WorkItem, error) {
			removed = true
			return WorkItem{ID: "7"}, nil
		},
		readItemBeforeBreadcrumb: true,
	})
	if err != nil {
		t.Fatalf("releaseClaimWithProtocol: %v", err)
	}
	if removed {
		t.Fatal("removeLabel called after a new provider winner appeared")
	}
	if releasedRunID != "old-run" {
		t.Fatalf("releasedRunID = %q, want old-run", releasedRunID)
	}
	if !gotBefore.HasLabel(LabelClaimed) || !gotFinal.HasLabel(LabelClaimed) {
		t.Fatalf("before/final labels = %v/%v, want claim label preserved", gotBefore.Labels, gotFinal.Labels)
	}
}

func TestReleaseClaimWithExpectedHolderRestoresLabelWhenWinnerAppearsDuringRemoval(t *testing.T) {
	ctx := context.Background()
	winners := []struct {
		run     string
		claimed bool
	}{
		{run: "old-run", claimed: true},
		{claimed: false},
		{run: "new-run", claimed: true},
	}
	winnerCalls := 0
	removed := false
	restored := false
	before := WorkItem{ID: "7", Labels: []string{LabelClaimed}}
	removedLabel := WorkItem{ID: "7"}
	restoredLabel := WorkItem{ID: "7", Labels: []string{LabelClaimed}}

	_, gotFinal, _, err := releaseClaimWithProtocol(ctx, ClaimWorkItemRequest{
		ID:                 "7",
		RunID:              "current-run",
		LedgerAuthorized:   true,
		ExpectedClaimRunID: "old-run",
	}, releaseClaimProtocolHooks{
		validate: func() (string, error) { return LabelClaimed, nil },
		winner: func(context.Context) (string, bool, error) {
			if winnerCalls >= len(winners) {
				return "", false, nil
			}
			next := winners[winnerCalls]
			winnerCalls++
			return next.run, next.claimed, nil
		},
		getItem:     func(context.Context) (WorkItem, error) { return before, nil },
		postRelease: func(context.Context, string) error { return nil },
		hasLabel:    func(item WorkItem, label string) bool { return item.HasLabel(label) },
		removeLabel: func(context.Context, string) (WorkItem, error) {
			removed = true
			return removedLabel, nil
		},
		restoreLabel: func(context.Context, string) (WorkItem, error) {
			restored = true
			return restoredLabel, nil
		},
		readItemBeforeBreadcrumb: true,
	})
	if err != nil {
		t.Fatalf("releaseClaimWithProtocol: %v", err)
	}
	if !removed || !restored {
		t.Fatalf("removed/restored = %v/%v, want both", removed, restored)
	}
	if !gotFinal.HasLabel(LabelClaimed) {
		t.Fatalf("final labels = %v, want restored claim label", gotFinal.Labels)
	}
}

func TestReleaseClaimWithExpectedHolderKeepsLabelWhenInitialWinnerAlreadyGone(t *testing.T) {
	ctx := context.Background()
	winnerCalls := 0
	removed := false
	before := WorkItem{ID: "7", Labels: []string{LabelClaimed}}
	finalWithNewClaim := WorkItem{ID: "7", Labels: []string{LabelClaimed}}

	_, gotFinal, _, err := releaseClaimWithProtocol(ctx, ClaimWorkItemRequest{
		ID:                 "7",
		RunID:              "current-run",
		LedgerAuthorized:   true,
		ExpectedClaimRunID: "old-run",
	}, releaseClaimProtocolHooks{
		validate: func() (string, error) { return LabelClaimed, nil },
		winner: func(context.Context) (string, bool, error) {
			winnerCalls++
			if winnerCalls == 1 {
				return "", false, nil
			}
			return "new-run", true, nil
		},
		getItem: func(context.Context) (WorkItem, error) {
			if winnerCalls <= 1 {
				return before, nil
			}
			return finalWithNewClaim, nil
		},
		postRelease: func(context.Context, string) error { return nil },
		hasLabel:    func(item WorkItem, label string) bool { return item.HasLabel(label) },
		removeLabel: func(context.Context, string) (WorkItem, error) {
			removed = true
			return WorkItem{ID: "7"}, nil
		},
		restoreLabel: func(context.Context, string) (WorkItem, error) {
			return finalWithNewClaim, nil
		},
		readItemBeforeBreadcrumb: true,
	})
	if err != nil {
		t.Fatalf("releaseClaimWithProtocol: %v", err)
	}
	if removed {
		t.Fatal("removeLabel called after a new provider winner appeared")
	}
	if !gotFinal.HasLabel(LabelClaimed) {
		t.Fatalf("final labels = %v, want claim label preserved", gotFinal.Labels)
	}
}
