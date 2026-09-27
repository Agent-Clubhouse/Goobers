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
	// adoUpdatesPageSize is the $top of one work-item updates page; 200 is
	// the largest page the updates API serves.
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
// 10,000-revision cap fails closed.
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
	var transitions []WorkItemLabelTransition
	for skip := 0; ; skip += adoUpdatesPageSize {
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
		}
		if skip+len(page) >= adoMaxWorkItemRevisions {
			return nil, fmt.Errorf("ADO work item %s has reached the %d-revision cap; its label history cannot be read completely", id, adoMaxWorkItemRevisions)
		}
		if len(page) < adoUpdatesPageSize {
			break
		}
	}
	sort.SliceStable(transitions, func(i, j int) bool {
		if transitions[i].OccurredAt.Equal(transitions[j].OccurredAt) {
			return transitions[i].EventID < transitions[j].EventID
		}
		return transitions[i].OccurredAt.Before(transitions[j].OccurredAt)
	})
	return transitions, nil
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
		return WorkItemLabelTransition{}, false, fmt.Errorf("ADO work item %s update %d: %w", id, update.ID, err)
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
