package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/providers"
)

const backlogAssignmentHelp = "Usage: goobers backlog-assignment [path]\n\n" +
	"Assign eligible, open, unassigned backlog items using the configured\n" +
	"strategy and JSON roster. Supported strategies are constant-cap and\n" +
	"round-robin. The trustLabel input and a valid, non-empty roster are\n" +
	"required; invalid configuration fails before any provider mutation.\n\n" +
	"Exit codes: 0 = assignment pass completed, 1 = configuration/provider/IO\n" +
	"error, 2 = usage error.\n"

const (
	assignmentStrategyConstantCap = "constant-cap"
	assignmentStrategyRoundRobin  = "round-robin"
	defaultAssignmentMaxItems     = 20
)

type assignmentRosterEntry struct {
	Assignee string `json:"assignee"`
	MaxOpen  int    `json:"maxOpen,omitempty"`
}

type assignmentPlanEntry struct {
	ItemID   string `json:"itemId"`
	Assignee string `json:"assignee"`
}

type backlogAssignmentReport struct {
	Strategy    string                `json:"strategy"`
	Eligible    int                   `json:"eligible"`
	Unassigned  int                   `json:"unassigned"`
	Assignments []assignmentPlanEntry `json:"assignments"`
	NoWork      bool                  `json:"noWork,omitempty"`
}

func runBacklogAssignment(args []string, stdout, stderr io.Writer) int {
	return runBacklogAssignmentWithMutationHook(args, stdout, stderr, nil)
}

func runBacklogAssignmentWithMutationHook(
	args []string,
	stdout, stderr io.Writer,
	beforeMutation func(assignmentPlanEntry),
) int {
	fs := newCLIFlagSet("backlog-assignment", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "backlog-assignment")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	strategy := providerInput("strategy", "")
	roster, err := parseAssignmentRoster(strategy, providerInput("roster", ""))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	trustLabel := strings.TrimSpace(providerInput("trustLabel", ""))
	if trustLabel == "" {
		pln(stderr, "error: trustLabel is required for backlog assignment (SEC-047)")
		return 1
	}
	maxItems, err := parseAssignmentMaxItems(providerInput("maxItems", ""))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	requireLabels := splitLabelList(providerInput("requireLabels", ""))
	excludeLabels := splitLabelList(providerInput("excludeLabels", ""))
	labelFilter, err := labelpredicate.Compile(providerInput("labelPredicate", ""), requireLabels, excludeLabels)
	if err != nil {
		pf(stderr, "error: invalid labelPredicate: %v\n", err)
		return 1
	}
	fieldFilter, err := fieldpredicate.Compile(providerInput("fieldPredicate", ""))
	if err != nil {
		pf(stderr, "error: invalid fieldPredicate: %v\n", err)
		return 1
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	assigner, err := assignmentProvider(root, repo)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	backlogRepo := backlogRepoRefForStage(root, repo)

	labels := append([]string{trustLabel}, labelFilter.RequiredLabels()...)
	ctx, cancel := providerCommandContext()
	defer cancel()
	items, _, err := listBacklogScanWindow(
		ctx, assigner, backlogRepo, labels, "", fieldFilter, 0, backlogScanCursor{}, true,
	)
	if err != nil {
		return failProviderStage(stderr, "list backlog for assignment", err, "backlog-assignment.json")
	}
	allFields, err := fieldpredicate.Compile("")
	if err != nil {
		pf(stderr, "error: compile assignment load filter: %v\n", err)
		return 1
	}
	scopedItems, _, err := listBacklogScanWindow(
		ctx, assigner, backlogRepo, nil, "", allFields, 0, backlogScanCursor{}, true,
	)
	if err != nil {
		return failProviderStage(stderr, "list backlog assignment load", err, "backlog-assignment.json")
	}
	eligible, err := assignmentEligibleItems(items, trustLabel, labelFilter, fieldFilter)
	if err != nil {
		pf(stderr, "error: filter backlog for assignment: %v\n", err)
		return 1
	}
	plan := planBacklogAssignments(strategy, roster, eligible, scopedItems, maxItems)
	applied := make([]assignmentPlanEntry, 0, len(plan))
	concurrentlyAssigned := 0
	for _, assignment := range plan {
		if beforeMutation != nil {
			beforeMutation(assignment)
		}
		current, err := assigner.GetWorkItem(ctx, backlogRepo, assignment.ItemID)
		if err != nil {
			return failProviderStage(stderr, "recheck backlog item "+assignment.ItemID, err, "backlog-assignment.json")
		}
		if current.Assignee != "" {
			concurrentlyAssigned++
			continue
		}
		assignee := assignment.Assignee
		if _, err := assigner.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: backlogRepo,
			ID:         assignment.ItemID,
			Assignee:   &assignee,
		}); err != nil {
			return failProviderStage(stderr, "assign backlog item "+assignment.ItemID, err, "backlog-assignment.json")
		}
		applied = append(applied, assignment)
	}

	unassigned := 0
	for _, item := range eligible {
		if item.Assignee == "" {
			unassigned++
		}
	}
	report := backlogAssignmentReport{
		Strategy:    strategy,
		Eligible:    len(eligible),
		Unassigned:  unassigned - len(applied) - concurrentlyAssigned,
		Assignments: applied,
		NoWork:      len(applied) == 0,
	}
	data, err := json.Marshal(report)
	if err != nil {
		pf(stderr, "error: marshal assignment report: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "backlog-assignment.json")
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "assigned %d backlog item(s); %d remain unassigned\n", len(applied), report.Unassigned)
	return 0
}

func parseAssignmentRoster(strategy, raw string) ([]assignmentRosterEntry, error) {
	if strategy != assignmentStrategyConstantCap && strategy != assignmentStrategyRoundRobin {
		return nil, fmt.Errorf("strategy must be %q or %q", assignmentStrategyConstantCap, assignmentStrategyRoundRobin)
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var roster []assignmentRosterEntry
	if err := decoder.Decode(&roster); err != nil {
		return nil, fmt.Errorf("invalid roster JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("invalid roster JSON: trailing content")
	}
	if len(roster) == 0 {
		return nil, fmt.Errorf("roster must contain at least one assignee")
	}
	seen := make(map[string]bool, len(roster))
	for i := range roster {
		roster[i].Assignee = strings.TrimSpace(roster[i].Assignee)
		if roster[i].Assignee == "" {
			return nil, fmt.Errorf("roster entry %d has an empty assignee", i)
		}
		if seen[roster[i].Assignee] {
			return nil, fmt.Errorf("roster contains duplicate assignee %q", roster[i].Assignee)
		}
		seen[roster[i].Assignee] = true
		switch strategy {
		case assignmentStrategyConstantCap:
			if roster[i].MaxOpen < 1 {
				return nil, fmt.Errorf("roster entry %q requires a positive maxOpen for constant-cap", roster[i].Assignee)
			}
		case assignmentStrategyRoundRobin:
			if roster[i].MaxOpen != 0 {
				return nil, fmt.Errorf("roster entry %q must omit maxOpen for round-robin", roster[i].Assignee)
			}
		}
	}
	return roster, nil
}

func parseAssignmentMaxItems(raw string) (int, error) {
	if raw == "" {
		return defaultAssignmentMaxItems, nil
	}
	maxItems, err := strconv.Atoi(raw)
	if err != nil || maxItems < 1 {
		return 0, fmt.Errorf("maxItems must be a positive integer")
	}
	return maxItems, nil
}

func assignmentProvider(root string, repo providers.RepositoryRef) (providers.BacklogProvider, error) {
	return newProviderForStage(root, repo, false, withStageProviderCache(), withStageProviderMutations("issue"))
}

func assignmentEligibleItems(
	items []providers.WorkItem,
	trustLabel string,
	labelFilter *labelpredicate.Predicate,
	fieldFilter *fieldpredicate.Predicate,
) ([]providers.WorkItem, error) {
	eligible := make([]providers.WorkItem, 0, len(items))
	for _, item := range items {
		if !item.HasLabel(trustLabel) || (item.State != "" && !strings.EqualFold(item.State, "open")) {
			continue
		}
		matched, err := labelFilter.Matches(item.Labels)
		if err != nil {
			return nil, fmt.Errorf("evaluate labelPredicate for item %s: %w", item.ID, err)
		}
		if !matched {
			continue
		}
		matched, err = fieldFilter.Matches(item.Fields)
		if err != nil {
			return nil, fmt.Errorf("evaluate fieldPredicate for item %s: %w", item.ID, err)
		}
		if matched {
			eligible = append(eligible, item)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		left, right := eligible[i], eligible[j]
		if left.CreatedAt != nil && right.CreatedAt != nil && !left.CreatedAt.Equal(*right.CreatedAt) {
			return left.CreatedAt.Before(*right.CreatedAt)
		}
		leftID, leftErr := strconv.ParseInt(left.ID, 10, 64)
		rightID, rightErr := strconv.ParseInt(right.ID, 10, 64)
		if leftErr == nil && rightErr == nil {
			return leftID < rightID
		}
		return left.ID < right.ID
	})
	return eligible, nil
}

func planBacklogAssignments(
	strategy string,
	roster []assignmentRosterEntry,
	eligible []providers.WorkItem,
	scopedItems []providers.WorkItem,
	maxItems int,
) []assignmentPlanEntry {
	load := make([]int, len(roster))
	indexByAssignee := make(map[string]int, len(roster))
	for i, entry := range roster {
		indexByAssignee[entry.Assignee] = i
	}
	for _, item := range scopedItems {
		if i, ok := indexByAssignee[item.Assignee]; ok {
			load[i]++
		}
	}
	var unassigned []providers.WorkItem
	for _, item := range eligible {
		if item.Assignee == "" {
			unassigned = append(unassigned, item)
		}
	}

	plan := make([]assignmentPlanEntry, 0, min(maxItems, len(unassigned)))
	for _, item := range unassigned {
		if len(plan) == maxItems {
			break
		}
		selected := -1
		switch strategy {
		case assignmentStrategyConstantCap:
			for i := range roster {
				if load[i] < roster[i].MaxOpen {
					selected = i
					break
				}
			}
		case assignmentStrategyRoundRobin:
			selected = 0
			for i := 1; i < len(roster); i++ {
				if load[i] < load[selected] {
					selected = i
				}
			}
		}
		if selected < 0 {
			break
		}
		plan = append(plan, assignmentPlanEntry{ItemID: item.ID, Assignee: roster[selected].Assignee})
		load[selected]++
	}
	return plan
}
