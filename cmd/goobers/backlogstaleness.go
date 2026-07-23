package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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
}

type curationClaimedItem struct {
	providers.WorkItem
	Staleness backlogStalenessSignal `json:"staleness"`
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
	for _, comment := range comments {
		if comment.CreatedAt == nil ||
			strings.EqualFold(comment.AuthorType, "bot") ||
			strings.EqualFold(comment.Author, botLogin) {
			continue
		}
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
	return backlogStalenessSignal{
		Stale:                    age >= policy.threshold(),
		AgeDays:                  int(age / (24 * time.Hour)),
		ThresholdDays:            policy.thresholdDays,
		LastMeaningfulActivityAt: lastActivity.UTC(),
		AutoCloseEnabled:         policy.autoCloseStale,
	}, nil
}
