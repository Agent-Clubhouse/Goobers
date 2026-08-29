package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

// Parked-backlog visibility (#3355).
//
// A park disposition (#2028) swaps goobers:ready for a park label, and
// query-backlog requires goobers:ready — so a parked item is not deferred or
// queued, it leaves the instance's ready pool entirely and nothing in the
// pipeline re-adds the label. On an unattended instance that reads as work
// silently deleted from the backlog: no error, no queue, no signal.
//
// Status therefore reports what the ready pool no longer shows. This is a
// read-only display path: it changes no label mechanics (backlog reconcile
// still owns the ready/park exclusivity rule) and adds no configuration
// surface — it only makes the exit from the pool visible.
const (
	// statusParkedBacklogQueryLimit bounds each park-label query. One page per
	// disposition keeps the status read cheap; a backlog with more parked items
	// than this is already a louder problem than the count line can convey, and
	// parkedBacklogStatusText says the list is partial.
	statusParkedBacklogQueryLimit = 100
	// statusParkedBacklogListLimit bounds how many parked items status prints
	// by ref. The count line always reports the full total.
	statusParkedBacklogListLimit = 10
)

// statusParkedDispositions are the park labels that cannot coexist with
// goobers:ready — the same set backlogreconcile.go's itemHasParkLabel
// enforces. Order is display order: human decision first, then the two
// mechanical parks.
var statusParkedDispositions = []string{
	providers.LabelNeedsHuman,
	blockedOnSiblingLabel,
	needsRemediationLabel,
}

// statusParkedItem is one open work item that carries a park disposition but
// not goobers:ready — an item the instance's own backlog view no longer shows.
type statusParkedItem struct {
	ID           string   `json:"id"`
	Ref          string   `json:"ref"`
	Title        string   `json:"title,omitempty"`
	Dispositions []string `json:"dispositions"`
}

// statusParkedBacklog is the parked-item snapshot behind the status section.
// Total counts every parked item found; Items is capped at
// statusParkedBacklogListLimit for display.
type statusParkedBacklog struct {
	Total int                `json:"total"`
	Items []statusParkedItem `json:"items"`
}

func parkedBacklogStatusText(parked statusParkedBacklog) string {
	var text strings.Builder
	fmt.Fprintf(&text,
		"Parked backlog items (park label, no %s — not selectable): %d\n",
		providers.LabelReady, parked.Total,
	)
	for _, item := range parked.Items {
		line := fmt.Sprintf("  %s %s", item.Ref, strings.Join(item.Dispositions, ","))
		if item.Title != "" {
			line += " — " + item.Title
		}
		text.WriteString(line + "\n")
	}
	if more := parked.Total - len(parked.Items); more > 0 {
		fmt.Fprintf(&text, "  ... and %d more\n", more)
	}
	if parked.Total > 0 {
		fmt.Fprintf(&text,
			"  No workflow re-adds %s — triage these or they stay out of the backlog.\n",
			providers.LabelReady,
		)
	}
	return text.String()
}

func parkedBacklogStatusUnavailableText(err error) string {
	return fmt.Sprintf("Parked backlog items unavailable: %v\n", err)
}

var loadStatusParkedBacklog = queryStatusParkedBacklog

// statusParkedBacklogCache bounds how often the parked-item query re-hits the
// provider, on the same cadence as the open-PR label counts so a `status
// --watch` redraw loop cannot amplify API traffic.
type statusParkedBacklogCache struct {
	load     func(context.Context, *instance.Config) (statusParkedBacklog, error)
	now      func() time.Time
	loadedAt time.Time
	parked   statusParkedBacklog
	err      error
}

func newStatusParkedBacklogCache() *statusParkedBacklogCache {
	return &statusParkedBacklogCache{
		load: loadStatusParkedBacklog,
		now:  time.Now,
	}
}

func (c *statusParkedBacklogCache) Load(ctx context.Context, cfg *instance.Config) (statusParkedBacklog, error) {
	if c.loadedAt.IsZero() || !c.now().Before(c.loadedAt.Add(localscheduler.DefaultOpenPRRefreshInterval)) {
		c.parked, c.err = c.load(ctx, cfg)
		c.loadedAt = c.now()
	}
	return c.parked, c.err
}

// queryStatusParkedBacklog lists the open work items that carry a park
// disposition without goobers:ready. It mirrors queryStatusPRLabelCounts's
// posture exactly (#683): its own store registry, nil registrar because status
// writes no journal, and the primary configured repository — status is an
// instance-level display path.
func queryStatusParkedBacklog(ctx context.Context, cfg *instance.Config) (statusParkedBacklog, error) {
	if len(cfg.Repos) == 0 {
		return statusParkedBacklog{}, errors.New("no target repository configured")
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return statusParkedBacklog{}, err
	}
	resolver, _, err := buildCredentials(cfg, stores, "", "", nil, nil)
	if err != nil {
		return statusParkedBacklog{}, err
	}
	repo := cfg.Repos[0]
	ref := repo.Owner + "/" + repo.Name
	ctx, cancel := context.WithTimeout(ctx, statusProviderQueryTimeout)
	defer cancel()
	token, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return statusParkedBacklog{}, fmt.Errorf("resolve status token for %s: %w", ref, err)
	}
	provider := newStatusGitHubProvider(token)
	repoRef := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    repo.Owner,
		Name:     repo.Name,
	}

	byID := make(map[string]*statusParkedItem)
	var order []string
	for _, disposition := range statusParkedDispositions {
		items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
			Repository: repoRef,
			Labels:     []string{disposition},
			State:      "open",
			Limit:      statusParkedBacklogQueryLimit,
		})
		if err != nil {
			return statusParkedBacklog{}, fmt.Errorf("list %s work items for %s: %s",
				disposition, ref, scrubRepositoryError(err, token))
		}
		for _, item := range items {
			// A parked item that still carries goobers:ready is still
			// selectable — backlog reconcile resolves that conflict; it is not
			// invisible work, so it is not this section's subject.
			if item.HasLabel(providers.LabelReady) {
				continue
			}
			existing, ok := byID[item.ID]
			if !ok {
				existing = &statusParkedItem{
					ID:    item.ID,
					Ref:   statusParkedRef(item.ID),
					Title: item.Title,
				}
				byID[item.ID] = existing
				order = append(order, item.ID)
			}
			existing.Dispositions = append(existing.Dispositions, disposition)
		}
	}

	parked := statusParkedBacklog{Total: len(order)}
	sort.Slice(order, func(i, j int) bool { return statusParkedIDLess(order[i], order[j]) })
	for _, id := range order {
		if len(parked.Items) == statusParkedBacklogListLimit {
			break
		}
		parked.Items = append(parked.Items, *byID[id])
	}
	return parked, nil
}

// statusParkedRef renders a work-item id the way an operator reads it: #N for
// a numeric provider id, the raw id otherwise.
func statusParkedRef(id string) string {
	if id == "" {
		return id
	}
	if _, err := strconv.Atoi(id); err != nil {
		return id
	}
	return "#" + id
}

// statusParkedIDLess orders numeric ids numerically (so #9 precedes #168) and
// falls back to lexical order for anything else, keeping the display stable.
func statusParkedIDLess(a, b string) bool {
	na, aerr := strconv.Atoi(a)
	nb, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return na < nb
	}
	if aerr == nil {
		return true
	}
	if berr == nil {
		return false
	}
	return a < b
}
