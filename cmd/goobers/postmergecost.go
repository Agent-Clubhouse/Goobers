package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

func mergedPullRequestComment(ctx context.Context, root string, repo providers.RepositoryRef, pullNumber string) (string, error) {
	base := fmt.Sprintf("Merged in pull request #%s.", pullNumber)
	if root == "" {
		return base, nil
	}

	db, err := openRollup(instance.NewLayout(root), false)
	if err != nil {
		return base, fmt.Errorf("open cost rollup: %w", err)
	}
	defer func() { _ = db.Close() }()

	runID := os.Getenv(executor.RunIDEnvVar)
	if runID != "" {
		runDir := filepath.Join(instance.NewLayout(root).ForGaggle(os.Getenv(executor.GaggleEnvVar)).RunsDir(), runID)
		if err := db.IngestRun(ctx, runDir); err != nil {
			return base, fmt.Errorf("ingest merge run cost: %w", err)
		}
	}
	aggregates, err := db.PullRequestCosts(ctx, string(repo.Provider))
	if err != nil {
		return base, fmt.Errorf("query merged pull-request cost: %w", err)
	}
	for _, aggregate := range aggregates {
		if aggregate.ExternalID != pullNumber {
			continue
		}
		total := formatMergedPullRequestCost(aggregate)
		if total == "" {
			return base, nil
		}
		if aggregate.MeasuredRuns < aggregate.TotalRuns {
			total += fmt.Sprintf(" (lower bound; %d/%d runs measured)", aggregate.MeasuredRuns, aggregate.TotalRuns)
		}
		return base + "\n\n**Total Goobers cost for this PR:** " + total, nil
	}
	return base, nil
}

func formatMergedPullRequestCost(aggregate rollup.CostAggregate) string {
	if aggregate.NanoAIU != nil {
		value := strconv.FormatFloat(float64(*aggregate.NanoAIU)/1e9, 'f', 2, 64) + " AI credits"
		if aggregate.CostUSD != nil {
			value += " (~$" + strconv.FormatFloat(*aggregate.CostUSD, 'f', 2, 64) + ")"
		}
		return value
	}
	if aggregate.CostUSD != nil {
		return "$" + strconv.FormatFloat(*aggregate.CostUSD, 'f', 2, 64) + " estimated"
	}
	if aggregate.CopilotPremiumRequests != nil {
		return strconv.FormatFloat(*aggregate.CopilotPremiumRequests, 'f', 2, 64) + " premium requests"
	}
	return ""
}
