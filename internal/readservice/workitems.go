package readservice

import (
	"context"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const MaxWorkItemsPageSize = 200

type workItemStore interface {
	WorkItems(context.Context, rollup.WorkItemQuery) ([]rollup.WorkItem, bool, error)
	WorkItemActions(context.Context, string, string, string, string) ([]rollup.WorkItemAction, bool, error)
	RelatedPullRequests(context.Context, string, string, string) ([]rollup.RelatedWorkItem, error)
}

type WorkItemListOptions struct {
	Provider string
	Kind     string
	Limit    int
}

type WorkItemPage struct {
	Items   []WorkItemSummary `json:"items"`
	HasMore bool              `json:"hasMore"`
}

type WorkItemSummary struct {
	Provider      string    `json:"provider"`
	Repository    string    `json:"repository,omitempty"`
	Kind          string    `json:"kind"`
	ExternalID    string    `json:"externalId"`
	URL           string    `json:"url,omitempty"`
	ActionCount   int       `json:"actionCount"`
	LastOperation string    `json:"lastOperation"`
	LastActionAt  time.Time `json:"lastActionAt"`
	LastRunID     string    `json:"lastRunId"`
	Gaggle        string    `json:"gaggle,omitempty"`
	Workflow      string    `json:"workflow,omitempty"`
	RunStatus     string    `json:"runStatus,omitempty"`
}

type WorkItemDetail struct {
	Provider            string            `json:"provider"`
	Repository          string            `json:"repository,omitempty"`
	Kind                string            `json:"kind"`
	ExternalID          string            `json:"externalId"`
	URL                 string            `json:"url,omitempty"`
	Cost                *WorkItemCost     `json:"cost,omitempty"`
	RelatedPullRequests []RelatedWorkItem `json:"relatedPullRequests"`
	Actions             []WorkItemAction  `json:"actions"`
	Truncated           bool              `json:"truncated"`
}

type WorkItemCost struct {
	CostUSD          *float64 `json:"costUSD,omitempty"`
	NanoAIU          *int64   `json:"nanoAIU,omitempty"`
	TotalRuns        int      `json:"totalRuns"`
	MeasuredRuns     int      `json:"measuredRuns"`
	TotalAttempts    int      `json:"totalAttempts"`
	MeasuredAttempts int      `json:"measuredAttempts"`
	LowerBound       bool     `json:"lowerBound"`
}

type RelatedWorkItem struct {
	Provider   string `json:"provider"`
	Repository string `json:"repository,omitempty"`
	Kind       string `json:"kind"`
	ExternalID string `json:"externalId"`
	URL        string `json:"url,omitempty"`
}

type WorkItemAction struct {
	RunID      string    `json:"runId"`
	Sequence   uint64    `json:"sequence"`
	URL        string    `json:"url,omitempty"`
	Operation  string    `json:"operation"`
	OccurredAt time.Time `json:"occurredAt"`
	Gaggle     string    `json:"gaggle,omitempty"`
	Workflow   string    `json:"workflow,omitempty"`
	RunStatus  string    `json:"runStatus,omitempty"`
}

func (s *Telemetry) WorkItems(ctx context.Context, options WorkItemListOptions) (WorkItemPage, error) {
	options.Provider = strings.TrimSpace(options.Provider)
	options.Kind = strings.TrimSpace(options.Kind)
	if options.Kind != "" && options.Kind != "pr" && options.Kind != "issue" {
		return WorkItemPage{}, ErrInvalidTelemetryRequest
	}
	if options.Limit <= 0 {
		options.Limit = 100
	}
	if options.Limit > MaxWorkItemsPageSize {
		return WorkItemPage{}, ErrInvalidTelemetryRequest
	}
	store, ok := s.store.(workItemStore)
	if !ok {
		return WorkItemPage{}, ErrTelemetryUnavailable
	}
	items, hasMore, err := store.WorkItems(ctx, rollup.WorkItemQuery{
		Provider: options.Provider,
		Kind:     options.Kind,
		Limit:    options.Limit,
	})
	if err != nil {
		return WorkItemPage{}, err
	}
	result := WorkItemPage{Items: make([]WorkItemSummary, 0, len(items)), HasMore: hasMore}
	for _, item := range items {
		result.Items = append(result.Items, WorkItemSummary{
			Provider: item.Provider, Repository: item.Repository, Kind: item.Kind, ExternalID: item.ExternalID,
			URL: item.URL, ActionCount: item.ActionCount, LastOperation: item.LastOperation,
			LastActionAt: item.LastActionAt, LastRunID: item.LastRunID, Gaggle: item.Gaggle,
			Workflow: item.Workflow, RunStatus: item.RunStatus,
		})
	}
	return result, nil
}

func (s *Telemetry) WorkItem(
	ctx context.Context,
	provider string,
	repository string,
	kind string,
	externalID string,
) (WorkItemDetail, error) {
	provider = strings.TrimSpace(provider)
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	kind = strings.TrimSpace(kind)
	externalID = strings.TrimSpace(externalID)
	if provider == "" || repository == "" || externalID == "" || (kind != "pr" && kind != "issue") {
		return WorkItemDetail{}, ErrInvalidTelemetryRequest
	}
	store, ok := s.store.(workItemStore)
	if !ok {
		return WorkItemDetail{}, ErrTelemetryUnavailable
	}
	actions, truncated, err := store.WorkItemActions(ctx, provider, repository, kind, externalID)
	if err != nil {
		return WorkItemDetail{}, err
	}
	if len(actions) == 0 {
		return WorkItemDetail{}, ErrWorkItemNotFound
	}
	result := WorkItemDetail{
		Provider: provider, Repository: repository, Kind: kind, ExternalID: externalID,
		URL: actions[0].URL, Actions: make([]WorkItemAction, 0, len(actions)), Truncated: truncated,
		RelatedPullRequests: []RelatedWorkItem{},
	}
	for _, action := range actions {
		if result.URL == "" && action.URL != "" {
			result.URL = action.URL
		}
		result.Actions = append(result.Actions, WorkItemAction{
			RunID: action.RunID, Sequence: action.Seq, URL: action.URL,
			Operation: action.Operation, OccurredAt: action.OccurredAt,
			Gaggle: action.Gaggle, Workflow: action.Workflow, RunStatus: action.RunStatus,
		})
	}
	costs, err := s.store.CostAggregates(ctx, rollup.CostQuery{
		Provider: provider, ExternalKind: kind, ExternalID: externalID,
	})
	if err != nil {
		return WorkItemDetail{}, err
	}
	var aggregates []rollup.CostAggregate
	if kind == "pr" {
		aggregates = costs.PullRequests
	} else {
		aggregates = costs.Issues
	}
	if len(aggregates) == 1 {
		cost := aggregates[0]
		result.Cost = &WorkItemCost{
			CostUSD: cost.CostUSD, NanoAIU: cost.NanoAIU,
			TotalRuns: cost.TotalRuns, MeasuredRuns: cost.MeasuredRuns,
			TotalAttempts: cost.TotalAttempts, MeasuredAttempts: cost.MeasuredAttempts,
			LowerBound: cost.MeasuredRuns < cost.TotalRuns || cost.MeasuredAttempts < cost.TotalAttempts,
		}
	}
	if kind == "issue" {
		related, err := store.RelatedPullRequests(ctx, provider, repository, externalID)
		if err != nil {
			return WorkItemDetail{}, err
		}
		for _, item := range related {
			result.RelatedPullRequests = append(result.RelatedPullRequests, RelatedWorkItem{
				Provider: item.Provider, Repository: item.Repository, Kind: item.Kind,
				ExternalID: item.ExternalID, URL: item.URL,
			})
		}
	}
	return result, nil
}

func (s *Local) WorkItems(ctx context.Context, options WorkItemListOptions) (WorkItemPage, error) {
	if s.telemetry == nil {
		return WorkItemPage{}, ErrTelemetryUnavailable
	}
	return s.telemetry.WorkItems(ctx, options)
}

func (s *Local) WorkItem(ctx context.Context, provider, repository, kind, externalID string) (WorkItemDetail, error) {
	if s.telemetry == nil {
		return WorkItemDetail{}, ErrTelemetryUnavailable
	}
	return s.telemetry.WorkItem(ctx, provider, repository, kind, externalID)
}
