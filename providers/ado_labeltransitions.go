package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// adoUpdatesPageSize is the $top requested for one work-item updates
	// page; 200 is the documented server maximum. The walk does not rely on
	// it: a server that serves fewer is paged on by what it actually
	// returned (#5960).
	adoUpdatesPageSize = 200
	// adoMaxWorkItemRevisions is Azure DevOps' per-work-item revision cap.
	// An item whose history reaches it cannot be read as complete, so the
	// transition walk fails closed there rather than returning a truncated
	// history a readiness age could be computed from.
	adoMaxWorkItemRevisions = 10000
)

// adoWorkItemUpdates is one page of GET _apis/wit/workItems/{id}/updates.
type adoWorkItemUpdates struct {
	Value []adoWorkItemUpdate `json:"value"`
}

// adoWorkItemUpdate is one revision's field changes. Fields holds only the
// fields that revision changed.
type adoWorkItemUpdate struct {
	ID     int64                             `json:"id"`
	Rev    int                               `json:"rev"`
	Fields map[string]adoWorkItemFieldChange `json:"fields"`
}

type adoWorkItemFieldChange struct {
	OldValue interface{} `json:"oldValue"`
	NewValue interface{} `json:"newValue"`
}

// ListWorkItemLabelTransitionsForItem returns one work item's add/remove
// history for label, read from its update history: each update that changes
// System.Tags is diffed old against new, comparing the label ignoring case as
// ADO's tag namespace does. A transition's time is that update's
// System.ChangedDate (an update's revisedDate is when the revision was
// superseded, not when it was made). A history that reaches ADO's
// 10,000-revision cap, a tag change without a readable change date, or a walk
// that ends short of the item's current revision fails closed with
// ErrLabelHistoryIncomplete.
//
// The walk is anchored to the item's revision, read first: it pages on by
// the number of updates each page actually returned, until a page reaches
// that revision or the server returns an empty page. A short page is not
// taken as the end (#5960): a server capping $top below 200 would otherwise
// truncate the history, and a missed later re-add would make the item look
// ready for longer than it has been. Stopping at the anchored revision rather
// than requesting a trailing empty page keeps the common single-page item at
// two requests.
func (p *ADOProvider) ListWorkItemLabelTransitionsForItem(ctx context.Context, repo RepositoryRef, id, label string) ([]WorkItemLabelTransition, error) {
	project := p.project(repo)
	if err := p.requireWorkItemScope(project); err != nil {
		return nil, err
	}
	if err := validateADOWorkItemID(id); err != nil {
		return nil, err
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, fmt.Errorf("label is required")
	}
	current, err := p.getWorkItemRevision(ctx, project, id)
	if err != nil {
		return nil, err
	}
	transitions, err := p.walkWorkItemLabelUpdates(ctx, project, id, label, current)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(transitions, func(i, j int) bool {
		if transitions[i].OccurredAt.Equal(transitions[j].OccurredAt) {
			return transitions[i].EventID < transitions[j].EventID
		}
		return transitions[i].OccurredAt.Before(transitions[j].OccurredAt)
	})
	return transitions, nil
}

// walkWorkItemLabelUpdates pages through the item's updates until one
// reaches revision current (or the server returns an empty page) and returns
// label's transitions in update order. It fails closed when the history
// reaches ADO's revision cap or ends before revision current: either way the
// history it read is not the item's whole history.
func (p *ADOProvider) walkWorkItemLabelUpdates(ctx context.Context, project, id, label string, current int) ([]WorkItemLabelTransition, error) {
	var transitions []WorkItemLabelTransition
	lastRev := 0
	for skip := 0; ; {
		page, err := p.getWorkItemUpdatesPage(ctx, project, id, skip)
		if err != nil {
			return nil, err
		}
		for _, update := range page {
			transition, ok, err := adoLabelTransition(update, id, label)
			if err != nil {
				return nil, err
			}
			if ok {
				transitions = append(transitions, transition)
			}
			lastRev = max(lastRev, update.Rev)
		}
		skip += len(page)
		if skip >= adoMaxWorkItemRevisions {
			return nil, fmt.Errorf("%w: ADO work item %s has reached the %d-revision cap", ErrLabelHistoryIncomplete, id, adoMaxWorkItemRevisions)
		}
		if len(page) == 0 || lastRev >= current {
			break
		}
	}
	// A later revision than the one read first only means the item changed
	// during the walk, and that newer history was read too. An earlier one
	// means the server stopped answering before the history ended.
	if lastRev < current {
		return nil, fmt.Errorf("%w: ADO work item %s update history ends at revision %d, before its current revision %d", ErrLabelHistoryIncomplete, id, lastRev, current)
	}
	return transitions, nil
}

// getWorkItemRevision reads only a work item's current revision, the anchor
// the updates walk must reach to be complete.
func (p *ADOProvider) getWorkItemRevision(ctx context.Context, project, id string) (int, error) {
	endpoint, err := p.workURL(project, "workitems", id)
	if err != nil {
		return 0, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"fields": []string{"System.Id"}})
	if err != nil {
		return 0, err
	}
	var out adoWorkItem
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return 0, err
	}
	if out.Rev <= 0 {
		return 0, fmt.Errorf("%w: ADO work item %s carries no revision to anchor its update history", ErrLabelHistoryIncomplete, id)
	}
	return out.Rev, nil
}

func (p *ADOProvider) getWorkItemUpdatesPage(ctx context.Context, project, id string, skip int) ([]adoWorkItemUpdate, error) {
	endpoint, err := p.workURL(project, "workItems", id, "updates")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"$top":  []string{strconv.Itoa(adoUpdatesPageSize)},
		"$skip": []string{strconv.Itoa(skip)},
	})
	if err != nil {
		return nil, err
	}
	var out adoWorkItemUpdates
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	return out.Value, nil
}

// adoLabelTransition reports whether update adds or removes label from the
// work item's System.Tags and, if so, the transition it makes.
func adoLabelTransition(update adoWorkItemUpdate, id, label string) (WorkItemLabelTransition, bool, error) {
	tags, ok := update.Fields["System.Tags"]
	if !ok {
		return WorkItemLabelTransition{}, false, nil
	}
	had := adoHasLabel(adoLabels(adoFieldString(tags.OldValue)), label)
	has := adoHasLabel(adoLabels(adoFieldString(tags.NewValue)), label)
	if had == has {
		return WorkItemLabelTransition{}, false, nil
	}
	changed, err := adoUpdateChangedDate(update)
	if err != nil {
		return WorkItemLabelTransition{}, false, fmt.Errorf("%w: ADO work item %s update %d: %w", ErrLabelHistoryIncomplete, id, update.ID, err)
	}
	return WorkItemLabelTransition{
		EventID:    update.ID,
		ItemID:     id,
		Label:      label,
		Added:      has,
		OccurredAt: changed,
	}, true, nil
}

// adoUpdateChangedDate returns the time an update was made, from its
// System.ChangedDate new value. A tag-changing update without one fails
// rather than yielding a zero time a readiness age would misread.
func adoUpdateChangedDate(update adoWorkItemUpdate) (time.Time, error) {
	change, ok := update.Fields["System.ChangedDate"]
	raw := adoFieldString(change.NewValue)
	if !ok || raw == "" {
		return time.Time{}, fmt.Errorf("tag change carries no System.ChangedDate")
	}
	changed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse System.ChangedDate %q: %w", raw, err)
	}
	return changed, nil
}

func adoFieldString(value interface{}) string {
	s, _ := value.(string)
	return s
}
