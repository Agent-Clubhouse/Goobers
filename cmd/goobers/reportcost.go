package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

const (
	costReportMarker     = "<!-- goobers:cost:v1 -->"
	costReportResultFile = "cost-report-result.json"
)

const reportCostHelp = "Usage: goobers report-cost [path]\n\n" +
	"Publish one versioned sticky cost report on each pull request and addressed\n" +
	"issue attributed to this run. Reports aggregate all known runs, preserve\n" +
	"unmeasured versus measured-zero values, and disclose incomplete coverage.\n\n" +
	"Publication defaults on. instance.yaml cost.enabled sets the default and a\n" +
	"gaggle's spec.cost.enabled overrides it. The stage requires the explicit\n" +
	"github:issues:write credential capability.\n"

func runReportCost(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("report-cost", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "report-cost")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	enabled, err := effectiveCostReporting(root, os.Getenv(executor.GaggleEnvVar))
	if err != nil {
		return failProviderStage(stderr, "resolve cost configuration", err, costReportResultFile)
	}
	if !enabled {
		if err := writeProviderStageResult(providerInput("resultFile", costReportResultFile), map[string]interface{}{
			"enabled": false, "published": 0,
		}); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		pln(stdout, "cost reporting disabled")
		return 0
	}

	runID := strings.TrimSpace(os.Getenv(executor.RunIDEnvVar))
	if runID == "" {
		return failProviderStage(stderr, "report cost", fmt.Errorf("%s is not set", executor.RunIDEnvVar), costReportResultFile)
	}
	repo, err := providerRepo(root)
	if err != nil {
		return failProviderStage(stderr, "resolve repository", err, costReportResultFile)
	}
	ctx, cancel := providerCommandContext()
	defer cancel()

	db, err := openRollup(instance.NewLayout(root), false)
	if err != nil {
		return failProviderStage(stderr, "open cost rollup", err, costReportResultFile)
	}
	defer func() { _ = db.Close() }()
	runDir := filepath.Join(instance.NewLayout(root).ForGaggle(os.Getenv(executor.GaggleEnvVar)).RunsDir(), runID)
	if err := db.IngestRun(ctx, runDir); err != nil {
		return failProviderStage(stderr, "ingest current run cost", err, costReportResultFile)
	}

	prIDs, issueIDs, err := db.CostTargetsForRun(ctx, string(repo.Provider), runID)
	if err != nil {
		return failProviderStage(stderr, "query cost targets", err, costReportResultFile)
	}
	prCosts, err := db.PullRequestCosts(ctx, string(repo.Provider))
	if err != nil {
		return failProviderStage(stderr, "query pull-request costs", err, costReportResultFile)
	}
	issueCosts, err := db.IssueCosts(ctx, string(repo.Provider))
	if err != nil {
		return failProviderStage(stderr, "query issue costs", err, costReportResultFile)
	}

	provider, err := newProviderForStage(root, repo, false,
		withStageProviderCapability(capability.GitHubIssuesWrite),
		withStageProviderMutations("cost-report"),
	)
	if err != nil {
		return failProviderStage(stderr, "construct report provider", err, costReportResultFile)
	}
	backlogRepo := backlogRepoRefForStage(root, repo)

	published := 0
	duplicates := 0
	for _, id := range prIDs {
		aggregate, ok := findCostAggregate(prCosts, id)
		if !ok {
			continue
		}
		result, err := providers.UpsertStickyComment(ctx, provider, repo,
			providers.StickyCommentTarget{Kind: providers.StickyCommentPullRequest, ID: id},
			costReportMarker, renderCostReport("PR", aggregate))
		if err != nil {
			return failProviderStage(stderr, "publish pull-request cost report", err, costReportResultFile)
		}
		published++
		duplicates += len(result.DuplicateIDs)
	}
	for _, id := range issueIDs {
		aggregate, ok := findCostAggregate(issueCosts, id)
		if !ok {
			continue
		}
		result, err := providers.UpsertStickyComment(ctx, provider, backlogRepo,
			providers.StickyCommentTarget{Kind: providers.StickyCommentIssue, ID: id},
			costReportMarker, renderCostReport("issue", aggregate))
		if err != nil {
			return failProviderStage(stderr, "publish issue cost report", err, costReportResultFile)
		}
		published++
		duplicates += len(result.DuplicateIDs)
	}

	if err := writeProviderStageResult(providerInput("resultFile", costReportResultFile), map[string]interface{}{
		"enabled": true, "published": published, "pullRequests": len(prIDs),
		"issues": len(issueIDs), "duplicateMarkers": duplicates,
	}); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "published %d sticky cost report(s)\n", published)
	return 0
}

func effectiveCostReporting(root, gaggleName string) (bool, error) {
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return false, err
	}
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return false, fmt.Errorf("%w (%v)", err, report)
	}
	var gaggle *apiv1.Gaggle
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggleName {
			gaggle = &set.Gaggles[i]
			break
		}
	}
	return instance.EffectiveCostEnabled(*config, gaggle), nil
}

func findCostAggregate(values []rollup.CostAggregate, id string) (rollup.CostAggregate, bool) {
	for _, value := range values {
		if value.ExternalID == id {
			return value, true
		}
	}
	return rollup.CostAggregate{}, false
}

func renderCostReport(subject string, aggregate rollup.CostAggregate) string {
	var b strings.Builder
	if subject == "PR" {
		fmt.Fprintf(&b, "Thanks for using Goobers! This PR cost **%s**.\n\n", costHeadline(aggregate))
	} else {
		fmt.Fprintf(&b, "Goobers has spent **%s** on this issue so far.\n\n", costHeadline(aggregate))
	}
	if aggregate.MeasuredRuns < aggregate.TotalRuns {
		fmt.Fprintf(&b, "Cost known for %d of %d runs -- totals are a lower bound.\n\n", aggregate.MeasuredRuns, aggregate.TotalRuns)
	}
	tokens := totalKnownTokens(aggregate.InputTokens, aggregate.OutputTokens)
	if aggregate.NanoAIU == nil && aggregate.CostUSD == nil && aggregate.CopilotPremiumRequests == nil {
		fmt.Fprintf(&b, "Cost is unavailable; known usage is %s tokens.\n\n", formatCount(tokens))
	}
	fmt.Fprintf(&b, "<details><summary>Cost breakdown -- %d runs, %s tokens</summary>\n\n", aggregate.TotalRuns, formatCount(tokens))
	b.WriteString("| Model | Attempts | Tokens (in/out/cached) | Cost |\n")
	b.WriteString("|---|---:|---:|---:|\n")
	if len(aggregate.Models) == 0 {
		fmt.Fprintf(&b, "| unavailable | %d | %s | %s |\n", aggregate.TotalAttempts,
			formatTokenTriplet(aggregate.InputTokens, aggregate.OutputTokens, aggregate.CacheReadTokens),
			costDetail(aggregate.NanoAIU, aggregate.CostUSD, aggregate.CopilotPremiumRequests, false))
	} else {
		models := append([]rollup.CostModelAggregate(nil), aggregate.Models...)
		sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })
		for _, model := range models {
			normalized := strings.HasPrefix(strings.ToLower(model.Model), "claude")
			fmt.Fprintf(&b, "| `%s` | %d | %s | %s |\n", model.Model, model.UsageAttempts,
				formatTokenTriplet(model.InputTokens, model.OutputTokens, model.CacheReadTokens),
				costDetail(model.NanoAIU, model.CostUSD, model.CopilotPremiumRequests, normalized))
		}
	}
	if hasClaudeModel(aggregate.Models) {
		b.WriteString("\n\\* Claude USD values are vendor-reported estimates; AI credits are normalized for totals.\n")
	}
	if costContainsString(aggregate.BillingModels, telemetry.BillingModelPremiumRequests) {
		b.WriteString("\nPremium requests are a legacy native unit and are not converted to AI credits.\n")
	}
	b.WriteString("</details>")
	return b.String()
}

func costHeadline(aggregate rollup.CostAggregate) string {
	if aggregate.NanoAIU != nil {
		credits := float64(*aggregate.NanoAIU) / 1e9
		text := formatDecimal(credits) + " AI credits"
		if aggregate.CostUSD != nil {
			text += " (~$" + formatDecimal(*aggregate.CostUSD) + ")"
		}
		if hasClaudeModel(aggregate.Models) {
			text += " (includes normalized estimates)"
		}
		return text
	}
	if aggregate.CostUSD != nil {
		return "$" + formatDecimal(*aggregate.CostUSD) + " estimated"
	}
	if aggregate.CopilotPremiumRequests != nil {
		return formatDecimal(*aggregate.CopilotPremiumRequests) + " premium requests"
	}
	return "unavailable"
}

func costDetail(nano *int64, usd, premium *float64, normalized bool) string {
	if nano != nil {
		value := formatDecimal(float64(*nano)/1e9) + " AIC"
		if usd != nil {
			value += " (~$" + formatDecimal(*usd) + ")"
		}
		if normalized {
			value += "*"
		}
		return value
	}
	if usd != nil {
		return "~$" + formatDecimal(*usd) + "*"
	}
	if premium != nil {
		return formatDecimal(*premium) + " premium requests"
	}
	return "unavailable"
}

func totalKnownTokens(input, output *int64) int64 {
	var total int64
	if input != nil {
		total += *input
	}
	if output != nil {
		total += *output
	}
	return total
}

func formatTokenTriplet(input, output, cached *int64) string {
	return formatOptionalCount(input) + " / " + formatOptionalCount(output) + " / " + formatOptionalCount(cached)
}

func formatOptionalCount(value *int64) string {
	if value == nil {
		return "unavailable"
	}
	return formatCount(*value)
}

func formatCount(value int64) string {
	switch {
	case value >= 1_000_000:
		return formatDecimal(float64(value)/1_000_000) + "M"
	case value >= 1_000:
		return formatDecimal(float64(value)/1_000) + "K"
	default:
		return strconv.FormatInt(value, 10)
	}
}

func formatDecimal(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64)
}

func hasClaudeModel(models []rollup.CostModelAggregate) bool {
	for _, model := range models {
		if strings.HasPrefix(strings.ToLower(model.Model), "claude") {
			return true
		}
	}
	return false
}

func costContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
