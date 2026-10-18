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
	// Staleness is nil only when StalenessUnavailable says why: an absent
	// signal must never serialize as a zero-valued "stale": false, which the
	// curator would read as fresh evidence.
	Staleness *backlogStalenessSignal `json:"staleness,omitempty"`
	// StalenessUnavailable is set ("provider") when the backlog provider
	// cannot supply the comment history staleness is computed from.
	StalenessUnavailable string `json:"stalenessUnavailable,omitempty"`
	CurationMode         string `json:"curationMode,omitempty"`
	ReadOnly             bool   `json:"readOnly,omitempty"`
}

// stalenessUnavailableProvider is StalenessUnavailable's value when the
// backlog provider offers no staleness evidence surface.
const stalenessUnavailableProvider = "provider"

// stalenessCommentProvider is the provider surface claimed-item staleness
// needs: the item's comment history. GitHub, Azure DevOps and Gitea all
// implement it.
type stalenessCommentProvider interface {
	ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error)
}

type stalenessLoginProvider interface {
	AuthenticatedLogin(context.Context) (string, error)
}

type stalenessIdentityProvider interface {
	AuthenticatedIdentity(context.Context) (providers.ADOIdentity, error)
}

// stalenessCommentFilter is how enrichment recognizes Goobers' own comments,
// which never count as meaningful activity. botLogin is compared with a
// comment's Author by calculateBacklogStaleness; selfID, when set, drops
// comments whose stable AuthorID matches before that.
type stalenessCommentFilter struct {
	botLogin string
	selfID   string
}

// resolveStalenessCommentFilter resolves the acting identity. Azure DevOps
// comment authors are display names, which are not unique, so there the
// stable identity ID is matched against Comment.AuthorID instead, and the ID
// also stands in as botLogin, which no display name equals (Goobers#6104).
// Every other provider keeps its AuthenticatedLogin comparison unchanged.
func resolveStalenessCommentFilter(ctx context.Context, provider stalenessCommentProvider) (stalenessCommentFilter, error) {
	if identified, ok := provider.(stalenessIdentityProvider); ok {
		identity, err := identified.AuthenticatedIdentity(ctx)
		if err != nil {
			return stalenessCommentFilter{}, err
		}
		return stalenessCommentFilter{botLogin: identity.ID, selfID: identity.ID}, nil
	}
	logged, ok := provider.(stalenessLoginProvider)
	if !ok {
		return stalenessCommentFilter{}, fmt.Errorf("provider cannot report its authenticated login")
	}
	login, err := logged.AuthenticatedLogin(ctx)
	return stalenessCommentFilter{botLogin: login}, err
}

func (f stalenessCommentFilter) keep(comments []providers.Comment) []providers.Comment {
	if f.selfID == "" {
		return comments
	}
	kept := make([]providers.Comment, 0, len(comments))
	for _, comment := range comments {
		if strings.TrimSpace(comment.AuthorID) != f.selfID {
			kept = append(kept, comment)
		}
	}
	return kept
}

// markStalenessUnavailable returns items with no staleness signal and the
// explicit unavailable marker, for a backlog provider with no comment surface.
func markStalenessUnavailable(items []providers.WorkItem) []curationClaimedItem {
	marked := make([]curationClaimedItem, 0, len(items))
	for _, item := range items {
		marked = append(marked, curationClaimedItem{WorkItem: item, StalenessUnavailable: stalenessUnavailableProvider})
	}
	return marked
}

func readBacklogStalenessPolicy() (backlogStalenessPolicy, error) {
	days, err := parseIntInput(
		strings.TrimSpace(providerInput("staleAfterDays", strconv.Itoa(int(defaultStaleAfter/(24*time.Hour))))),
		func(value int) bool { return value >= 1 && value <= maxStaleAfterDays },
		func(raw string, _ error) string {
			return fmt.Sprintf("invalid staleAfterDays %q (want an integer from 1 through %d)", raw, maxStaleAfterDays)
		},
	)
	if err != nil {
		return backlogStalenessPolicy{}, err
	}

	autoClose, err := parseBoolInput(
		strings.TrimSpace(providerInput("staleAutoClose", "false")),
		func(raw string, _ bool) bool { return raw == "true" || raw == "false" },
		func(raw string, _ error) string {
			return fmt.Sprintf("invalid staleAutoClose %q (want true or false)", raw)
		},
	)
	if err != nil {
		return backlogStalenessPolicy{}, err
	}
	return backlogStalenessPolicy{thresholdDays: days, autoCloseStale: autoClose}, nil
}

func enrichClaimedItemsWithStaleness(
	ctx context.Context,
	provider stalenessCommentProvider,
	repo providers.RepositoryRef,
	items []providers.WorkItem,
	observedAt time.Time,
	policy backlogStalenessPolicy,
) ([]curationClaimedItem, error) {
	if provider == nil {
		return markStalenessUnavailable(items), nil
	}
	filter, err := resolveStalenessCommentFilter(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("resolve curation actor: %w", err)
	}

	enriched := make([]curationClaimedItem, 0, len(items))
	for _, item := range items {
		comments, err := provider.ListComments(ctx, repo, item.ID)
		if err != nil {
			return nil, fmt.Errorf("list comments for issue #%s: %w", item.ID, err)
		}
		signal, err := calculateBacklogStaleness(item, filter.keep(comments), filter.botLogin, observedAt, policy)
		if err != nil {
			return nil, fmt.Errorf("issue #%s: %w", item.ID, err)
		}
		enriched = append(enriched, curationClaimedItem{WorkItem: item, Staleness: &signal})
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
