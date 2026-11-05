package schedcounters

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/backlogdefaults"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// pageSize bounds one eligibility read; a longer backlog is scanned across polls
// by cursor, as locally.
const pageSize = 100

// counter sizes a workflow's fan-out (or refill) to the work items the
// backlog-query stage would find eligible.
type counter struct {
	open            Opener
	quota           *localscheduler.ProviderQuotaState
	repo            providers.RepositoryRef
	labels          []string
	labelPredicate  *labelpredicate.Predicate
	fieldPredicate  *fieldpredicate.Predicate
	respectAssignee bool
	assignedTo      string

	mu     sync.Mutex
	cursor string
}

var _ localscheduler.BacklogCounter = (*counter)(nil)

// ProviderQuotaGuarded tells the scheduler this counter enforces the quota
// ledger itself, so it may still poll (and be shed by the request gate) when the
// scheduler's own reservation found the window empty.
func (c *counter) ProviderQuotaGuarded() bool { return c.quota != nil }

func (c *counter) EligibleCount(ctx context.Context) (int, error) {
	snapshot, err := c.EligibleSnapshot(ctx)
	return snapshot.Count, err
}

func (c *counter) EligibleSnapshot(ctx context.Context) (localscheduler.BacklogSnapshot, error) {
	access, done := accountFor(ctx, c.quota)
	defer done()
	provider, err := c.open(ctx, c.repo, access)
	if err != nil {
		return localscheduler.BacklogSnapshot{}, fmt.Errorf("open provider for backlog count of %s/%s: %w", c.repo.Owner, c.repo.Name, err)
	}
	c.mu.Lock()
	cursor := c.cursor
	c.mu.Unlock()
	assignee := ""
	if c.respectAssignee {
		assignee = c.assignedTo
	}
	pageInfo := &providers.ListWorkItemsPageInfo{}
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: c.repo, Labels: c.labels, State: "open", Limit: pageSize,
		CompareLabels: c.labelPredicate.Labels(), Assignee: assignee,
		Cursor: cursor, PageInfo: pageInfo, OldestFirst: true,
	})
	if err != nil {
		return localscheduler.BacklogSnapshot{}, err
	}
	c.mu.Lock()
	c.cursor = ""
	if pageInfo.HasNext {
		c.cursor = pageInfo.NextCursor
	}
	c.mu.Unlock()
	var snapshot localscheduler.BacklogSnapshot
	for _, item := range items {
		if c.respectAssignee && !item.AssigneeMatches(c.assignedTo) {
			continue
		}
		matched, err := c.labelPredicate.Matches(item.Labels)
		if err != nil {
			return localscheduler.BacklogSnapshot{}, fmt.Errorf("evaluate backlog label predicate: %w", err)
		}
		if matched {
			if matched, err = c.fieldPredicate.Matches(item.Fields); err != nil {
				return localscheduler.BacklogSnapshot{}, fmt.Errorf("evaluate backlog field predicate: %w", err)
			}
		}
		if !matched {
			continue
		}
		snapshot.Count++
		if item.ReadyAt != nil && !item.ReadyAt.IsZero() {
			readyAt := item.ReadyAt.UTC()
			if snapshot.OldestReadyAt.IsZero() || readyAt.Before(snapshot.OldestReadyAt) {
				snapshot.OldestReadyAt = readyAt
			}
		}
	}
	return snapshot, nil
}

// backlog is the fan-out counter of the workflow's first enabled backlog-item
// trigger; nil when it has none.
func (c *Counters) backlog(gaggle apiv1.Gaggle, wf *apiv1.Workflow) (*counter, error) {
	var trigger *apiv1.Trigger
	for i := range wf.Spec.Triggers {
		t := &wf.Spec.Triggers[i]
		if t.Type == apiv1.TriggerBacklogItem && (t.Enabled == nil || *t.Enabled) {
			trigger = t
			break
		}
	}
	if trigger == nil {
		return nil, nil
	}
	labels := append([]string(nil), gaggle.Spec.Backlog.Labels...)
	for key := range trigger.Selector {
		labels = append(labels, key)
	}
	labels = uniqueSorted(labels)
	predicate, err := labelpredicate.Compile(backlogdefaults.LabelPredicateConjunction(gaggle.Spec.Backlog.LabelPredicate, trigger.LabelPredicate), labels, nil)
	if err != nil {
		return nil, fmt.Errorf("workflow %q backlog label predicate: %w", wf.Name, err)
	}
	fields, err := fieldpredicate.CompileConjunction(gaggle.Spec.Backlog.FieldPredicate, trigger.FieldPredicate)
	if err != nil {
		return nil, fmt.Errorf("workflow %q backlog field predicate: %w", wf.Name, err)
	}
	repo, err := c.repoFor(gaggle.Spec.Project)
	if err != nil {
		return nil, err
	}
	return &counter{open: c.deps.Open, quota: c.deps.Quota, repo: repo, labels: labels, labelPredicate: predicate, fieldPredicate: fields}, nil
}

// refill is the eligibility counter behind readiness.desiredConcurrentRuns for a
// workflow that starts with the built-in backlog-query. It reads the stage's own
// inputs so refill sees exactly the items the stage would claim. A backlog-item
// workflow already polls, so it gets none.
func (c *Counters) refill(gaggle apiv1.Gaggle, wf *apiv1.Workflow) (*counter, error) {
	if wf.Spec.Readiness.DesiredConcurrentRuns <= 0 {
		return nil, nil
	}
	for _, trigger := range wf.Spec.Triggers {
		if trigger.Type == apiv1.TriggerBacklogItem {
			return nil, nil
		}
	}
	var task *apiv1.Task
	for i := range wf.Spec.Tasks {
		t := &wf.Spec.Tasks[i]
		if t.Name == wf.Spec.Start && t.Run != nil && len(t.Run.Command) >= 2 &&
			t.Run.Command[0] == "goobers" && t.Run.Command[1] == "backlog-query" {
			task = t
			break
		}
	}
	if task == nil {
		return nil, nil
	}
	required := append([]string(nil), gaggle.Spec.RequireLabels...)
	if configured, ok := task.Inputs["requireLabels"]; ok {
		required = splitList(configured)
	}
	required = append(required, gaggle.Spec.Backlog.Labels...)
	if trust := task.Inputs["trustLabel"]; trust != "" {
		required = append(required, trust)
	}
	excluded := append(splitList(task.Inputs["excludeLabels"]), providers.LabelClaimed)
	required, excluded = uniqueSorted(required), uniqueSorted(excluded)
	predicate, err := compileSelection(backlogdefaults.LabelPredicateConjunction(gaggle.Spec.Backlog.LabelPredicate, task.Inputs["labelPredicate"]),
		required, excluded, task.Inputs["parkLabels"], task.Inputs["filterParkLabels"])
	if err != nil {
		return nil, fmt.Errorf("workflow %q refill label predicate: %w", wf.Name, err)
	}
	fields, err := fieldpredicate.Compile(task.Inputs["fieldPredicate"])
	if err != nil {
		return nil, fmt.Errorf("workflow %q refill field predicate: %w", wf.Name, err)
	}
	assignedTo, configured := task.Inputs["assignedTo"]
	respect := task.Inputs["respectAssignee"] == "true"
	if respect && !configured {
		// The login a respectAssignee backlog-query matches is the gaggle's own
		// selfIdentity, falling back to the instance default: the same resolution
		// the local daemon applies (runtimewiring.SelfIdentitiesByGaggle), so a
		// per-gaggle override is honoured here exactly as there. Empty matches only
		// unassigned items, as it does locally.
		assignedTo = instance.EffectiveSelfIdentity(c.deps.Config, &gaggle)
	}
	repo, err := c.repoFor(gaggle.Spec.Project)
	if err != nil {
		return nil, err
	}
	return &counter{open: c.deps.Open, quota: c.deps.Quota, repo: repo, labels: required, labelPredicate: predicate, fieldPredicate: fields, respectAssignee: respect, assignedTo: assignedTo}, nil
}

// compileSelection mirrors the local backlog-query selection: park labels are
// excluded unless filterParkLabels is "false", independently of any explicit
// excludeLabels or labelPredicate policy.
func compileSelection(expression string, required, excluded []string, parkLabels, filterParkLabels string) (*labelpredicate.Predicate, error) {
	switch filterParkLabels {
	case "", "true":
		excluded = append(append([]string(nil), excluded...), splitList(parkLabels)...)
	case "false":
	default:
		return nil, fmt.Errorf("filterParkLabels must be true or false")
	}
	predicate, err := labelpredicate.Compile(expression, required, excluded)
	if err != nil {
		return nil, fmt.Errorf("invalid labelPredicate: %w", err)
	}
	return predicate, nil
}

func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func uniqueSorted(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if label != "" {
			out = append(out, label)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
