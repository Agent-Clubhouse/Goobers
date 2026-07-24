package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

const backlogHealthHelp = "Usage: goobers backlog-health [path]\n\n" +
	"Snapshot the current ready backlog depth and age into a flat stage result\n" +
	"for telemetry rollups. Exit codes: 0 = OK, 1 = provider/IO error, 2 = usage error.\n"

type backlogHealthReport struct {
	ReadyPoolDepth         int     `json:"readyPoolDepth"`
	AverageReadyAgeSeconds float64 `json:"averageReadyAgeSeconds"`
	OldestReadyAgeSeconds  float64 `json:"oldestReadyAgeSeconds"`
	ReadyPoolStarved       bool    `json:"readyPoolStarved"`
	ReadyPoolObservedAt    string  `json:"readyPoolObservedAt"`
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
	labels := []string{readyLabel}
	if trustLabel != "" {
		labels = append([]string{trustLabel}, labels...)
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	items, err := newCachedGitHubProvider(root, token).ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repo,
		Labels:     labels,
		State:      "open",
	})
	if err != nil {
		return failProviderStage(stderr, "snapshot ready backlog", err, "backlog-health.json")
	}

	observedAt := time.Now().UTC()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		pf(stderr, "error: inspect ready-pool claims: %v\n", err)
		return 1
	}
	items = unclaimedReadyItems(items, ledger, providerGaggle(), string(repo.Provider), observedAt)
	report := measureReadyPool(items, readyLabel, observedAt)
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
		readyAt := item.UpdatedAt
		if readyAt == nil {
			readyAt = item.CreatedAt
		}
		age := float64(0)
		if readyAt != nil && observedAt.After(*readyAt) {
			age = observedAt.Sub(*readyAt).Seconds()
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
