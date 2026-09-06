package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

const maxStaleAfterDays = int((1<<63 - 1) / int64(24*time.Hour))

type backlogStalenessPolicy struct {
	thresholdDays  int
	autoCloseStale bool
}

func (p backlogStalenessPolicy) threshold() time.Duration {
	return time.Duration(p.thresholdDays) * 24 * time.Hour
}

type backlogStalenessSignal struct {
	Stale                    bool      `json:"stale"`
	AgeDays                  int       `json:"ageDays"`
	ThresholdDays            int       `json:"thresholdDays"`
	LastMeaningfulActivityAt time.Time `json:"lastMeaningfulActivityAt"`
	AutoCloseEnabled         bool      `json:"autoCloseEnabled"`
	// Integrity is the weakest provenance among the inputs this signal was
	// derived from — the work item plus every comment that moved
	// LastMeaningfulActivityAt. Without it the serialized item kept only the
	// item's own (often maintainer) grade, so an unapproved commenter could
	// move stale/ageDays and have the result admitted as maintainer input
	// (TBH-4).
	Integrity apiv1.Integrity `json:"integrity,omitempty"`
}

type curationClaimedItem struct {
	providers.WorkItem
	Staleness    backlogStalenessSignal `json:"staleness"`
	CurationMode string                 `json:"curationMode,omitempty"`
	ReadOnly     bool                   `json:"readOnly,omitempty"`
}

func readBacklogStalenessPolicy() (backlogStalenessPolicy, error) {
	rawDays := strings.TrimSpace(providerInput("staleAfterDays", strconv.Itoa(int(defaultStaleAfter/(24*time.Hour)))))
	days, err := strconv.Atoi(rawDays)
	if err != nil || days < 1 || days > maxStaleAfterDays {
		return backlogStalenessPolicy{}, fmt.Errorf(
			"invalid staleAfterDays %q (want an integer from 1 through %d)",
			rawDays,
			maxStaleAfterDays,
		)
	}

	rawAutoClose := strings.TrimSpace(providerInput("staleAutoClose", "false"))
	switch rawAutoClose {
	case "true":
		return backlogStalenessPolicy{thresholdDays: days, autoCloseStale: true}, nil
	case "false":
		return backlogStalenessPolicy{thresholdDays: days}, nil
	default:
		return backlogStalenessPolicy{}, fmt.Errorf(
			"invalid staleAutoClose %q (want true or false)",
			rawAutoClose,
		)
	}
}

func enrichClaimedItemsWithStaleness(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	items []providers.WorkItem,
	observedAt time.Time,
	policy backlogStalenessPolicy,
) ([]curationClaimedItem, error) {
	botLogin, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve curation actor: %w", err)
	}

	enriched := make([]curationClaimedItem, 0, len(items))
	for _, item := range items {
		comments, err := provider.ListComments(ctx, repo, item.ID)
		if err != nil {
			return nil, fmt.Errorf("list comments for issue #%s: %w", item.ID, err)
		}
		signal, err := calculateBacklogStaleness(item, comments, botLogin, observedAt, policy)
		if err != nil {
			return nil, fmt.Errorf("issue #%s: %w", item.ID, err)
		}
		enriched = append(enriched, curationClaimedItem{WorkItem: item, Staleness: signal})
	}
	return enriched, nil
}

func calculateBacklogStaleness(
	item providers.WorkItem,
	comments []providers.Comment,
	botLogin string,
	observedAt time.Time,
	policy backlogStalenessPolicy,
) (backlogStalenessSignal, error) {
	lastActivity := time.Time{}
	if item.CreatedAt != nil {
		lastActivity = *item.CreatedAt
	} else if item.UpdatedAt != nil {
		lastActivity = *item.UpdatedAt
	}
	// The item's own grade plus each contributing comment's; aggregated below.
	grades := []apiv1.Integrity{}
	if item.Integrity != "" {
		grades = append(grades, item.Integrity)
	}
	for _, comment := range comments {
		if comment.CreatedAt == nil ||
			strings.EqualFold(comment.AuthorType, "bot") ||
			strings.EqualFold(comment.Author, botLogin) {
			continue
		}
		// This comment is admitted as staleness evidence, so its provenance
		// travels with the signal it helps produce — whether or not it ends up
		// being the latest activity.
		grades = append(grades, comment.Integrity)
		if lastActivity.IsZero() || comment.CreatedAt.After(lastActivity) {
			lastActivity = *comment.CreatedAt
		}
	}
	if lastActivity.IsZero() {
		return backlogStalenessSignal{}, fmt.Errorf("provider returned no creation or activity timestamp")
	}

	age := observedAt.Sub(lastActivity)
	if age < 0 {
		age = 0
	}
	stale := age >= policy.threshold()
	// Checklist-only tracking parents are maintained through their children.
	// Their curation comments must not create a zero-day stale-notice loop.
	if item.HasLabel(providers.LabelTracking) && !item.HasLabel(providers.LabelAutoClose) {
		stale = false
	}
	return backlogStalenessSignal{
		Stale:                    stale,
		AgeDays:                  int(age / (24 * time.Hour)),
		ThresholdDays:            policy.thresholdDays,
		LastMeaningfulActivityAt: lastActivity.UTC(),
		AutoCloseEnabled:         policy.autoCloseStale,
		Integrity:                stalenessIntegrity(grades),
	}, nil
}

// stalenessIntegrity aggregates the provenance of everything a staleness signal
// was derived from. An unlabeled contributor collapses the whole signal to
// unapproved rather than being skipped: a comment with no grade is exactly the
// case that must not be admitted as maintainer evidence.
func stalenessIntegrity(grades []apiv1.Integrity) apiv1.Integrity {
	if len(grades) == 0 {
		return ""
	}
	for _, grade := range grades {
		if !grade.Valid() {
			return apiv1.IntegrityUnapproved
		}
	}
	return apiv1.WeakestIntegrity(grades...)
}
