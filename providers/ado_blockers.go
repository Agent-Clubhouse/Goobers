package providers

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// adoPredecessorRel is the relation a successor carries for each
// predecessor that blocks it ("Predecessor" in the ADO UI).
const adoPredecessorRel = "System.LinkTypes.Dependency-Reverse"

// adoDefaultDoneCategories are the state categories that count as done when
// a gaggle declares no backlog.doneStates categories. Resolved is included
// because a Resolved Bug means its code has landed: completing a PR with
// transitionWorkItems leaves a linked Bug in Resolved, not Completed
// (docs/design/ado-parity-dsl-2-0.md §6).
var adoDefaultDoneCategories = []string{"Resolved", "Completed", "Removed"}

// ADODoneStates is the provider form of a gaggle's backlog.doneStates: the
// states that count as done when deciding whether a predecessor still blocks
// its successor (ADO-N32).
type ADODoneStates struct {
	// Categories are the state categories that count as done for any type
	// not listed in ByType. Empty means adoDefaultDoneCategories.
	Categories []string
	// ByType maps a work item type name to the state names that count as
	// done for that type. A listed type ignores Categories. Type and state
	// names compare case-insensitively, as ADO does.
	ByType map[string][]string
}

// WithADODoneStates sets the states that count as done for predecessor
// blocking. Omitting it keeps the default: Resolved, Completed and Removed.
// Names are normalized as validate reads them (normalizeADODoneStates).
func WithADODoneStates(states ADODoneStates) func(*ADOProvider) {
	normalized := normalizeADODoneStates(states)
	return func(p *ADOProvider) { p.doneStates = normalized }
}

// normalizeADODoneStates trims the category, type and state names, so a name
// written with stray whitespace matches as `goobers validate` reports it, and
// merges ByType keys that differ only in case (ADO type names are
// case-insensitive) into one lower-cased key holding the union of their
// states, so the lookup no longer depends on map iteration order.
func normalizeADODoneStates(states ADODoneStates) ADODoneStates {
	out := ADODoneStates{Categories: trimmedNonEmpty(states.Categories)}
	if len(states.ByType) == 0 {
		return out
	}
	keys := make([]string, 0, len(states.ByType))
	for key := range states.ByType {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out.ByType = make(map[string][]string, len(keys))
	for _, key := range keys {
		folded := strings.ToLower(strings.TrimSpace(key))
		for _, state := range trimmedNonEmpty(states.ByType[key]) {
			if !containsFold(out.ByType[folded], state) {
				out.ByType[folded] = append(out.ByType[folded], state)
			}
		}
		if _, ok := out.ByType[folded]; !ok {
			out.ByType[folded] = []string{}
		}
	}
	return out
}

func trimmedNonEmpty(values []string) []string {
	var out []string
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// done reports whether a predecessor of itemType in the named state and
// category no longer blocks.
func (s ADODoneStates) done(itemType, state, category string) bool {
	for typeName, states := range s.ByType {
		if strings.EqualFold(typeName, itemType) {
			return containsFold(states, state)
		}
	}
	categories := s.Categories
	if len(categories) == 0 {
		categories = adoDefaultDoneCategories
	}
	return containsFold(categories, category)
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// HasOpenWorkItemBlocker reports whether an ADO work item has a predecessor
// that is not in a done state (backlog.blockers).
func (p *ADOProvider) HasOpenWorkItemBlocker(ctx context.Context, repo RepositoryRef, id string) (bool, error) {
	blockers, err := p.ListWorkItemBlockers(ctx, repo, id)
	if err != nil {
		return false, err
	}
	for _, blocker := range blockers {
		if blocker.State == "open" {
			return true, nil
		}
	}
	return false, nil
}

// ListWorkItemBlockers returns every predecessor of an ADO work item with its
// real blocking state, matching GitHub's list (#5960): State is "open" for a
// predecessor that still blocks and "closed" for one in a done state
// (backlog.doneStates, so a Resolved Bug reads "closed" by default even
// though its state category maps to an open work item elsewhere). Callers
// that only care about live blockers filter on "open", as
// HasOpenWorkItemBlocker does. A predecessor the batch omits (deleted, or
// unreadable by this identity) is reported "open": its state cannot be
// checked, so the answer fails closed exactly as it did before the check
// existed.
func (p *ADOProvider) ListWorkItemBlockers(ctx context.Context, repo RepositoryRef, id string) ([]WorkItem, error) {
	item, err := p.getRawWorkItem(ctx, repo, id)
	if err != nil {
		return nil, err
	}
	ids, err := adoPredecessorIDs(item.Relations)
	if err != nil {
		return nil, fmt.Errorf("ADO work item %s: %w", id, err)
	}
	hydrated, err := p.getWorkItemsBatch(ctx, repo, ids)
	if err != nil {
		return nil, fmt.Errorf("read predecessors of ADO work item %s: %w", id, err)
	}
	byID := make(map[int]adoWorkItem, len(hydrated))
	for _, predecessor := range hydrated {
		byID[predecessor.ID] = predecessor
	}
	blockers := make([]WorkItem, 0, len(ids))
	for _, predecessorID := range ids {
		predecessor, ok := byID[predecessorID]
		if !ok {
			blockers = append(blockers, WorkItem{Provider: ProviderADO, ID: strconv.Itoa(predecessorID), State: "open"})
			continue
		}
		blocking, status, err := p.adoPredecessorBlocks(ctx, repo, predecessor)
		if err != nil {
			return nil, fmt.Errorf("predecessor %d of ADO work item %s: %w", predecessorID, id, err)
		}
		state := "closed"
		if blocking {
			state = "open"
		}
		blockers = append(blockers, mapADOWorkItemState(predecessor, state, status))
	}
	return blockers, nil
}

// adoPredecessorBlocks reports whether one hydrated predecessor is still
// outside the configured done states, and the work-item status its state
// category maps to. Its state categories are read for its own project, which
// a cross-project link can make differ from the successor's.
func (p *ADOProvider) adoPredecessorBlocks(ctx context.Context, repo RepositoryRef, predecessor adoWorkItem) (bool, WorkItemStatus, error) {
	itemType := stringField(predecessor.Fields, "System.WorkItemType")
	state := stringField(predecessor.Fields, "System.State")
	stateRepo := repo
	if project := stringField(predecessor.Fields, "System.TeamProject"); project != "" {
		stateRepo.Project = project
	}
	states, err := p.adoWorkItemStateCategories(ctx, stateRepo, itemType)
	if err != nil {
		return false, "", err
	}
	definition, found := findADOWorkItemState(states, state)
	if !found {
		return false, "", fmt.Errorf("ADO work item type %q has unknown state %q", itemType, state)
	}
	blocking := !p.doneStates.done(itemType, definition.Name, definition.Category)
	_, status, err := commonADOStateCategory(definition.Category)
	if err != nil {
		// The blocking verdict comes from the done states, not the common
		// mapping, so an unusual category only loses the status detail.
		status = WorkItemStatusInProgress
	}
	if !blocking {
		status = WorkItemStatusDone
	}
	return blocking, status, nil
}

// adoPredecessorIDs returns the distinct predecessor ids of a work item's
// relations in relation order.
func adoPredecessorIDs(relations []adoRelation) ([]int, error) {
	var ids []int
	seen := make(map[int]bool)
	for _, relation := range relations {
		if relation.Rel != adoPredecessorRel {
			continue
		}
		segment := lastPathSegment(relation.URL)
		predecessorID, err := strconv.Atoi(segment)
		if err != nil || predecessorID <= 0 {
			return nil, fmt.Errorf("predecessor relation has no work item id in %q", relation.URL)
		}
		if !seen[predecessorID] {
			seen[predecessorID] = true
			ids = append(ids, predecessorID)
		}
	}
	return ids, nil
}
