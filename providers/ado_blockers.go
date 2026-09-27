package providers

import (
	"context"
	"fmt"
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
func WithADODoneStates(states ADODoneStates) func(*ADOProvider) {
	return func(p *ADOProvider) { p.doneStates = states }
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
	blockers, err := p.openADOPredecessors(ctx, repo, id)
	if err != nil {
		return false, err
	}
	return len(blockers) > 0, nil
}

// ListWorkItemBlockers returns the predecessors of an ADO work item that
// still block it. Unlike GitHub's list, a predecessor in a done state is left
// out, and each one returned is reported with State "open" whatever its
// native category, because "still blocking" is what the caller asks about.
func (p *ADOProvider) ListWorkItemBlockers(ctx context.Context, repo RepositoryRef, id string) ([]WorkItem, error) {
	return p.openADOPredecessors(ctx, repo, id)
}

// openADOPredecessors reads the item's Dependency-Reverse relations,
// hydrates the predecessors in one workitemsbatch call per 200 ids, and
// returns the ones that still block. A predecessor the batch omits (deleted,
// or unreadable by this identity) still blocks: its state cannot be checked,
// so the answer fails closed exactly as it did before the check existed.
func (p *ADOProvider) openADOPredecessors(ctx context.Context, repo RepositoryRef, id string) ([]WorkItem, error) {
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
	var open []WorkItem
	for _, predecessorID := range ids {
		predecessor, ok := byID[predecessorID]
		if !ok {
			open = append(open, WorkItem{Provider: ProviderADO, ID: strconv.Itoa(predecessorID), State: "open"})
			continue
		}
		blocking, err := p.adoPredecessorBlocks(ctx, repo, predecessor)
		if err != nil {
			return nil, fmt.Errorf("predecessor %d of ADO work item %s: %w", predecessorID, id, err)
		}
		if blocking {
			mapped := mapADOWorkItemState(predecessor, "open", WorkItemStatusInProgress)
			open = append(open, mapped)
		}
	}
	return open, nil
}

// adoPredecessorBlocks reports whether one hydrated predecessor is still
// outside the configured done states. Its state categories are read for its
// own project, which a cross-project link can make differ from the
// successor's.
func (p *ADOProvider) adoPredecessorBlocks(ctx context.Context, repo RepositoryRef, predecessor adoWorkItem) (bool, error) {
	itemType := stringField(predecessor.Fields, "System.WorkItemType")
	state := stringField(predecessor.Fields, "System.State")
	stateRepo := repo
	if project := stringField(predecessor.Fields, "System.TeamProject"); project != "" {
		stateRepo.Project = project
	}
	states, err := p.adoWorkItemStateCategories(ctx, stateRepo, itemType)
	if err != nil {
		return false, err
	}
	definition, found := findADOWorkItemState(states, state)
	if !found {
		return false, fmt.Errorf("ADO work item type %q has unknown state %q", itemType, state)
	}
	return !p.doneStates.done(itemType, definition.Name, definition.Category), nil
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
