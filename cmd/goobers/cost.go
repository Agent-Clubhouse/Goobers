package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

const costHelp = "Usage: goobers cost [--pr=<id> | --issue=<id>] [--provider=<name>] [--window=<duration>] [--since=<RFC3339>] [--until=<RFC3339>] [--json] [--rebuild] [path]\n\n" +
	"Show exact recorded AI usage attributed to pull requests and issues. The\n" +
	"default window is 7d ending now; --since replaces --window, and every\n" +
	"query is capped at 90d. Native AI credits/USD and normalized estimates are\n" +
	"reported separately. Missing measurements make totals lower bounds.\n" +
	"Exit codes: 0 = OK, 2 = usage, query, or I/O error.\n"

type costReader interface {
	TelemetryCosts(context.Context, readservice.TelemetryCostRequest) (readservice.TelemetryCostResult, error)
}

type costReaderOpener func(string, bool) (costReader, io.Closer, error)

type costCommandOptions struct {
	root     string
	rebuild  bool
	json     bool
	provider string
	scope    string
	external string
	since    time.Time
	until    time.Time
}

func runCost(args []string, stdout, stderr io.Writer) int {
	return runCostAt(args, stdout, stderr, time.Now(), func(root string, rebuild bool) (costReader, io.Closer, error) {
		db, err := openRollup(instance.NewLayout(root), rebuild)
		if err != nil {
			return nil, nil, err
		}
		reader, err := readservice.NewTelemetry(db)
		if err != nil {
			_ = db.Close()
			return nil, nil, err
		}
		return reader, db, nil
	})
}

func runCostAt(args []string, stdout, stderr io.Writer, now time.Time, open costReaderOpener) int {
	options, ok := parseCostCommand(args, stderr, now)
	if !ok {
		return 2
	}
	reader, closer, err := open(options.root, options.rebuild)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	defer func() { _ = closer.Close() }()

	result, err := reader.TelemetryCosts(context.Background(), readservice.TelemetryCostRequest{
		Provider: options.provider, Scope: options.scope, ExternalID: options.external,
		Since: options.since, Until: options.until,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if options.json {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			pf(stderr, "error: encode cost report: %v\n", err)
			return 2
		}
		return 0
	}
	if err := writeCostReport(stdout, result); err != nil {
		pf(stderr, "error: write cost report: %v\n", err)
		return 2
	}
	return 0
}

func parseCostCommand(args []string, stderr io.Writer, now time.Time) (costCommandOptions, bool) {
	fs := newCLIFlagSet("cost", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pr := fs.String("pr", "", "show cost attributed to one pull request")
	issue := fs.String("issue", "", "show cost attributed to one issue")
	provider := fs.String("provider", "", "filter external references to one provider (for example github or ado)")
	window := fs.String("window", "7d", "bounded lookback duration (maximum 90d)")
	sinceValue := fs.String("since", "", "inclusive RFC3339 run-start lower bound; replaces --window")
	untilValue := fs.String("until", "", "exclusive RFC3339 run-start upper bound (default now)")
	jsonOutput := fs.Bool("json", false, "emit the shared aggregate contract as JSON")
	rebuild := fs.Bool("rebuild", false, "force a full telemetry rebuild before querying")
	fs.Usage = helpUsage(stderr, "cost")
	if err := fs.Parse(args); err != nil {
		return costCommandOptions{}, false
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return costCommandOptions{}, false
	}
	if strings.TrimSpace(*pr) != "" && strings.TrimSpace(*issue) != "" {
		pf(stderr, "error: --pr and --issue are mutually exclusive\n")
		return costCommandOptions{}, false
	}
	until := now.UTC()
	var err error
	if *untilValue != "" {
		until, err = time.Parse(time.RFC3339Nano, *untilValue)
		if err != nil {
			pf(stderr, "error: --until must be an RFC3339 timestamp\n")
			return costCommandOptions{}, false
		}
	}
	sinceSet := false
	windowSet := false
	fs.Visit(func(value *flag.Flag) {
		switch value.Name {
		case "since":
			sinceSet = true
		case "window":
			windowSet = true
		}
	})
	if sinceSet && windowSet {
		pf(stderr, "error: --since and --window are mutually exclusive\n")
		return costCommandOptions{}, false
	}
	var since time.Time
	if sinceSet {
		since, err = time.Parse(time.RFC3339Nano, *sinceValue)
		if err != nil {
			pf(stderr, "error: --since must be an RFC3339 timestamp\n")
			return costCommandOptions{}, false
		}
	} else {
		duration, parseErr := parseCostDuration(*window)
		if parseErr != nil {
			pf(stderr, "error: --window %v\n", parseErr)
			return costCommandOptions{}, false
		}
		since = until.Add(-duration)
	}

	scope := readservice.TelemetryCostScopeSummary
	external := ""
	if strings.TrimSpace(*pr) != "" {
		scope = readservice.TelemetryCostScopePullRequest
		external = strings.TrimSpace(*pr)
	} else if strings.TrimSpace(*issue) != "" {
		scope = readservice.TelemetryCostScopeIssue
		external = strings.TrimSpace(*issue)
	}
	options := costCommandOptions{
		root: ".", rebuild: *rebuild, json: *jsonOutput,
		provider: strings.TrimSpace(*provider), scope: scope, external: external,
		since: since.UTC(), until: until.UTC(),
	}
	if fs.NArg() == 1 {
		options.root = fs.Arg(0)
	}
	if !options.since.Before(options.until) {
		pf(stderr, "error: --since must be before --until\n")
		return costCommandOptions{}, false
	}
	if options.until.Sub(options.since) > readservice.MaxTelemetryCostWindow {
		pf(stderr, "error: cost window must not exceed 90d\n")
		return costCommandOptions{}, false
	}
	return options, true
}

func parseCostDuration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 32)
		if err != nil || days <= 0 || days > 90 {
			return 0, fmt.Errorf("must be a positive duration")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("must be a positive duration")
	}
	return duration, nil
}

func writeCostReport(output io.Writer, result readservice.TelemetryCostResult) error {
	var buffer bytes.Buffer
	fmt.Fprintf(&buffer, "COST ATTRIBUTION\nWindow: %s to %s\n", result.Since.Format(time.RFC3339), result.Until.Format(time.RFC3339))
	items := append([]readservice.TelemetryCostAggregate{}, result.PullRequests...)
	items = append(items, result.Issues...)
	if len(items) == 0 {
		buffer.WriteString("No attributed cost found.\n")
	} else {
		for _, item := range items {
			fmt.Fprintf(&buffer, "\n%s #%s (%s)\n", strings.ToUpper(item.ExternalKind), item.ExternalID, item.Provider)
			fmt.Fprintf(&buffer, "  Native: %s\n", formatCostAmounts(item.NativeTotals, "unmeasured"))
			fmt.Fprintf(&buffer, "  Normalized estimate: %s\n", formatCostAmounts(item.NormalizedTotals, "unavailable"))
			if item.Coverage.LowerBound {
				fmt.Fprintf(&buffer, "  Coverage: lower bound; %d/%d runs, %d/%d attempts measured\n",
					item.Coverage.MeasuredRuns, item.Coverage.TotalRuns,
					item.Coverage.MeasuredAttempts, item.Coverage.TotalAttempts)
			} else {
				fmt.Fprintf(&buffer, "  Coverage: complete; %d runs, %d attempts measured\n",
					item.Coverage.TotalRuns, item.Coverage.TotalAttempts)
			}
			for _, model := range item.Models {
				fmt.Fprintf(&buffer, "  Model %s: %s; %d/%d attempts measured\n",
					model.Model, formatCostAmounts(model.NativeTotals, "unmeasured"),
					model.MeasuredAttempts, model.UsageAttempts)
			}
			for _, run := range item.Runs {
				fmt.Fprintf(&buffer, "  Run %s (%s): %s; %d/%d attempts measured\n",
					run.RunID, run.StartedAt.Format(time.RFC3339),
					formatCostAmounts(run.NativeTotals, "unmeasured"),
					run.MeasuredAttempts, run.UsageAttempts)
			}
		}
	}
	_, err := io.Copy(output, &buffer)
	return err
}

func formatCostAmounts(amounts []readservice.TelemetryCostAmount, empty string) string {
	if len(amounts) == 0 {
		return empty
	}
	values := make([]string, 0, len(amounts))
	for _, amount := range amounts {
		var value string
		switch amount.Unit {
		case "usd":
			value = "$" + strconv.FormatFloat(amount.Value, 'f', 4, 64)
		case "aiCredits":
			value = strconv.FormatFloat(amount.Value, 'f', 4, 64) + " AI credits"
		case "premiumRequests":
			value = strconv.FormatFloat(amount.Value, 'f', 4, 64) + " premium requests"
		default:
			value = strconv.FormatFloat(amount.Value, 'f', 4, 64) + " " + amount.Unit
		}
		if amount.Estimated {
			value += " estimated"
		}
		values = append(values, value)
	}
	return strings.Join(values, ", ")
}
