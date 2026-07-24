package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

const backlogHealthHelp = "Usage: goobers backlog-health [path]\n\n" +
	"Snapshot ready-pool depth and age from provider label-event timestamps, and\n" +
	"persist the paginated ready-transition ledger for telemetry rollups. Exit\n" +
	"codes: 0 = OK, 1 = provider/IO error, 2 = usage error.\n"

type backlogHealthReport struct {
	ReadyPoolDepth         int                                 `json:"readyPoolDepth"`
	AverageReadyAgeSeconds float64                             `json:"averageReadyAgeSeconds"`
	OldestReadyAgeSeconds  float64                             `json:"oldestReadyAgeSeconds"`
	ReadyPoolStarved       bool                                `json:"readyPoolStarved"`
	ReadyPoolObservedAt    string                              `json:"readyPoolObservedAt"`
	ReadyTransitions       []providers.WorkItemLabelTransition `json:"readyTransitions,omitempty"`
}

func runBacklogHealth(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backlog-health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "backlog-health")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	trustLabel := providerInput("trustLabel", "")
	readyLabel := providerInput("readyLabel", "goobers:ready")
	var labels []string
	if trustLabel != "" {
		labels = []string{trustLabel}
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	issueProvider := newCachedGitHubProvider(root, token)
	items, err := issueProvider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repo,
		Labels:     labels,
		State:      "all",
	})
	if err != nil {
		return failProviderStage(stderr, "snapshot ready backlog", err, "backlog-health.json")
	}
	transitions, err := issueProvider.ListWorkItemLabelTransitions(ctx, repo, readyLabel)
	if err != nil {
		return failProviderStage(stderr, "read ready-label transitions", err, "backlog-health.json")
	}
	transitions = transitionsForItems(transitions, items)
	if err := annotateReadyTimes(items, readyLabel, transitions); err != nil {
		pf(stderr, "error: snapshot ready backlog: %v\n", err)
		return 1
	}

	observedAt := time.Now().UTC()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		pf(stderr, "error: inspect ready-pool claims: %v\n", err)
		return 1
	}
	items = unclaimedReadyItems(items, ledger, providerGaggle(), string(repo.Provider), observedAt)
	report := measureReadyPool(items, readyLabel, observedAt)
	report.ReadyTransitions = transitions
	data, err := json.Marshal(report)
	if err != nil {
		pf(stderr, "error: marshal backlog health: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "backlog-health.json")
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "ready pool: %d items, oldest age %.0fs\n", report.ReadyPoolDepth, report.OldestReadyAgeSeconds)
	return 0
}

func measureReadyPool(items []providers.WorkItem, readyLabel string, observedAt time.Time) backlogHealthReport {
	report := backlogHealthReport{ReadyPoolObservedAt: observedAt.UTC().Format(time.RFC3339Nano)}
	var totalAge float64
	for _, item := range items {
		if !item.HasLabel(readyLabel) || (item.State != "" && !strings.EqualFold(item.State, "open")) {
			continue
		}
		age := float64(0)
		if item.ReadyAt != nil && observedAt.After(*item.ReadyAt) {
			age = observedAt.Sub(*item.ReadyAt).Seconds()
		}
		report.ReadyPoolDepth++
		totalAge += age
		if age > report.OldestReadyAgeSeconds {
			report.OldestReadyAgeSeconds = age
		}
	}
	report.ReadyPoolStarved = report.ReadyPoolDepth == 0
	if report.ReadyPoolDepth > 0 {
		report.AverageReadyAgeSeconds = totalAge / float64(report.ReadyPoolDepth)
	}
	return report
}

func transitionsForItems(
	transitions []providers.WorkItemLabelTransition,
	items []providers.WorkItem,
) []providers.WorkItemLabelTransition {
	ids := make(map[string]bool, len(items))
	for _, item := range items {
		ids[item.ID] = true
	}
	filtered := make([]providers.WorkItemLabelTransition, 0, len(transitions))
	for _, transition := range transitions {
		if ids[transition.ItemID] {
			filtered = append(filtered, transition)
		}
	}
	return filtered
}

func annotateReadyTimes(
	items []providers.WorkItem,
	readyLabel string,
	transitions []providers.WorkItemLabelTransition,
) error {
	ordered := append([]providers.WorkItemLabelTransition(nil), transitions...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].OccurredAt.Equal(ordered[j].OccurredAt) {
			return ordered[i].EventID < ordered[j].EventID
		}
		return ordered[i].OccurredAt.Before(ordered[j].OccurredAt)
	})
	active := make(map[string]time.Time)
	for _, transition := range ordered {
		if transition.Label != readyLabel {
			continue
		}
		if transition.Added {
			active[transition.ItemID] = transition.OccurredAt
		} else {
			delete(active, transition.ItemID)
		}
	}
	for i := range items {
		if !items[i].HasLabel(readyLabel) {
			continue
		}
		readyAt, ok := active[items[i].ID]
		if !ok {
			return fmt.Errorf("issue %s has %q but no active label-add event", items[i].ID, readyLabel)
		}
		items[i].ReadyAt = &readyAt
	}
	return nil
}

func unclaimedReadyItems(
	items []providers.WorkItem,
	ledger *localscheduler.ClaimLedger,
	gaggle, provider string,
	observedAt time.Time,
) []providers.WorkItem {
	if ledger == nil {
		return items
	}
	available := items[:0]
	for _, item := range items {
		var (
			entry localscheduler.ClaimEntry
			ok    bool
		)
		if gaggle == "" {
			entry, ok = ledger.Lookup(item.ID)
		} else {
			entry, ok = ledger.LookupScoped(localscheduler.ClaimKey{
				Gaggle: gaggle, Provider: provider, ExternalID: item.ID,
			})
		}
		if ok && entry.ExpiresAt.After(observedAt) {
			continue
		}
		available = append(available, item)
	}
	return available
}
