// Package backlogscope collects an explicitly scoped, bounded comparison set of
// backlog work items through a provider's paged ListWorkItems contract and
// reports how complete that collection is.
//
// A collection is a sequence of independent provider page reads, not an atomic
// snapshot: items can change, appear or disappear between pages. Coverage
// states that consistency limitation instead of implying a snapshot.
package backlogscope

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/providers"
)

// Comparison states a Scope accepts.
const (
	StateOpen = "open"
	StateAll  = "all"
)

// Coverage statuses, the reasons an incomplete collection stopped, and the
// consistency it provides.
const (
	StatusComplete   = "complete"
	StatusIncomplete = "incomplete"

	ReasonScanLimit = "scan-limit"
	ReasonDeadline  = "deadline"

	// ConsistencyPaged says the collection was read page by page with no
	// cross-page snapshot guarantee.
	ConsistencyPaged = "paged-non-atomic"
)

// PageSize is the per-page item limit Collect requests.
const PageSize = 100

// Lister is the read-only provider surface Collect uses.
type Lister interface {
	ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
}

// Scope is the comparison set a caller asked for.
type Scope struct {
	Repository providers.RepositoryRef
	// Labels must all be present on an item. They are sent to the provider as
	// ListWorkItemsRequest.Labels, which providers narrow server-side (Azure
	// DevOps WIQL [System.Tags] CONTAINS, GitHub/Gitea label filters).
	Labels []string
	// FieldPredicate is applied exactly to every retrieved item. Azure
	// DevOps also narrows its query by the predicate's required exact
	// area-path and work-item-type equalities (providers.
	// ADOQueryFieldEqualities) unless it rejects that query; the report
	// records what each page's query actually applied.
	FieldPredicate *fieldpredicate.Predicate
	// State is StateOpen or StateAll.
	State string
}

// ParseState validates a comparison state input; empty means StateOpen.
func ParseState(raw string) (string, error) {
	switch state := strings.ToLower(strings.TrimSpace(raw)); state {
	case "":
		return StateOpen, nil
	case StateOpen, StateAll:
		return state, nil
	default:
		return "", fmt.Errorf("invalid compareState %q (want %q or %q)", raw, StateOpen, StateAll)
	}
}

// Coverage reports how much of the requested scope a collection read.
type Coverage struct {
	// Status is StatusComplete only when the provider reported no further
	// page for the scope; otherwise it is StatusIncomplete with a Reason.
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// ExaminedCandidates is the provider's raw candidate count across pages,
	// before its exact post-retrieval filters.
	ExaminedCandidates int `json:"examinedCandidates"`
	// MaxCandidates is the raw candidate budget the collection ran under.
	MaxCandidates int `json:"maxCandidates"`
	Pages         int `json:"pages"`
	// ScopeMismatches counts returned items dropped because they did not
	// match the requested provider, state, labels or field predicate, or
	// repeated an item already collected.
	ScopeMismatches int    `json:"scopeMismatches"`
	Consistency     string `json:"consistency"`

	// queryNarrowed holds the provider-query filters applied on every page
	// read; narrowingFallbackPages counts pages whose provider dropped its
	// field clauses after rejecting the narrowed query.
	queryNarrowed          []string
	narrowingFallbackPages int
}

// recordNarrowing folds one page's provider-reported query narrowing into
// the collection: a filter counts as provider-narrowed only if every page
// applied it.
func (c *Coverage) recordNarrowing(page *providers.ListWorkItemsPageInfo) {
	if page.QueryNarrowingFallback {
		c.narrowingFallbackPages++
	}
	if c.Pages == 1 {
		c.queryNarrowed = append([]string{}, page.QueryNarrowed...)
		return
	}
	kept := c.queryNarrowed[:0]
	for _, name := range c.queryNarrowed {
		if slices.Contains(page.QueryNarrowed, name) {
			kept = append(kept, name)
		}
	}
	c.queryNarrowed = kept
}

// Complete reports whether the whole requested scope was read.
func (c Coverage) Complete() bool { return c.Status == StatusComplete }

// Collect reads scope page by page until the provider reports no next page,
// maxCandidates raw candidates have been examined, or ctx's deadline passes.
// Each page request carries the remaining budget as MaxCandidates, so the
// examined count never exceeds maxCandidates; a provider that overshoots it
// fails the collection.
// A provider error before the deadline is returned as an error: a failed read
// is never reported as a partial collection. maxCandidates must be positive.
func Collect(ctx context.Context, provider Lister, scope Scope, maxCandidates int) ([]providers.WorkItem, Coverage, error) {
	coverage := Coverage{MaxCandidates: maxCandidates, Consistency: ConsistencyPaged}
	if maxCandidates <= 0 {
		return nil, coverage, fmt.Errorf("comparison scan limit must be positive, got %d", maxCandidates)
	}
	var items []providers.WorkItem
	seen := make(map[string]bool)
	cursor := ""
	for {
		pageInfo := &providers.ListWorkItemsPageInfo{}
		remaining := maxCandidates - coverage.ExaminedCandidates
		page, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
			Repository:     scope.Repository,
			Labels:         scope.Labels,
			FieldPredicate: scope.FieldPredicate,
			State:          scope.State,
			// Limit stays constant so page-numbered provider cursors keep
			// their meaning; MaxCandidates is the raw budget left.
			Limit:         PageSize,
			MaxCandidates: remaining,
			Cursor:        cursor,
			PageInfo:      pageInfo,
			OldestFirst:   true,
		})
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				coverage.Status, coverage.Reason = StatusIncomplete, ReasonDeadline
				return items, coverage, nil
			}
			return nil, coverage, err
		}
		if pageInfo.CandidateCount < 0 || pageInfo.CandidateCount > remaining {
			return nil, coverage, fmt.Errorf("provider returned work-item candidate count %d outside the remaining scan budget %d",
				pageInfo.CandidateCount, remaining)
		}
		coverage.Pages++
		coverage.ExaminedCandidates += pageInfo.CandidateCount
		coverage.recordNarrowing(pageInfo)
		for _, item := range page {
			if seen[item.ID] || !scope.matches(item) {
				coverage.ScopeMismatches++
				continue
			}
			seen[item.ID] = true
			items = append(items, item)
		}
		if !pageInfo.HasNext {
			coverage.Status = StatusComplete
			return items, coverage, nil
		}
		if pageInfo.CandidateCount == 0 || pageInfo.NextCursor == "" || pageInfo.NextCursor == cursor {
			return nil, coverage, errors.New("provider returned a non-advancing work-item cursor")
		}
		if coverage.ExaminedCandidates >= maxCandidates {
			coverage.Status, coverage.Reason = StatusIncomplete, ReasonScanLimit
			return items, coverage, nil
		}
		cursor = pageInfo.NextCursor
	}
}

// matches rechecks an item the provider returned against the requested scope,
// so a provider-side substring tag match or a cross-provider item never enters
// the comparison set.
func (s Scope) matches(item providers.WorkItem) bool {
	if item.ID == "" {
		return false
	}
	if item.Provider != "" && s.Repository.Provider != "" && item.Provider != s.Repository.Provider {
		return false
	}
	if s.State == StateOpen && item.State != "" && !strings.EqualFold(item.State, StateOpen) {
		return false
	}
	for _, label := range s.Labels {
		if !containsFold(item.Labels, label) {
			return false
		}
	}
	matched, err := s.FieldPredicate.Matches(item.Fields)
	return err == nil && matched
}

// retrievalChecks names the checks matches and Collect's de-duplication
// apply to every retrieved item for this scope.
func (s Scope) retrievalChecks() []string {
	checks := []string{"identity"}
	if s.Repository.Provider != "" {
		checks = append(checks, "provider")
	}
	if s.State == StateOpen {
		checks = append(checks, "state")
	}
	if len(s.Labels) > 0 {
		checks = append(checks, "labels")
	}
	if !s.FieldPredicate.IsZero() {
		checks = append(checks, "fieldPredicate")
	}
	return checks
}

// Assessment values a Report carries.
const (
	AssessmentComplete   = "complete-input"
	AssessmentIncomplete = "incomplete-input"
)

// Report is the input-completeness section of a comparison artifact. It is
// separate from any ranked-output truncation the caller reports.
type Report struct {
	Coverage
	Scope ScopeReport `json:"scope"`
	// ClaimedNotCompared lists selected (claimed) item ids absent from the
	// collected comparison set: either outside the requested scope or not
	// reached by an incomplete collection. They were not compared.
	ClaimedNotCompared []string `json:"claimedNotCompared"`
	// Assessment is AssessmentComplete only when the collection was complete
	// and every selected item was compared. Even then, no ranked candidate
	// does not prove the absence of a semantic duplicate.
	Assessment string `json:"assessment"`
}

// ScopeReport records the requested comparison scope and where each part of
// it was applied.
type ScopeReport struct {
	State          string   `json:"state"`
	Labels         []string `json:"labels"`
	FieldPredicate string   `json:"fieldPredicate,omitempty"`
	// ProviderNarrowed names the scope parts the provider reported its query
	// actually applied on every page read ("labels", "state", and
	// "fieldPredicate:<field>" for a required exact equality on that field).
	// It is empty when no page was read.
	ProviderNarrowed []string `json:"providerNarrowed"`
	// ProviderNarrowingFallbackPages counts pages for which the provider
	// rejected its narrowed query and read without its field clauses; those
	// parts were then applied only after retrieval.
	ProviderNarrowingFallbackPages int `json:"providerNarrowingFallbackPages"`
	// FilteredAfterRetrieval names the scope checks applied exactly to every
	// retrieved item, whether or not the provider query also narrowed them:
	// "identity" (non-empty, not already collected), "provider", "state",
	// "labels" and "fieldPredicate".
	FilteredAfterRetrieval []string `json:"filteredAfterRetrieval"`
}

// NewReport builds the completeness report for a collection of scope that
// read items, given the selected ids that must each be compared.
func NewReport(scope Scope, fieldExpression string, coverage Coverage, items []providers.WorkItem, selectedIDs []string) Report {
	collected := make(map[string]bool, len(items))
	for _, item := range items {
		collected[item.ID] = true
	}
	missing := []string{}
	for _, id := range selectedIDs {
		if !collected[id] {
			missing = append(missing, id)
		}
	}
	scopeReport := ScopeReport{
		State:                          scope.State,
		Labels:                         append([]string{}, scope.Labels...),
		FieldPredicate:                 strings.TrimSpace(fieldExpression),
		ProviderNarrowed:               append([]string{}, coverage.queryNarrowed...),
		ProviderNarrowingFallbackPages: coverage.narrowingFallbackPages,
		FilteredAfterRetrieval:         scope.retrievalChecks(),
	}
	assessment := AssessmentComplete
	if !coverage.Complete() || len(missing) > 0 {
		assessment = AssessmentIncomplete
	}
	return Report{
		Coverage:           coverage,
		Scope:              scopeReport,
		ClaimedNotCompared: missing,
		Assessment:         assessment,
	}
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
